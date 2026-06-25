package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	cft "github.com/geektr-cloud/go-cloudflare-fastest-transport"
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

	// Routers (gorilla/mux, SkipClean preserves raw paths like OCI digests)
	Router     *mux.Router // HTTPS (all requests)
	HTTPRouter *mux.Router // HTTP (keepHTTP + redirect)

	mirrors      atomic.Pointer[[]Mirror]
	allowedHosts atomic.Pointer[map[string]bool]
	keepHTTP     atomic.Pointer[map[string]bool]
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

	// Upstream transport: proxy > disguise > CF fastest-node.
	// When a proxy is configured, skip the CF node optimizer entirely.
	var transport http.RoundTripper
	if px := config.GetUpstreamProxy(); px != "" {
		t, err := buildProxyTransport(px)
		if err != nil {
			return nil, fmt.Errorf("proxy transport: %w", err)
		}
		transport = t
		log.Printf("upstream via proxy %s", px)
	} else if dp := config.GetUpstreamDisguisePort(); dp != 0 {
		transport = buildDisguiseTransport(u.Hostname(), dp)
	} else {
		t, err := buildTransport(config.GetPoolSize())
		if err != nil {
			return nil, fmt.Errorf("cf transport: %w", err)
		}
		transport = t
	}

	// No Client.Timeout: it caps the WHOLE request including body read, which
	// kills large-file streaming (>30s downloads get cut mid-stream). Phase
	// timeouts (connect / TLS / response-header) live on the transports instead.
	httpClient := &http.Client{Transport: transport}

	s := &Server{
		upstream:    u,
		client:      httpClient,
		resty:       resty.NewWithClient(httpClient).SetBaseURL(config.GetUpstreamURL()),
		baseDomains: config.AllDomainNames(),
	}

	s.buildRouters()

	if err := s.refreshMirrors(); err != nil {
		return nil, fmt.Errorf("initial mirror fetch: %w", err)
	}

	go s.watchMirrors(10 * time.Minute)
	return s, nil
}

// buildRouters sets up the mux routers (like old relay/server.go).
func (s *Server) buildRouters() {
	// --- HTTPS router (serves all traffic) ---
	router := mux.NewRouter().SkipClean(true)

	// Internal relay endpoints
	relayRouter := router.PathPrefix("/.mirror-relay").Subrouter()
	g := &guard.Guard{SecretKey: config.GetSecretKey()}
	storage := GetCertMagicStorage()
	relayRouter.Handle("/cert-pack", g.Wrap(certsrv.New(storage)))

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

// handleRequest is the main proxy handler — determines relay path and proxies.
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	// Relay middleware: strip /@relay/ prefix from incoming requests (sent by lower relays).
	s.stripRelayPrefix(r)

	host := requestHost(r)

	// Filter: subdomain must be a known mirror host
	if sub, _ := s.matchBaseDomain(host); sub != "" {
		hosts := s.allowedHosts.Load()
		if hosts == nil || !(*hosts)[sub] {
			http.Error(w, "unknown mirror host", http.StatusForbidden)
			return
		}
	}

	relayPath := s.resolveRelayPath(host, r.URL.Path)
	s.proxy(w, r, relayPath)
}

// stripRelayPrefix rewrites the request in-place if its path starts with /@relay/.
//   - /@relay/@<sub>/<path> → Host = <sub>.<base_domain>, path = /<path>
//   - /@relay/<path>        → strip prefix, keep host
func (s *Server) stripRelayPrefix(r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/@relay/") {
		return
	}
	rest := path[len("/@relay/"):]

	if strings.HasPrefix(rest, "@") {
		// /@relay/@<sub>/<path>
		sub, subPath, _ := strings.Cut(rest[1:], "/")
		if sub != "" && len(s.baseDomains) > 0 {
			r.Host = sub + "." + s.baseDomains[0]
			r.URL.Path = "/" + subPath
		}
	} else {
		// /@relay/<path>
		r.URL.Path = "/" + rest
	}
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

// resolveRelayPath determines the /@relay/... path for this request.
func (s *Server) resolveRelayPath(host, path string) string {
	if sub, _ := s.matchBaseDomain(host); sub != "" {
		return "/@relay/@" + sub + path
	}
	return "/@relay" + path
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, relayPath string) {
	upstreamURL := *s.upstream
	upstreamURL.Path = relayPath
	upstreamURL.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	copyHeaders(req.Header, r.Header)
	req.Header.Set("Host", s.upstream.Host)

	// Only set X-MDB-Relay-Host if not already present (a lower relay may have set it).
	if r.Header.Get("X-MDB-Relay-Host") == "" {
		host := requestHost(r)
		if _, base := s.matchBaseDomain(host); base != "" {
			req.Header.Set("X-MDB-Relay-Host", base)
		}
	}

	resp, err := s.client.Do(req)
	if err != nil {
		log.Printf("upstream error: %v", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// refreshMirrors fetches /api/mirrors from upstream.
func (s *Server) refreshMirrors() error {
	var result mirrorsResponse
	resp, err := s.resty.R().SetResult(&result).Get("/api/mirrors")
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
	log.Printf("loaded %d mirrors (%d hosts) from upstream", len(mirrors), len(hosts))
	return nil
}

func (s *Server) watchMirrors(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
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

func httpRedirectHandler(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	target := "https://" + host + r.URL.RequestURI()
	w.Header().Set("Connection", "close")
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

func hostOnly(hostport string) string {
	if idx := strings.LastIndex(hostport, ":"); idx != -1 {
		return hostport[:idx]
	}
	return hostport
}

func requestHost(r *http.Request) string {
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return hostOnly(host)
}

// buildTransport creates an http.RoundTripper using CF fastest-node discovery.
func buildTransport(poolSize int) (http.RoundTripper, error) {
	mgr := cft.NewPoolManagerWithFile(poolSize, config.GetCfNodesFile())
	mgr.RefreshPool() // skips if pool already >61.8% full (i.e. file had enough nodes)

	transport := mgr.Transport(cft.TransportOptions{
		UpstreamCount:  3,
		EliminateDelay: 5 * time.Second,
	})

	return transport, nil
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
	addr := fmt.Sprintf("%s:%d", host, port)
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

var hopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailers":            true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if hopHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}
