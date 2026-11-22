package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

func openTestStore(t *testing.T) (*hashstore.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := hashstore.Open(hashstore.Config{Dir: dir, Capacity: 1 << 20})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s, dir
}

// newHashServer builds a real router (so registration order is exercised) with an
// upstream that records how often the proxy catch-all is reached.
func newHashServer(t *testing.T, store *hashstore.Store) (*Server, *int) {
	t.Helper()
	// buildRouters asks config for the JWT secret; keep it out of /etc.
	t.Setenv("RELAY_CONFIG_DIR", t.TempDir())

	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = io.WriteString(w, "upstream")
	}))
	t.Cleanup(up.Close)

	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		upstream:    u,
		client:      &http.Client{},
		baseDomains: []string{"relay.example.com"},
		hashStore:   store,
	}
	s.proxy = s.newReverseProxy()
	s.buildRouters()
	return s, &hits
}

func serveRelay(s *Server, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "https://relay.example.com"+target, nil)
	req.Host = "relay.example.com"
	rec := httptest.NewRecorder()
	s.Router.ServeHTTP(rec, req)
	return rec
}

func TestHashContentRoutes(t *testing.T) {
	store, _ := openTestStore(t)
	const payload = "cached payload"
	c, err := store.Put(context.Background(), strings.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	s, hits := newHashServer(t, store)
	// The routes must not depend on a manifest snapshot.
	if s.ready.Load() {
		t.Fatal("test precondition: server should not be manifest-ready")
	}

	cases := []struct {
		name       string
		method     string
		target     string
		wantStatus int
		wantBody   string
	}{
		{"sha256", http.MethodGet, "/hash-content/sha256/" + c.SHA256, http.StatusOK, payload},
		{"sha1", http.MethodGet, "/hash-content/sha1/" + c.SHA1, http.StatusOK, payload},
		{"uppercase hex normalised", http.MethodGet, "/hash-content/sha1/" + strings.ToUpper(c.SHA1), http.StatusOK, payload},
		{"head", http.MethodHead, "/hash-content/sha256/" + c.SHA256, http.StatusOK, ""},
		{"unknown digest", http.MethodGet, "/hash-content/sha256/" + strings.Repeat("0", 64), http.StatusNotFound, ""},
		{"unknown algorithm", http.MethodGet, "/hash-content/md5/" + c.SHA1, http.StatusBadRequest, ""},
		{"short digest", http.MethodGet, "/hash-content/sha1/" + c.SHA1[:39], http.StatusBadRequest, ""},
		{"non-hex digest", http.MethodGet, "/hash-content/sha1/" + strings.Repeat("z", 40), http.StatusBadRequest, ""},
		{"missing segment", http.MethodGet, "/hash-content/sha256", http.StatusBadRequest, ""},
		{"bare namespace", http.MethodGet, "/hash-content", http.StatusBadRequest, ""},
		{"extra segment", http.MethodGet, "/hash-content/sha256/" + c.SHA256 + "/x", http.StatusBadRequest, ""},
		{"post", http.MethodPost, "/hash-content/sha256/" + c.SHA256, http.StatusMethodNotAllowed, ""},
		{"put", http.MethodPut, "/hash-content/sha1/" + c.SHA1, http.StatusMethodNotAllowed, ""},
		{"delete", http.MethodDelete, "/hash-content/sha256/" + c.SHA256, http.StatusMethodNotAllowed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveRelay(s, tc.method, tc.target)
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s %s: status %d, want %d (body %q)", tc.method, tc.target, rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusOK && tc.method == http.MethodGet {
				if got := rec.Body.String(); got != tc.wantBody {
					t.Fatalf("body = %q, want %q", got, tc.wantBody)
				}
			}
			if tc.method == http.MethodHead {
				if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(payload)) {
					t.Fatalf("HEAD Content-Length = %q, want %d", got, len(payload))
				}
			}
			if tc.method != http.MethodGet && tc.method != http.MethodHead {
				if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
					t.Errorf("Allow = %q", got)
				}
			}
		})
	}

	if *hits != 0 {
		t.Fatalf("hash-content traffic reached the upstream %d times", *hits)
	}
}

// A storage error that is not a miss is a 502, not a 404: the index is intact.
func TestHashContentTransientErrorIs502(t *testing.T) {
	store, dir := openTestStore(t)
	const payload = "unreadable soon"
	c, err := store.Put(context.Background(), strings.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	// Replace the body with a self-referential symlink: it is still a body entry
	// (so background reconciliation keeps it) but opening it fails with ELOOP —
	// an unreadable body, not an absent one.
	body := filepath.Join(dir, "data", c.SHA256[0:2], c.SHA256[2:4], c.SHA256)
	if err := os.Remove(body); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(body, body); err != nil {
		t.Fatal(err)
	}

	s, _ := newHashServer(t, store)
	if rec := serveRelay(s, http.MethodGet, "/hash-content/sha256/"+c.SHA256); rec.Code != http.StatusBadGateway {
		t.Fatalf("GET status = %d, want 502 (body %q)", rec.Code, rec.Body.String())
	}
	if rec := serveRelay(s, http.MethodHead, "/hash-content/sha256/"+c.SHA256); rec.Code != http.StatusBadGateway {
		t.Fatalf("HEAD status = %d, want 502", rec.Code)
	}
}

// Without CACHE_DIR the route is not registered and the request proxies as
// before.
func TestHashContentDisabledFallsThroughToProxy(t *testing.T) {
	s, hits := newHashServer(t, nil)
	s.ready.Store(true)

	rec := serveRelay(s, http.MethodGet, "/hash-content/sha256/"+strings.Repeat("a", 64))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the proxied 200", rec.Code)
	}
	if got := rec.Body.String(); got != "upstream" {
		t.Fatalf("body = %q, want the upstream response", got)
	}
	if *hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", *hits)
	}
}
