package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, upstream string) *Server {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	s := &Server{
		upstream:    u,
		client:      &http.Client{},
		baseDomains: []string{"relay.example.com", "relay-alt.example.org"},
	}
	s.ready.Store(true)
	return s
}

// h2c-ish case: an Upgrade request must survive the hop-by-hop stripping.
func TestProxyPassesUpgradeHeaders(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	s := newTestServer(t, up.URL)
	s.proxy = s.newReverseProxy()

	req := httptest.NewRequest(http.MethodGet, "http://relay.example.com/charts/x", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	s.proxy.ServeHTTP(httptest.NewRecorder(), req)

	if got.Get("Upgrade") != "websocket" {
		t.Fatalf("Upgrade header dropped: %v", got)
	}
}

// Tokens named in Connection must be stripped, not just the well-known list.
func TestProxyStripsConnectionTokens(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Connection", "X-Leaky")
		w.Header().Set("X-Leaky", "secret")
	}))
	defer up.Close()

	s := newTestServer(t, up.URL)
	s.proxy = s.newReverseProxy()

	req := httptest.NewRequest(http.MethodGet, "http://relay.example.com/npm/pkg", nil)
	req.Header.Set("Connection", "X-Drop-Me")
	req.Header.Set("X-Drop-Me", "1")

	rec := httptest.NewRecorder()
	s.proxy.ServeHTTP(rec, req)

	if got.Get("X-Drop-Me") != "" {
		t.Errorf("request header named in Connection was forwarded")
	}
	if rec.Header().Get("X-Leaky") != "" {
		t.Errorf("response header named in Connection was forwarded")
	}
}

// The relay must not re-encode escape sequences on the way through; OCI tags
// and digests rely on it.
func TestProxyPreservesRawPath(t *testing.T) {
	var gotURI string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	s := newTestServer(t, up.URL)
	s.proxy = s.newReverseProxy()

	// A non-empty base path on the upstream must not double up either.
	s.upstream.Path = ""

	raw := "/v2/library/nginx/manifests/sha256%3Aabc%2Fdef?ns=a%2Fb"
	req := httptest.NewRequest(http.MethodGet, "http://relay.example.com"+raw, nil)

	s.proxy.ServeHTTP(httptest.NewRecorder(), req)

	if gotURI != raw {
		t.Fatalf("request URI mutated:\n got %q\nwant %q", gotURI, raw)
	}
}

// An upstream configured with a base path keeps it, and the inbound path is
// appended once — not twice.
func TestProxyUpstreamBasePath(t *testing.T) {
	var gotURI string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
	}))
	defer up.Close()

	s := newTestServer(t, up.URL+"/base")
	s.proxy = s.newReverseProxy()

	s.proxy.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://relay.example.com/a/b", nil))

	if gotURI != "/base/a/b" {
		t.Fatalf("RequestURI = %q, want /base/a/b", gotURI)
	}
}

// X-MDB-Relay-Host is the only channel carrying the user-visible origin, so it
// must pass through untouched and only be filled in when absent.
//
// The proxy rewrites Host to the upstream host (dcr.<upstream>), which does not
// resolve in tests, so assert on the outbound request via Rewrite rather than
// on a live server: Rewrite is where the decision is made.
func TestProxyRelayHostHeader(t *testing.T) {
	s := newTestServer(t, "https://mirs.uk")

	rewrite := func(host, clientValue string) http.Header {
		in := httptest.NewRequest(http.MethodGet, "http://"+host+"/v2/", nil)
		if clientValue != "" {
			in.Header.Set(RelayHostHeader, clientValue)
		}
		out := in.Clone(context.Background())
		s.rewriteUpstream(&httputil.ProxyRequest{In: in, Out: out})
		return out.Header
	}

	// Client-provided value wins (open project: anybody may proxy).
	if got := rewrite("dcr.relay.example.com", "user.example.net").Get(RelayHostHeader); got != "user.example.net" {
		t.Errorf("client header overwritten: %q", got)
	}

	// Absent → filled with this relay's base domain.
	if got := rewrite("dcr.relay.example.com", "").Get(RelayHostHeader); got != "relay.example.com" {
		t.Errorf("missing header not filled: %q", got)
	}
	if got := rewrite("relay-alt.example.org", "").Get(RelayHostHeader); got != "relay-alt.example.org" {
		t.Errorf("wrong base domain chosen: %q", got)
	}

	// Foreign host → no header at all (bare passthrough must not invent one).
	if got := rewrite("evil.example.net", "").Get(RelayHostHeader); got != "" {
		t.Errorf("header set for foreign host: %q", got)
	}
}

// The outbound Host must be the upstream subdomain, not the relay's own.
func TestRewriteTargetHost(t *testing.T) {
	s := newTestServer(t, "https://mirs.uk")
	cases := []struct{ in, wantHost string }{
		{"http://relay.example.com/", "mirs.uk"},
		{"http://dcr.relay.example.com/v2/", "dcr.mirs.uk"},
		{"http://relay-alt.example.org/x", "mirs.uk"},
		{"http://unknown.example.net/x", "mirs.uk"},
	}
	for _, c := range cases {
		in := httptest.NewRequest(http.MethodGet, c.in, nil)
		out := in.Clone(context.Background())
		s.rewriteUpstream(&httputil.ProxyRequest{In: in, Out: out})
		if out.URL.Host != c.wantHost {
			t.Errorf("%s → url.Host %q, want %q", c.in, out.URL.Host, c.wantHost)
		}
		if out.URL.Path != in.URL.Path {
			t.Errorf("%s → path %q, want %q", c.in, out.URL.Path, in.URL.Path)
		}
	}
}

func TestProxyRewritesHost(t *testing.T) {
	// Portable: assert on the outbound request instead of dialing a hostname
	// that only resolves when the real upstream exists.
	s := newTestServer(t, "https://mirs.uk")

	cases := []struct{ in, wantHost string }{
		{"http://relay.example.com/", "mirs.uk"},
		{"http://dcr.relay.example.com/v2/", "dcr.mirs.uk"},
		{"http://relay-alt.example.org/x", "mirs.uk"},
		{"http://unknown.example.net/x", "mirs.uk"},
	}
	for _, c := range cases {
		in := httptest.NewRequest(http.MethodGet, c.in, nil)
		out := in.Clone(context.Background())
		s.rewriteUpstream(&httputil.ProxyRequest{In: in, Out: out})
		if got := out.URL.Host; got != c.wantHost {
			t.Errorf("%s → url.Host %q, want %q", c.in, got, c.wantHost)
		}
	}
}

func TestProxyUpstreamError(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	s.proxy = s.newReverseProxy()

	rec := httptest.NewRecorder()
	s.proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://relay.example.com/", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rec.Code)
	}
}

func TestProxyBodyPassthrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Write(append([]byte("echo:"), b...))
	}))
	defer up.Close()

	s := newTestServer(t, up.URL)
	s.proxy = s.newReverseProxy()

	req := httptest.NewRequest(http.MethodPost, "http://relay.example.com/npm/pkg", strings.NewReader("hello"))
	rec := httptest.NewRecorder()
	s.proxy.ServeHTTP(rec, req)

	if rec.Body.String() != "echo:hello" {
		t.Fatalf("body %q", rec.Body.String())
	}
}

func TestHandleRequestNotReady(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	s.ready.Store(false)

	rec := httptest.NewRecorder()
	s.handleRequest(rec, httptest.NewRequest(http.MethodGet, "http://relay.example.com/", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 before first manifest fetch", rec.Code)
	}
}
