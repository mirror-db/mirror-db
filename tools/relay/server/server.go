package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	"github.com/mirror-db/mirror-db/tools/relay/certsrv"
	"github.com/mirror-db/mirror-db/tools/relay/config"
	"github.com/mirror-db/mirror-db/tools/relay/guard"
	"golang.org/x/net/proxy"
	"resty.dev/v3"
)

// Mirror matches the JSON shape from GET /api/mirrors.
type Mirror struct {
	Name     string `json:"name"`
	Host     string `json:"host,omitempty"`
	Path     string `json:"path,omitempty"`
	KeepHTTP bool   `json:"keepHTTP,omitempty"`
}

// Server holds state and exposes the HTTPS router + HTTP router.
type Server struct {
	upstream    *url.URL
	client      *http.Client
	resty       *resty.Client
	baseDomains []string
	proxy       *httputil.ReverseProxy

	// Routers (gorilla/mux, SkipClean preserves raw paths like OCI digests)
	Router     *mux.Router // HTTPS (all requests)
	HTTPRouter *mux.Router // HTTP (keepHTTP + redirect)

	edge *edgeSelection

	mirrors      atomic.Pointer[[]Mirror]
	loaded       atomic.Bool // manifest has been fetched at least once
	allowedHosts atomic.Pointer[map[string]bool]
	keepHTTP     atomic.Pointer[map[string]bool]

	// ready is closed after the first successful manifest fetch. Until then the
	// proxy answers 502 instead of an empty allow-list.
	ready atomic.Bool
}

// mirrorsResponse matches the JSON shape from GET /api/mirrors.
type mirrorsResponse struct {
	Mirrors []Mirror `json:"mirrors"`
}

// NewServer creates a relay server. Discovers CF edge nodes, fetches mirror
// manifest from upstream, and builds routers.
func NewServer() (*Server, error) {
	u, err := url.Parse(config.GetUpstreamURL())
	if err != nil {
		return nil, fmt.Errorf("parse upstream: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("upstream url %q needs scheme and host", u.String())
	}

	// Upstream transport: proxy > disguise > edge selection.
	//
	// The first two are explicit routing decisions and bypass edge selection
	// entirely: when the operator says "go through this proxy" or "use this
	// disguised port", picking a Cloudflare IP would override their intent.
	var (
		transport http.RoundTripper
		edge      *edgeSelection
	)
	switch {
	case config.GetUpstreamProxy() != "":
		t, err := buildProxyTransport(config.GetUpstreamProxy())
		if err != nil {
			return nil, fmt.Errorf("proxy transport: %w", err)
		}
		transport = t
		log.Printf("upstream via proxy %s", config.GetUpstreamProxy())
	case config.GetUpstreamDisguisePort() != 0:
		transport = buildDisguiseTransport(u.Hostname(), config.GetUpstreamDisguisePort())
		log.Printf("upstream via SSH disguise :%d", config.GetUpstreamDisguisePort())
	default:
		edge, transport = newEdgeSelection()
	}

	// No Client.Timeout: it caps the WHOLE request including body read, which
	// kills large-file streaming (>30s downloads get cut mid-stream). Phase
	// timeouts (connect / TLS / response-header) live on the transports instead.
	httpClient := &http.Client{Transport: transport}

	s := &Server{
		upstream:    u,
		client:      httpClient,
		edge:        edge,
		resty:       resty.NewWithClient(httpClient).SetBaseURL(config.GetUpstreamURL()),
		baseDomains: config.AllDomainNames(),
	}
	if len(s.baseDomains) == 0 {
		return nil, fmt.Errorf("no relay.base_domains configured")
	}

	s.proxy = s.newReverseProxy()

	s.buildRouters()

	// A transient upstream failure must not turn into a systemd restart loop:
	// log it, serve 502 until the watcher succeeds.
	if err := s.refreshMirrors(); err != nil {
		log.Printf("initial mirror fetch failed (serve 502 until it succeeds): %v", err)
	}

	go s.watchMirrors(10 * time.Minute)
	return s, nil
}

// buildRouters sets up the mux routers.
func (s *Server) buildRouters() {
	// --- HTTPS router (serves all traffic) ---
	router := mux.NewRouter().SkipClean(true)

	// Internal relay endpoints
	relayRouter := router.PathPrefix("/.mirror-relay").Subrouter()
	g := &guard.Guard{SecretKey: config.GetSecretKey()}
	relayRouter.Handle("/cert-pack", g.Wrap(certsrv.New(CertMagicStorage())))
	relayRouter.Handle("/mirrors", g.Wrap(http.HandlerFunc(s.handleMirrors)))
	relayRouter.Handle("/pool", g.Wrap(http.HandlerFunc(s.handlePool)))

	// Speedtest endpoint
	router.HandleFunc("/speedtest/{mb}", s.handleSpeedtest)

	// All other requests → proxy
	router.PathPrefix("/").HandlerFunc(s.handleRequest)

	s.Router = router

	// --- HTTP router (keepHTTP mirrors served directly, others redirected) ---
	httpRouter := mux.NewRouter().SkipClean(true)
	httpRouter.PathPrefix("/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.ShouldKeepHTTP(r) {
			s.handleRequest(w, r)
		} else {
			httpRedirectHandler(w, r)
		}
	})

	s.HTTPRouter = httpRouter
}

// handleRequest is the main proxy handler — replaces host and proxies.
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		http.Error(w, "relay has no upstream manifest yet", http.StatusBadGateway)
		return
	}
	s.proxy.ServeHTTP(w, r)
}

// newReverseProxy wires httputil.ReverseProxy. Using the stdlib proxy gives us
// RFC 9110 hop-by-hop handling (including tokens named in Connection), correct
// Trailer / 1xx handling and X-Forwarded-For for free.
func (s *Server) newReverseProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite:       s.rewriteUpstream,
		Transport:     s.client.Transport,
		FlushInterval: 100 * time.Millisecond,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Client cancellations are normal; don't spam the log.
			if r.Context().Err() != nil {
				return
			}
			log.Printf("upstream error for %s %s: %v", r.Method, r.URL.Redacted(), err)
			http.Error(w, "upstream error", http.StatusBadGateway)
		},
	}
}

// rewriteUpstream maps a client request onto the upstream origin, preserving
// the exact request URI (fresh: URL escape sequences, `//`, `..` are not
// re-encoded) and passing X-MDB-Relay-Host through untouched.
func (s *Server) rewriteUpstream(pr *httputil.ProxyRequest) {
	in, out := pr.In, pr.Out

	basePath, baseRawPath := s.upstream.Path, s.upstream.RawPath
	if basePath == "" || basePath == "/" {
		basePath, baseRawPath = "", ""
	}

	outURL := *s.upstream
	outURL.Host = s.upstreamHostFor(requestHost(in))
	outURL.Path, outURL.RawPath, outURL.RawQuery = "", "", ""

	// SetXForwarded first: it only touches headers.
	pr.SetXForwarded()

	// X-MDB-Relay-Host: trust the client's value (open project — anyone may
	// proxy) and only fill it in when absent, so a multi-level chain keeps the
	// origin closest to the user.
	if in.Header.Get(RelayHostHeader) == "" {
		if _, base := s.matchBaseDomain(requestHost(in)); base != "" {
			out.Header.Set(RelayHostHeader, base)
		}
	}

	// Out.URL.Path must not be touched before this: SetURL appends the inbound
	// path onto the target.
	pr.SetURL(&outURL)

	// Restore the upstream's base path: SetURL overwrote it.
	if basePath != "" {
		out.URL.Path = strings.TrimSuffix(basePath, "/") + in.URL.Path
		if baseRawPath != "" {
			out.URL.RawPath = strings.TrimSuffix(baseRawPath, "/") + in.URL.EscapedPath()
		}
	}
	out.Host = outURL.Host
}

// RelayHostHeader is the header the Worker reads to build absolute URLs that
// point back through the relay (see server/relay.ts).
const RelayHostHeader = "X-MDB-Relay-Host"

// upstreamHostFor maps a relay-facing host onto the upstream host:
//
//	dcr.relay.example.com → dcr.<upstream>
//	relay.example.com     → <upstream>
//	anything else         → <upstream>  (bare passthrough)
func (s *Server) upstreamHostFor(host string) string {
	sub, base := s.matchBaseDomain(host)
	if sub != "" && base != "" {
		return sub + "." + s.upstream.Hostname()
	}
	return s.upstream.Host
}

// matchBaseDomain checks if host is a subdomain of any configured base domain.
func (s *Server) matchBaseDomain(host string) (sub string, base string) {
	for _, bd := range s.baseDomains {
		if host == bd {
			return "", bd
		}
		if strings.HasSuffix(host, "."+bd) {
			return strings.TrimSuffix(host, "."+bd), bd
		}
	}
	return "", ""
}

// refreshMirrors fetches /api/mirrors from upstream.
func (s *Server) refreshMirrors() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result mirrorsResponse
	resp, err := s.resty.R().SetContext(ctx).SetResult(&result).Get("/api/mirrors")
	if err != nil {
		return err
	}
	if resp.IsStatusFailure() {
		return fmt.Errorf("upstream returned %s", resp.Status())
	}

	mirrors := result.Mirrors

	hosts := make(map[string]bool, len(mirrors))
	httpOK := make(map[string]bool)
	for _, m := range mirrors {
		if m.Host != "" {
			hosts[m.Host] = true
			if m.KeepHTTP {
				httpOK[m.Host] = true
			}
		}
		if m.Path != "" && m.KeepHTTP {
			httpOK[m.Path] = true
		}
	}

	s.mirrors.Store(&mirrors)
	s.allowedHosts.Store(&hosts)
	s.keepHTTP.Store(&httpOK)
	s.ready.Store(true)
	log.Printf("loaded %d mirrors (%d hosts) from upstream", len(mirrors), len(hosts))
	s.loaded.Store(true)
	return nil
}

// Mirrors returns the current manifest as JSON, for operators debugging
// upstream discovery. Served at /.mirror-relay/mirrors.
func (s *Server) Mirrors() ([]Mirror, error) {
	p := s.mirrors.Load()
	if p == nil {
		return nil, nil
	}
	return *p, nil
}

// handlePool reports what the edge-selection engine currently believes, so an
// operator can answer "why is traffic going to that IP" without guessing.
func (s *Server) handlePool(w http.ResponseWriter, r *http.Request) {
	snap := s.edge.Snapshot()
	if snap == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "edge selection is not running (configured proxy or disguise)",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap)
}

// Close stops background work. The edge engine owns goroutines that would
// otherwise keep probing a network nobody is serving from.
func (s *Server) Close() {
	if s.edge != nil {
		s.edge.Close()
	}
}

func (s *Server) handleMirrors(w http.ResponseWriter, r *http.Request) {
	m, err := s.Mirrors()
	if err != nil {
		http.Error(w, "no manifest", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mirrorsResponse{Mirrors: m})
}

func (s *Server) watchMirrors(interval time.Duration) {
	for {
		// Until the manifest has loaded once, this relay has no routes at all:
		// every request is a 502. Retry on a short timer rather than waiting a
		// full interval, which is what turned one unlucky edge address at
		// startup into ten minutes of downtime.
		wait := interval
		if !s.loaded.Load() {
			wait = 15 * time.Second
		}

		// Jitter each cycle so a fleet of relays doesn't stampede the Worker.
		next := wait + time.Duration(rand.Int64N(int64(wait/3))) - wait/6
		time.Sleep(next)
		if err := s.refreshMirrors(); err != nil {
			log.Printf("mirror refresh failed: %v", err)
		}
	}
}

// ShouldKeepHTTP returns true if the request targets a mirror that allows plain HTTP.
func (s *Server) ShouldKeepHTTP(r *http.Request) bool {
	httpOK := s.keepHTTP.Load()
	if httpOK == nil {
		return false
	}

	host := requestHost(r)
	if sub, _ := s.matchBaseDomain(host); sub != "" {
		return (*httpOK)[sub]
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if seg, _, _ := strings.Cut(path, "/"); seg != "" {
		return (*httpOK)[seg]
	}
	return false
}

// httpRedirectHandler upgrades plain HTTP to HTTPS. 308 preserves the method
// and body across the redirect; 301 would silently turn a POST into a GET.
func httpRedirectHandler(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	target := "https://" + host + r.URL.RequestURI()
	w.Header().Set("Connection", "close")
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}

func hostOnly(hostport string) string {
	if idx := strings.LastIndex(hostport, ":"); idx != -1 {
		return hostport[:idx]
	}
	return hostport
}

// requestHost is the host the client addressed us on. X-Forwarded-Host is
// trusted unconditionally: the relay is an open proxy by design.
func requestHost(r *http.Request) string {
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		if i := strings.IndexByte(h, ','); i >= 0 {
			h = h[:i]
		}
		host = strings.TrimSpace(h)
	}
	return strings.ToLower(strings.TrimSuffix(hostOnly(host), "."))
}

// buildProxyTransport creates an http.RoundTripper that routes all upstream
// traffic through an http/https or socks5 proxy. When set, the CF fastest-node
// optimizer is bypassed entirely.
//
// Supported schemes: http, https, socks5, socks5h.
func buildProxyTransport(rawURL string) (http.RoundTripper, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url: %w", err)
	}

	t := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		t.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		dialer, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 dialer: %w", err)
		}
		if cd, ok := dialer.(proxy.ContextDialer); ok {
			t.DialContext = cd.DialContext
		} else {
			t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (want http/https/socks5)", u.Scheme)
	}

	return t, nil
}

// buildDisguiseTransport creates an http.RoundTripper that dials upstream via
// SSH-banner-disguised TLS on the given port.
func buildDisguiseTransport(host string, port int) http.RoundTripper {
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	return &http.Transport{
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return disguiseDialTLS(ctx, addr, &tls.Config{ServerName: host})
		},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
}
