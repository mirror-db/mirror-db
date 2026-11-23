package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// testClock is a manually advanced clock: no test waits for wall time.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// recordingUpstream counts requests and keeps each request's headers.
type recordingUpstream struct {
	mu      sync.Mutex
	hits    int
	headers []http.Header
	handler http.HandlerFunc
}

func (u *recordingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.hits++
	u.headers = append(u.headers, r.Header.Clone())
	u.mu.Unlock()
	u.handler(w, r)
}

func (u *recordingUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits
}

func (u *recordingUpstream) lastHeader() http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.headers) == 0 {
		return nil
	}
	return u.headers[len(u.headers)-1]
}

func startUpstream(t *testing.T, handler http.HandlerFunc) (*recordingUpstream, *httptest.Server) {
	t.Helper()
	rec := &recordingUpstream{handler: handler}
	up := httptest.NewServer(rec)
	t.Cleanup(up.Close)
	return rec, up
}

type cacheServerOptions struct {
	dir      string
	clock    *testClock
	maxSize  int64
	mirrors  []Mirror
	policies []Policy
}

// newCacheServer builds a Server wired to a content store and a test upstream.
// It deliberately does not call NewServer: no manifest fetch, no edge engine.
func newCacheServer(t *testing.T, upstream string, opts cacheServerOptions) (*Server, *hashstore.Store) {
	t.Helper()
	if opts.dir == "" {
		opts.dir = t.TempDir()
	}
	if opts.clock == nil {
		opts.clock = newTestClock()
	}
	if opts.maxSize == 0 {
		opts.maxSize = 8 << 20
	}
	store, err := hashstore.Open(hashstore.Config{
		Dir:      opts.dir,
		Capacity: 64 << 20,
		MaxSize:  opts.maxSize,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	st, err := newManifestState(mirrorsResponse{Mirrors: opts.mirrors, Policies: opts.policies}, `"test"`)
	if err != nil {
		t.Fatalf("manifest state: %v", err)
	}
	s := &Server{
		upstream:    mustURL(t, upstream),
		client:      &http.Client{},
		baseDomains: []string{"relay.example.com"},
		hashStore:   store,
		now:         opts.clock.Now,
	}
	s.manifest.Store(st)
	s.ready.Store(true)
	s.proxy = s.newReverseProxy()
	return s, store
}

func cacheRequest(s *Server, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://relay.example.com"+target, nil)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	s.handleRequest(rec, req)
	return rec
}

func cacheRequestHost(s *Server, method, host, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://"+host+target, nil)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	s.handleRequest(rec, req)
	return rec
}

func writeResponse(w http.ResponseWriter, r *http.Request, status int, body string, headers map[string]string) {
	for name, value := range headers {
		w.Header().Set(name, value)
	}
	if w.Header().Get("Content-Length") == "" && status != http.StatusNotModified && status != http.StatusPreconditionFailed {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead && status != http.StatusNotModified && status != http.StatusPreconditionFailed {
		_, _ = w.Write([]byte(body))
	}
}

func testMirror() Mirror { return Mirror{Name: "test", Path: "test"} }

func TestCacheFreshGetHeadAndPersistence(t *testing.T) {
	const body = "hello world"
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, body, map[string]string{
			"ETag":          `"v1"`,
			"Cache-Control": "max-age=3600",
		})
	})

	clock := newTestClock()
	dir := t.TempDir()
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{dir: dir, clock: clock, mirrors: []Mirror{testMirror()}})

	first := cacheRequest(s, http.MethodGet, "/test/thing", nil)
	if first.Code != http.StatusOK || first.Body.String() != body {
		t.Fatalf("first GET = %d %q", first.Code, first.Body.String())
	}
	if up.count() != 1 {
		t.Fatalf("upstream after first GET = %d", up.count())
	}

	clock.Advance(5 * time.Second)
	second := cacheRequest(s, http.MethodGet, "/test/thing", nil)
	if second.Code != http.StatusOK || second.Body.String() != body {
		t.Fatalf("second GET = %d %q", second.Code, second.Body.String())
	}
	if up.count() != 1 {
		t.Fatalf("fresh GET hit the upstream: %d", up.count())
	}
	if got := second.Header().Get("Age"); got != "5" {
		t.Fatalf("Age = %q, want 5", got)
	}

	head := cacheRequest(s, http.MethodHead, "/test/thing", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD = %d body %d", head.Code, head.Body.Len())
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("HEAD Content-Length = %q", got)
	}
	if up.count() != 1 {
		t.Fatalf("fresh HEAD hit the upstream: %d", up.count())
	}

	// Restart: the metadata and body survive in the same directory.
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	s2, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{dir: dir, clock: clock, mirrors: []Mirror{testMirror()}})
	afterRestart := cacheRequest(s2, http.MethodGet, "/test/thing", nil)
	if afterRestart.Code != http.StatusOK || afterRestart.Body.String() != body {
		t.Fatalf("GET after restart = %d %q", afterRestart.Code, afterRestart.Body.String())
	}
	if up.count() != 1 {
		t.Fatalf("restart lost the cache entry: %d upstream requests", up.count())
	}
}

func TestCacheKeyIncludesRawQuery(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "body:"+r.URL.RawQuery, map[string]string{"Cache-Control": "max-age=3600"})
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

	if rec := cacheRequest(s, http.MethodGet, "/test/q?a=1", nil); rec.Body.String() != "body:a=1" {
		t.Fatalf("query a=1 body = %q", rec.Body.String())
	}
	if rec := cacheRequest(s, http.MethodGet, "/test/q?a=2", nil); rec.Body.String() != "body:a=2" {
		t.Fatalf("query a=2 body = %q", rec.Body.String())
	}
	if rec := cacheRequest(s, http.MethodGet, "/test/q?a=1", nil); rec.Body.String() != "body:a=1" {
		t.Fatalf("query a=1 second body = %q", rec.Body.String())
	}
	if up.count() != 2 {
		t.Fatalf("query string did not key the cache: %d", up.count())
	}
}

func TestCacheVaryDoesNotSplitOnAcceptEncoding(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body := "lang:" + r.Header.Get("X-Lang")
		writeResponse(w, r, http.StatusOK, body, map[string]string{
			"Vary":          "X-Lang, Accept-Encoding",
			"Cache-Control": "max-age=3600",
		})
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

	if rec := cacheRequest(s, http.MethodGet, "/test/vary", map[string]string{"X-Lang": "en"}); rec.Body.String() != "lang:en" {
		t.Fatalf("en body = %q", rec.Body.String())
	}
	if rec := cacheRequest(s, http.MethodGet, "/test/vary", map[string]string{"X-Lang": "fr"}); rec.Body.String() != "lang:fr" {
		t.Fatalf("fr body = %q", rec.Body.String())
	}
	if rec := cacheRequest(s, http.MethodGet, "/test/vary", map[string]string{"X-Lang": "en"}); rec.Body.String() != "lang:en" {
		t.Fatalf("en second body = %q", rec.Body.String())
	}
	if up.count() != 2 {
		t.Fatalf("vary upstream = %d, want 2", up.count())
	}

	// Accept-Encoding is not a variant dimension. The variant still hits; the
	// body is transcoded for delivery.
	rec := cacheRequest(s, http.MethodGet, "/test/vary", map[string]string{
		"X-Lang":          "en",
		"Accept-Encoding": "gzip",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("encoded hit = %d", rec.Code)
	}
	if up.count() != 2 {
		t.Fatalf("Accept-Encoding created a variant: %d", up.count())
	}
}

func TestCacheAmbiguousVariantsRefetch(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "upstream", map[string]string{
			"Cache-Control": "max-age=3600",
			"Vary":          "X-B, X-A",
		})
	})
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	key := "mirror://test/ambig"

	now := s.currentTime()
	for _, head := range []string{"X-A", "X-B"} {
		content, err := store.Put(context.Background(), strings.NewReader("old"), 3)
		if err != nil {
			t.Fatal(err)
		}
		v := hashstore.Variant{
			URL:             key,
			Vary:            []string{strings.ToLower(head)},
			RequestHeaders:  http.Header{head: {"1"}},
			ResponseHeaders: http.Header{"Cache-Control": {"max-age=3600"}},
			BodySHA256:      content.SHA256,
			BodySize:        content.Size,
			StoredAt:        now,
			ExpiresAt:       now.Add(time.Hour),
		}
		if err := store.PutVariant(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}

	rec := cacheRequest(s, http.MethodGet, "/test/ambig", map[string]string{"X-A": "1", "X-B": "1"})
	if rec.Code != http.StatusOK || rec.Body.String() != "upstream" {
		t.Fatalf("ambiguous response = %d %q", rec.Code, rec.Body.String())
	}
	if up.count() != 1 {
		t.Fatalf("ambiguous request did not refetch: %d", up.count())
	}

	// The refetch committed one identity and removed the other variants, so
	// the next identical request is a plain local hit.
	rec = cacheRequest(s, http.MethodGet, "/test/ambig", map[string]string{"X-A": "1", "X-B": "1"})
	if rec.Code != http.StatusOK || rec.Body.String() != "upstream" {
		t.Fatalf("post-cleanup response = %d %q", rec.Code, rec.Body.String())
	}
	if up.count() != 1 {
		t.Fatalf("ambiguity persisted after refetch: %d", up.count())
	}
	variants, err := store.Variants(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 1 {
		t.Fatalf("ambiguous cleanup left %d variants", len(variants))
	}
}

func TestCacheNoVaryOverridesVaryStar(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/test")
		switch path {
		case "/no-vary":
			writeResponse(w, r, http.StatusOK, "nv", map[string]string{
				"Vary":          "*",
				"X-MDB-Relay":   "no-vary",
				"Cache-Control": "max-age=3600",
			})
		case "/no-vary-no-store":
			writeResponse(w, r, http.StatusOK, "nv", map[string]string{
				"Vary":          "*",
				"X-MDB-Relay":   "no-vary, no-store",
				"Cache-Control": "max-age=3600",
			})
		default:
			writeResponse(w, r, http.StatusOK, "star", map[string]string{
				"Vary":          "*",
				"Cache-Control": "max-age=3600",
			})
		}
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

	for i := 0; i < 2; i++ {
		rec := cacheRequest(s, http.MethodGet, "/test/no-vary", nil)
		if rec.Header().Get(RelayDirectiveHeader) != "" {
			t.Fatalf("X-MDB-Relay leaked to client")
		}
		if rec.Header().Get("Vary") != "*" {
			t.Fatalf("no-vary swallowed the delivered Vary header: %q", rec.Header().Get("Vary"))
		}
	}
	if up.count() != 1 {
		t.Fatalf("no-vary did not override Vary:*: %d", up.count())
	}

	for i := 0; i < 2; i++ {
		cacheRequest(s, http.MethodGet, "/test/no-vary-no-store", nil)
	}
	if up.count() != 3 {
		t.Fatalf("no-vary relaxed no-store: %d", up.count())
	}

	for i := 0; i < 2; i++ {
		cacheRequest(s, http.MethodGet, "/test/star", nil)
	}
	if up.count() != 5 {
		t.Fatalf("Vary:* was stored: %d", up.count())
	}
}

func TestCacheFreshnessRules(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/test")
		switch path {
		case "/ttl":
			writeResponse(w, r, http.StatusOK, "ttl", map[string]string{"X-MDB-Relay": "ttl=60"})
		case "/ttl-zero":
			writeResponse(w, r, http.StatusOK, "ttl0", nil)
		case "/no-store":
			writeResponse(w, r, http.StatusOK, "ns", map[string]string{
				"X-MDB-Relay":   "no-store",
				"Cache-Control": "max-age=3600",
			})
		case "/cc-no-store":
			writeResponse(w, r, http.StatusOK, "ns", map[string]string{"Cache-Control": "no-store"})
		case "/private":
			writeResponse(w, r, http.StatusOK, "priv", map[string]string{"Cache-Control": "private, max-age=3600"})
		case "/no-transform":
			writeResponse(w, r, http.StatusOK, "raw", map[string]string{
				"Cache-Control": "max-age=3600, no-transform",
			})
		}
	})
	clock := newTestClock()
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{testMirror()}})

	cacheRequest(s, http.MethodGet, "/test/ttl", nil)
	clock.Advance(30 * time.Second)
	if rec := cacheRequest(s, http.MethodGet, "/test/ttl", nil); rec.Body.String() != "ttl" {
		t.Fatalf("ttl hit body = %q", rec.Body.String())
	}
	if up.count() != 1 {
		t.Fatalf("ttl entry expired early: %d", up.count())
	}
	clock.Advance(31 * time.Second)
	cacheRequest(s, http.MethodGet, "/test/ttl", nil)
	if up.count() != 2 {
		t.Fatalf("ttl entry never expired: %d", up.count())
	}

	cacheRequest(s, http.MethodGet, "/test/ttl-zero", nil)
	cacheRequest(s, http.MethodGet, "/test/ttl-zero", nil)
	if up.count() != 4 {
		t.Fatalf("missing ttl defaulted to fresh: %d", up.count())
	}

	for _, path := range []string{"/test/no-store", "/test/cc-no-store", "/test/private"} {
		before := up.count()
		for i := 0; i < 2; i++ {
			rec := cacheRequest(s, http.MethodGet, path, nil)
			if rec.Header().Get(RelayDirectiveHeader) != "" {
				t.Fatalf("%s leaked X-MDB-Relay", path)
			}
		}
		if up.count() != before+2 {
			t.Fatalf("%s was cached", path)
		}
	}

	// A no-transform entry stays stored, but an incompatible coding must go
	// upstream instead of being transcoded.
	cacheRequest(s, http.MethodGet, "/test/no-transform", nil)
	before := up.count()
	rec := cacheRequest(s, http.MethodGet, "/test/no-transform", map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != http.StatusOK || rec.Body.String() != "raw" {
		t.Fatalf("no-transform response = %d %q", rec.Code, rec.Body.String())
	}
	if up.count() != before+1 {
		t.Fatalf("no-transform entry was transcoded locally")
	}
	if rec = cacheRequest(s, http.MethodGet, "/test/no-transform", nil); rec.Body.String() != "raw" || up.count() != before+1 {
		t.Fatalf("stored no-transform entry was damaged")
	}
}

func TestCacheExpiresNoCacheAndTTLPrecedence(t *testing.T) {
	clock := newTestClock()
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/test")
		switch path {
		case "/expires":
			writeResponse(w, r, http.StatusOK, "expires", map[string]string{
				"Expires": clock.Now().Add(60 * time.Second).UTC().Format(http.TimeFormat),
			})
		case "/precedence":
			writeResponse(w, r, http.StatusOK, "precedence", map[string]string{
				"Cache-Control": "max-age=1",
				"X-MDB-Relay":   "ttl=1000",
			})
		case "/no-cache":
			if r.Header.Get("If-None-Match") == `"nc"` {
				writeResponse(w, r, http.StatusNotModified, "", map[string]string{
					"ETag":          `"nc"`,
					"Cache-Control": "no-cache, max-age=3600",
				})
				return
			}
			writeResponse(w, r, http.StatusOK, "no-cache", map[string]string{
				"ETag":          `"nc"`,
				"Cache-Control": "no-cache, max-age=3600",
			})
		}
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{testMirror()}})

	// Expires is an absolute freshness boundary.
	cacheRequest(s, http.MethodGet, "/test/expires", nil)
	clock.Advance(30 * time.Second)
	cacheRequest(s, http.MethodGet, "/test/expires", nil)
	if up.count() != 1 {
		t.Fatalf("Expires entry expired early: %d", up.count())
	}
	clock.Advance(31 * time.Second)
	cacheRequest(s, http.MethodGet, "/test/expires", nil)
	if up.count() != 2 {
		t.Fatalf("Expires entry never expired: %d", up.count())
	}

	// Upstream max-age wins over the relay ttl fallback.
	cacheRequest(s, http.MethodGet, "/test/precedence", nil)
	clock.Advance(2 * time.Second)
	cacheRequest(s, http.MethodGet, "/test/precedence", nil)
	if up.count() != 4 {
		t.Fatalf("ttl overrode upstream max-age: %d", up.count())
	}

	// no-cache is stored, but every hit revalidates.
	cacheRequest(s, http.MethodGet, "/test/no-cache", nil)
	rec := cacheRequest(s, http.MethodGet, "/test/no-cache", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "no-cache" {
		t.Fatalf("no-cache revalidation = %d %q", rec.Code, rec.Body.String())
	}
	if up.count() != 6 {
		t.Fatalf("no-cache entry was not revalidated: %d", up.count())
	}
}

func TestCacheRevalidation(t *testing.T) {
	t.Run("304 refreshes metadata", func(t *testing.T) {
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("If-None-Match") == `"1"` {
				writeResponse(w, r, http.StatusNotModified, "", map[string]string{
					"ETag":          `"1"`,
					"Cache-Control": "max-age=60",
				})
				return
			}
			writeResponse(w, r, http.StatusOK, "one", map[string]string{
				"ETag":          `"1"`,
				"Cache-Control": "max-age=0",
			})
		})
		clock := newTestClock()
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{testMirror()}})

		if rec := cacheRequest(s, http.MethodGet, "/test/reval304", nil); rec.Body.String() != "one" {
			t.Fatalf("first body = %q", rec.Body.String())
		}
		clock.Advance(time.Second)
		rec := cacheRequest(s, http.MethodGet, "/test/reval304", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "one" {
			t.Fatalf("revalidated response = %d %q", rec.Code, rec.Body.String())
		}
		if up.count() != 2 {
			t.Fatalf("upstream = %d, want 2", up.count())
		}
		if got := up.lastHeader().Get("If-None-Match"); got != `"1"` {
			t.Fatalf("relay conditional = %q", got)
		}
		// max-age=60 from the 304 makes the entry fresh again.
		if rec := cacheRequest(s, http.MethodGet, "/test/reval304", nil); rec.Body.String() != "one" {
			t.Fatalf("third body = %q", rec.Body.String())
		}
		if up.count() != 2 {
			t.Fatalf("304 did not refresh freshness: %d", up.count())
		}
	})

	t.Run("200 replaces body", func(t *testing.T) {
		var mu sync.Mutex
		version := 1
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			current := version
			mu.Unlock()
			body := "one"
			etag := `"1"`
			freshness := "max-age=0"
			if current == 2 {
				body = "two"
				etag = `"2"`
				freshness = "max-age=60"
			}
			writeResponse(w, r, http.StatusOK, body, map[string]string{
				"ETag":          etag,
				"Cache-Control": freshness,
			})
		})
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

		cacheRequest(s, http.MethodGet, "/test/reval200", nil)
		mu.Lock()
		version = 2
		mu.Unlock()
		clock := s.now
		_ = clock
		rec := cacheRequest(s, http.MethodGet, "/test/reval200", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "two" {
			t.Fatalf("replacement = %d %q", rec.Code, rec.Body.String())
		}
		if up.count() != 2 {
			t.Fatalf("upstream = %d", up.count())
		}
		if rec := cacheRequest(s, http.MethodGet, "/test/reval200", nil); rec.Body.String() != "two" || up.count() != 2 {
			t.Fatalf("replacement was not stored")
		}
	})

	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run("missing variant "+strconv.Itoa(status), func(t *testing.T) {
			var mu sync.Mutex
			first := true
			up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				isFirst := first
				first = false
				mu.Unlock()
				if !isFirst {
					writeResponse(w, r, status, "", nil)
					return
				}
				writeResponse(w, r, http.StatusOK, "gone", map[string]string{
					"ETag":          `"g"`,
					"Cache-Control": "max-age=0",
				})
			})
			s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

			cacheRequest(s, http.MethodGet, "/test/gone", nil)
			if rec := cacheRequest(s, http.MethodGet, "/test/gone", nil); rec.Code != status {
				t.Fatalf("revalidation status = %d, want %d", rec.Code, status)
			}
			if up.count() != 2 {
				t.Fatalf("upstream = %d", up.count())
			}
			// The entry was removed, so the next request is a cold miss.
			if rec := cacheRequest(s, http.MethodGet, "/test/gone", nil); rec.Code != status {
				t.Fatalf("cold status = %d", rec.Code)
			}
			if up.count() != 3 {
				t.Fatalf("missing variant was not deleted: %d", up.count())
			}
		})
	}
}

func TestCacheHeadSemantics(t *testing.T) {
	t.Run("cold HEAD leaves no entry", func(t *testing.T) {
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			writeResponse(w, r, http.StatusOK, "body", map[string]string{"Cache-Control": "max-age=3600"})
		})
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

		if rec := cacheRequest(s, http.MethodHead, "/test/head", nil); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("cold HEAD = %d body %d", rec.Code, rec.Body.Len())
		}
		if rec := cacheRequest(s, http.MethodGet, "/test/head", nil); rec.Body.String() != "body" {
			t.Fatalf("GET after cold HEAD = %q", rec.Body.String())
		}
		if up.count() != 2 {
			t.Fatalf("cold HEAD created an entry: %d", up.count())
		}
	})

	t.Run("stale HEAD 304 refreshes", func(t *testing.T) {
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("If-None-Match") == `"h"` {
				writeResponse(w, r, http.StatusNotModified, "", map[string]string{
					"ETag":          `"h"`,
					"Cache-Control": "max-age=60",
				})
				return
			}
			writeResponse(w, r, http.StatusOK, "head", map[string]string{
				"ETag":          `"h"`,
				"Cache-Control": "max-age=0",
			})
		})
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

		cacheRequest(s, http.MethodGet, "/test/head", nil)
		rec := cacheRequest(s, http.MethodHead, "/test/head", nil)
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("stale HEAD = %d body %d", rec.Code, rec.Body.Len())
		}
		if up.count() != 2 {
			t.Fatalf("stale HEAD did not revalidate: %d", up.count())
		}
		if rec := cacheRequest(s, http.MethodGet, "/test/head", nil); rec.Body.String() != "head" || up.count() != 2 {
			t.Fatalf("304 HEAD did not refresh metadata")
		}
	})

	t.Run("stale HEAD 200 invalidates", func(t *testing.T) {
		var mu sync.Mutex
		body := "one"
		etag := `"1"`
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			currentBody, currentETag := body, etag
			mu.Unlock()
			writeResponse(w, r, http.StatusOK, currentBody, map[string]string{
				"ETag":          currentETag,
				"Cache-Control": "max-age=0",
			})
		})
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

		cacheRequest(s, http.MethodGet, "/test/head", nil)
		mu.Lock()
		body, etag = "two", `"2"`
		mu.Unlock()
		if rec := cacheRequest(s, http.MethodHead, "/test/head", nil); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("stale HEAD 200 = %d body %d", rec.Code, rec.Body.Len())
		}
		if rec := cacheRequest(s, http.MethodGet, "/test/head", nil); rec.Body.String() != "two" {
			t.Fatalf("GET after HEAD 200 = %q", rec.Body.String())
		}
		if up.count() != 3 {
			t.Fatalf("HEAD 200 left a stale entry: %d", up.count())
		}
	})
}

func TestCacheClientConditionsAreIsolated(t *testing.T) {
	const lastModified = "Mon, 05 Jan 2026 00:00:00 GMT"
	var mu sync.Mutex
	body := "one"
	etag := `"1"`
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		currentBody, currentETag := body, etag
		mu.Unlock()
		writeResponse(w, r, http.StatusOK, currentBody, map[string]string{
			"ETag":          currentETag,
			"Last-Modified": lastModified,
			"Cache-Control": "max-age=3600",
		})
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	cacheRequest(s, http.MethodGet, "/test/cond", nil)

	cases := []struct {
		name    string
		headers map[string]string
		status  int
	}{
		{"if-none-match strong", map[string]string{"If-None-Match": `"1"`}, http.StatusNotModified},
		{"if-none-match weak", map[string]string{"If-None-Match": `W/"1"`}, http.StatusNotModified},
		{"if-none-match mismatch", map[string]string{"If-None-Match": `"other"`}, http.StatusOK},
		{"if-modified-since", map[string]string{"If-Modified-Since": lastModified}, http.StatusNotModified},
		{"if-match strong", map[string]string{"If-Match": `"1"`}, http.StatusOK},
		{"if-match mismatch", map[string]string{"If-Match": `"other"`}, http.StatusPreconditionFailed},
		{"if-match weak never matches", map[string]string{"If-Match": `W/"1"`}, http.StatusPreconditionFailed},
		{"if-unmodified-since older", map[string]string{"If-Unmodified-Since": "Sun, 04 Jan 2026 00:00:00 GMT"}, http.StatusPreconditionFailed},
		{"if-unmodified-since newer", map[string]string{"If-Unmodified-Since": "Tue, 06 Jan 2026 00:00:00 GMT"}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := cacheRequest(s, http.MethodGet, "/test/cond", tc.headers); rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
		})
	}
	if up.count() != 1 {
		t.Fatalf("local conditions reached upstream: %d", up.count())
	}

	// Client condition is not relay revalidation: upstream 304 is caused only
	// by our own If-None-Match, then the client's own condition is evaluated
	// locally (and here does not match).
	staleUp, staleSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"1"` {
			writeResponse(w, r, http.StatusNotModified, "", map[string]string{
				"ETag":          `"1"`,
				"Cache-Control": "max-age=60",
			})
			return
		}
		writeResponse(w, r, http.StatusOK, "one", map[string]string{
			"ETag":          `"1"`,
			"Cache-Control": "max-age=0",
		})
	})
	stale, _ := newCacheServer(t, staleSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	cacheRequest(stale, http.MethodGet, "/test/iso", nil)
	rec := cacheRequest(stale, http.MethodGet, "/test/iso", map[string]string{"If-None-Match": `"nope"`})
	if rec.Code != http.StatusOK || rec.Body.String() != "one" {
		t.Fatalf("isolated condition = %d %q", rec.Code, rec.Body.String())
	}
	if got := staleUp.lastHeader().Get("If-None-Match"); got != `"1"` {
		t.Fatalf("relay sent %q instead of its own validator", got)
	}

	// Conversely, a client condition matching the old validator must not turn a
	// replacement 200 into a 304.
	var changedMu sync.Mutex
	changedBody, changedETag := "one", `"1"`
	changedUp, changedSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		changedMu.Lock()
		currentBody, currentETag := changedBody, changedETag
		changedMu.Unlock()
		writeResponse(w, r, http.StatusOK, currentBody, map[string]string{
			"ETag":          currentETag,
			"Cache-Control": "max-age=0",
		})
	})
	changed, _ := newCacheServer(t, changedSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	cacheRequest(changed, http.MethodGet, "/test/cond2", nil)
	changedMu.Lock()
	changedBody, changedETag = "two", `"2"`
	changedMu.Unlock()
	rec = cacheRequest(changed, http.MethodGet, "/test/cond2", map[string]string{"If-None-Match": `"1"`})
	if rec.Code != http.StatusOK || rec.Body.String() != "two" {
		t.Fatalf("changed resource with matching client condition = %d %q", rec.Code, rec.Body.String())
	}
	if changedUp.count() != 2 {
		t.Fatalf("changed resource upstream = %d", changedUp.count())
	}
}

func TestCachePassthroughAndRequestDirective(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/test/echo") {
			writeResponse(w, r, http.StatusOK, "seen:"+r.Header.Get(RelayDirectiveHeader), map[string]string{"Cache-Control": "no-store"})
			return
		}
		headers := map[string]string{"Cache-Control": "max-age=3600"}
		if strings.HasPrefix(r.URL.Path, "/test/pass/") {
			headers[RelayDirectiveHeader] = "no-store"
		}
		writeResponse(w, r, http.StatusOK, "body", headers)
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors: []Mirror{testMirror()},
		policies: []Policy{{
			Name:  "passthrough",
			Match: PolicyMatch{Mirror: "test", PathPrefix: "/pass/"},
			Rules: PolicyRules{Passthrough: boolRule(true)},
		}},
	})

	for i := 0; i < 2; i++ {
		rec := cacheRequest(s, http.MethodGet, "/test/pass/x", nil)
		if got := rec.Header().Get(RelayDirectiveHeader); got != "no-store" {
			t.Fatalf("passthrough response lost X-MDB-Relay: %q", got)
		}
	}
	if up.count() != 2 {
		t.Fatalf("passthrough was cached: %d", up.count())
	}

	cacheRequest(s, http.MethodGet, "/test/open/x", nil)
	cacheRequest(s, http.MethodGet, "/test/open/x", nil)
	if up.count() != 3 {
		t.Fatalf("non-passthrough was not cached: %d", up.count())
	}

	// Direct client: no X-MDB-Relay-Host, so its X-MDB-Relay is stripped.
	rec := cacheRequest(s, http.MethodGet, "/test/echo1", map[string]string{RelayDirectiveHeader: "ttl=5"})
	if rec.Body.String() != "seen:" {
		t.Fatalf("direct request directive reached upstream: %q", rec.Body.String())
	}
	// Multi-tier: the request already carries X-MDB-Relay-Host and keeps it.
	rec = cacheRequest(s, http.MethodGet, "/test/echo2", map[string]string{
		RelayHostHeader:      "upper.example.com",
		RelayDirectiveHeader: "ttl=5",
	})
	if rec.Body.String() != "seen:ttl=5" {
		t.Fatalf("relayed request directive was stripped: %q", rec.Body.String())
	}
}

func TestCacheStoredResponseHeaders(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "a=b")
		w.Header().Set("WWW-Authenticate", `Basic realm=x`)
		w.Header().Set("Connection", "X-Drop")
		w.Header().Set("X-Drop", "secret")
		w.Header().Set("Vary", "Accept-Language")
		w.Header().Set("ETag", `"stored"`)
		writeResponse(w, r, http.StatusOK, "body", map[string]string{
			"Cache-Control": "max-age=3600",
			"X-MDB-Relay":   "ttl=60",
		})
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

	cacheRequest(s, http.MethodGet, "/test/headers", nil)
	rec := cacheRequest(s, http.MethodGet, "/test/headers", nil)
	if up.count() != 1 {
		t.Fatalf("stored headers test missed: %d", up.count())
	}
	for _, name := range []string{"Set-Cookie", "WWW-Authenticate", "X-Drop", RelayDirectiveHeader} {
		if got := rec.Header().Get(name); got != "" {
			t.Errorf("stored/delivered header %s = %q", name, got)
		}
	}
	if rec.Header().Get("Vary") != "Accept-Language" || rec.Header().Get("ETag") != `"stored"` {
		t.Errorf("end-to-end headers lost: %v", rec.Header())
	}
	if rec.Header().Get("Content-Length") != "4" {
		t.Errorf("Content-Length = %q", rec.Header().Get("Content-Length"))
	}
	if rec.Header().Get("Age") == "" {
		t.Errorf("Age missing")
	}
}

func TestCacheStoreFailureFallsBack(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "body", map[string]string{"Cache-Control": "max-age=3600"})
	})

	t.Run("oversized body still delivered", func(t *testing.T) {
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{maxSize: 16, mirrors: []Mirror{testMirror()}})
		long := strings.Repeat("x", 100)
		overUp, overSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			writeResponse(w, r, http.StatusOK, long, map[string]string{"Cache-Control": "max-age=3600"})
		})
		s.upstream = mustURL(t, overSrv.URL)
		s.proxy = s.newReverseProxy()

		if rec := cacheRequest(s, http.MethodGet, "/test/big", nil); rec.Body.String() != long {
			t.Fatalf("oversized body was truncated: %d bytes", rec.Body.Len())
		}
		if overUp.count() != 1 {
			t.Fatalf("upstream = %d", overUp.count())
		}
		cacheRequest(s, http.MethodGet, "/test/big", nil)
		if overUp.count() != 2 {
			t.Fatalf("oversized body was stored: %d", overUp.count())
		}
	})

	t.Run("closed store falls back to proxy", func(t *testing.T) {
		s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		before := up.count()
		rec := cacheRequest(s, http.MethodGet, "/test/broken", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "body" {
			t.Fatalf("closed store broke delivery: %d %q", rec.Code, rec.Body.String())
		}
		if up.count() != before+1 {
			t.Fatalf("upstream = %d", up.count())
		}
	})

	t.Run("missing body repairs entry", func(t *testing.T) {
		dir := t.TempDir()
		lostUp, lostSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			writeResponse(w, r, http.StatusOK, "body", map[string]string{"Cache-Control": "max-age=3600"})
		})
		s, _ := newCacheServer(t, lostSrv.URL, cacheServerOptions{dir: dir, mirrors: []Mirror{testMirror()}})
		cacheRequest(s, http.MethodGet, "/test/lost", nil)
		if lostUp.count() != 1 {
			t.Fatalf("setup upstream = %d", lostUp.count())
		}
		files, err := filepath.Glob(filepath.Join(dir, "data", "*", "*", "*"))
		if err != nil || len(files) == 0 {
			t.Fatalf("find stored body: %v %v", files, err)
		}
		for _, file := range files {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		}
		rec := cacheRequest(s, http.MethodGet, "/test/lost", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "body" {
			t.Fatalf("missing body response = %d %q", rec.Code, rec.Body.String())
		}
		if lostUp.count() != 2 {
			t.Fatalf("missing body was not refetched: %d", lostUp.count())
		}
		cacheRequest(s, http.MethodGet, "/test/lost", nil)
		if lostUp.count() != 2 {
			t.Fatalf("missing body was not repaired: %d", lostUp.count())
		}
	})
}

type abortWriter struct {
	header http.Header
	limit  int
	wrote  int
	code   int
}

func (w *abortWriter) Header() http.Header  { return w.header }
func (w *abortWriter) WriteHeader(code int) { w.code = code }
func (w *abortWriter) Write(p []byte) (int, error) {
	if w.wrote >= w.limit {
		return 0, errClientAborted
	}
	w.wrote += len(p)
	return len(p), nil
}

func TestCacheClientAbortDoesNotCommit(t *testing.T) {
	long := bytes.Repeat([]byte("z"), 256*1024)
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, string(long), map[string]string{"Cache-Control": "max-age=3600"})
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

	req := httptest.NewRequest(http.MethodGet, "http://relay.example.com/test/abort", nil)
	aw := &abortWriter{header: make(http.Header), limit: 10}
	s.handleRequest(aw, req)
	if up.count() != 1 {
		t.Fatalf("upstream = %d", up.count())
	}

	rec := cacheRequest(s, http.MethodGet, "/test/abort", nil)
	if rec.Body.Len() != len(long) {
		t.Fatalf("refetch body = %d bytes", rec.Body.Len())
	}
	if up.count() != 2 {
		t.Fatalf("aborted body was committed: %d", up.count())
	}
}

func TestCacheKeepHTTPSharesTheChain(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "http", map[string]string{"Cache-Control": "max-age=3600"})
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors: []Mirror{{Name: "test", Path: "test", KeepHTTP: true}},
	})

	req := httptest.NewRequest(http.MethodGet, "http://relay.example.com/test/keep", nil)
	if !s.ShouldKeepHTTP(req) {
		t.Fatal("keepHTTP mirror was not recognised")
	}
	rec := httptest.NewRecorder()
	s.handleRequest(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "http" {
		t.Fatalf("keepHTTP first = %d %q", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "http://relay.example.com/test/keep", nil)
	rec = httptest.NewRecorder()
	s.handleRequest(rec, req)
	if rec.Body.String() != "http" || up.count() != 1 {
		t.Fatalf("keepHTTP did not hit the cache: %d", up.count())
	}
}

func TestCacheUnresolvedRequestsBypass(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "internal", map[string]string{"Cache-Control": "max-age=3600"})
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	for i := 0; i < 2; i++ {
		cacheRequest(s, http.MethodGet, "/api/mirrors", nil)
	}
	if up.count() != 2 {
		t.Fatalf("unresolved endpoint entered the cache: %d", up.count())
	}
	for i := 0; i < 2; i++ {
		cacheRequest(s, http.MethodPost, "/test/write", nil)
	}
	if up.count() != 4 {
		t.Fatalf("non-GET/HEAD entered the cache: %d", up.count())
	}
}

func TestCacheRange(t *testing.T) {
	const body = "0123456789"
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, body, map[string]string{
			"ETag":          `"r1"`,
			"Last-Modified": "Mon, 05 Jan 2026 00:00:00 GMT",
			"Cache-Control": "max-age=3600",
		})
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	cacheRequest(s, http.MethodGet, "/test/range", nil)

	single := cacheRequest(s, http.MethodGet, "/test/range", map[string]string{"Range": "bytes=2-5"})
	if single.Code != http.StatusPartialContent || single.Body.String() != "2345" {
		t.Fatalf("single range = %d %q", single.Code, single.Body.String())
	}
	if got := single.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q", got)
	}
	if got := single.Header().Get("Content-Length"); got != "4" {
		t.Fatalf("range Content-Length = %q", got)
	}

	suffix := cacheRequest(s, http.MethodGet, "/test/range", map[string]string{"Range": "bytes=-3"})
	if suffix.Code != http.StatusPartialContent || suffix.Body.String() != "789" {
		t.Fatalf("suffix range = %d %q", suffix.Code, suffix.Body.String())
	}
	open := cacheRequest(s, http.MethodGet, "/test/range", map[string]string{"Range": "bytes=7-"})
	if open.Code != http.StatusPartialContent || open.Body.String() != "789" {
		t.Fatalf("open range = %d %q", open.Code, open.Body.String())
	}

	multi := cacheRequest(s, http.MethodGet, "/test/range", map[string]string{"Range": "bytes=0-1,8-9"})
	if multi.Code != http.StatusPartialContent {
		t.Fatalf("multi range = %d", multi.Code)
	}
	mediaType, params, err := mime.ParseMediaType(multi.Header().Get("Content-Type"))
	if err != nil || mediaType != "multipart/byteranges" {
		t.Fatalf("multipart type = %q %v", mediaType, err)
	}
	reader := multipart.NewReader(multi.Body, params["boundary"])
	var parts []string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("part: %v", err)
		}
		data, _ := io.ReadAll(part)
		parts = append(parts, part.Header.Get("Content-Range")+":"+string(data))
	}
	wantParts := []string{"bytes 0-1/10:01", "bytes 8-9/10:89"}
	if strings.Join(parts, "|") != strings.Join(wantParts, "|") {
		t.Fatalf("multipart parts = %v", parts)
	}

	unsat := cacheRequest(s, http.MethodGet, "/test/range", map[string]string{"Range": "bytes=20-30"})
	if unsat.Code != http.StatusRequestedRangeNotSatisfiable || unsat.Header().Get("Content-Range") != "bytes */10" {
		t.Fatalf("416 = %d %q", unsat.Code, unsat.Header().Get("Content-Range"))
	}

	if rec := cacheRequest(s, http.MethodGet, "/test/range", map[string]string{"Range": "bytes=2-5", "If-Range": `"r1"`}); rec.Code != http.StatusPartialContent {
		t.Fatalf("matching If-Range = %d", rec.Code)
	}
	for _, ifRange := range []string{`"other"`, `W/"r1"`, "Tue, 06 Jan 2026 00:00:00 GMT"} {
		rec := cacheRequest(s, http.MethodGet, "/test/range", map[string]string{"Range": "bytes=2-5", "If-Range": ifRange})
		if rec.Code != http.StatusOK || rec.Body.String() != body {
			t.Fatalf("If-Range %q = %d %q", ifRange, rec.Code, rec.Body.String())
		}
	}
	date := cacheRequest(s, http.MethodGet, "/test/range", map[string]string{
		"Range":    "bytes=0-0",
		"If-Range": "Mon, 05 Jan 2026 00:00:00 GMT",
	})
	if date.Code != http.StatusPartialContent || date.Body.String() != "0" {
		t.Fatalf("date If-Range = %d %q", date.Code, date.Body.String())
	}

	head := cacheRequest(s, http.MethodHead, "/test/range", map[string]string{"Range": "bytes=0-1"})
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != "10" {
		t.Fatalf("HEAD range = %d body %d CL %q", head.Code, head.Body.Len(), head.Header().Get("Content-Length"))
	}
	if up.count() != 1 {
		t.Fatalf("local range hit the upstream: %d", up.count())
	}
}

func TestCacheRangeMiss(t *testing.T) {
	t.Run("full 200 is stored", func(t *testing.T) {
		const body = "0123456789"
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			writeResponse(w, r, http.StatusOK, body, map[string]string{"Cache-Control": "max-age=3600"})
		})
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

		var logBuf bytes.Buffer
		previousLog := log.Writer()
		log.SetOutput(&logBuf)
		rec := cacheRequest(s, http.MethodGet, "/test/rmiss", map[string]string{"Range": "bytes=0-1"})
		log.SetOutput(previousLog)
		if rec.Code != http.StatusOK || rec.Body.String() != body {
			t.Fatalf("ignored Range response = %d %q", rec.Code, rec.Body.String())
		}
		if !strings.Contains(logBuf.String(), "ignored Range") {
			t.Fatalf("no warning logged for a full 200 answering a Range request: %q", logBuf.String())
		}
		if rec := cacheRequest(s, http.MethodGet, "/test/rmiss", nil); rec.Code != http.StatusOK || rec.Body.String() != body {
			t.Fatalf("full GET = %d %q", rec.Code, rec.Body.String())
		}
		if up.count() != 1 {
			t.Fatalf("full 200 was not stored: %d", up.count())
		}
	})

	t.Run("206 is not stored", func(t *testing.T) {
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Range") != "" {
				w.Header().Set("Content-Range", "bytes 0-1/10")
				writeResponse(w, r, http.StatusPartialContent, "01", nil)
				return
			}
			writeResponse(w, r, http.StatusOK, "0123456789", map[string]string{"Cache-Control": "max-age=3600"})
		})
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

		if rec := cacheRequest(s, http.MethodGet, "/test/r206", map[string]string{"Range": "bytes=0-1"}); rec.Code != http.StatusPartialContent {
			t.Fatalf("206 = %d", rec.Code)
		}
		cacheRequest(s, http.MethodGet, "/test/r206", nil)
		if up.count() != 2 {
			t.Fatalf("206 was stored: %d", up.count())
		}
	})
}

func gzipBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCacheEncodingNegotiation(t *testing.T) {
	payload := []byte(strings.Repeat("payload-", 100))
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, string(payload), map[string]string{
			"Cache-Control": "max-age=3600",
			"ETag":          `"enc"`,
		})
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	cacheRequest(s, http.MethodGet, "/test/enc", nil)

	for _, coding := range []ContentCoding{CodingGzip, CodingDeflate, CodingBrotli, CodingZstd} {
		rec := cacheRequest(s, http.MethodGet, "/test/enc", map[string]string{"Accept-Encoding": string(coding)})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", coding, rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != string(coding) {
			t.Fatalf("%s Content-Encoding = %q", coding, got)
		}
		if rec.Header().Get("Content-Length") != "" {
			t.Fatalf("%s transcoded response kept Content-Length", coding)
		}
		decoded, err := Transcode(rec.Body, coding, CodingIdentity)
		if err != nil {
			t.Fatalf("%s decode: %v", coding, err)
		}
		got, _ := io.ReadAll(decoded)
		decoded.Close()
		if !bytes.Equal(got, payload) {
			t.Fatalf("%s body changed", coding)
		}
	}
	if up.count() != 1 {
		t.Fatalf("transcoding hit upstream: %d", up.count())
	}

	// HEAD negotiates the same way, changing only headers.
	head := cacheRequest(s, http.MethodHead, "/test/enc", map[string]string{"Accept-Encoding": "gzip"})
	if head.Code != http.StatusOK || head.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("HEAD encoding = %d %q", head.Code, head.Header().Get("Content-Encoding"))
	}
	if head.Header().Get("Content-Length") != "" || head.Body.Len() != 0 {
		t.Fatalf("HEAD transcode produced a body or length")
	}

	// A transcoded representation has a weak validator: a strong If-Match can
	// never match it, while If-None-Match still uses weak comparison.
	if rec := cacheRequest(s, http.MethodGet, "/test/enc", map[string]string{
		"Accept-Encoding": "gzip",
		"If-Match":        `"enc"`,
	}); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("If-Match on a transcoded delivery = %d", rec.Code)
	}
	notModified := cacheRequest(s, http.MethodGet, "/test/enc", map[string]string{
		"Accept-Encoding": "gzip",
		"If-None-Match":   `"enc"`,
	})
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match on transcoded delivery = %d", notModified.Code)
	}
	if got := notModified.Header().Get("ETag"); !strings.HasPrefix(got, "W/") {
		t.Fatalf("304 ETag was not weak: %q", got)
	}
	if up.count() != 1 {
		t.Fatalf("transcoded conditional reached upstream: %d", up.count())
	}

	// No acceptable coding: re-fetch upstream.
	before := up.count()
	if rec := cacheRequest(s, http.MethodGet, "/test/enc", map[string]string{"Accept-Encoding": "identity;q=0"}); rec.Code != http.StatusOK {
		t.Fatalf("unacceptable coding = %d", rec.Code)
	}
	if up.count() != before+1 {
		t.Fatalf("unacceptable coding did not refetch: %d", up.count())
	}

	// A range on a hit that needs transcoding goes upstream rather than slicing
	// a re-encoded stream.
	before = up.count()
	if rec := cacheRequest(s, http.MethodGet, "/test/enc", map[string]string{
		"Accept-Encoding": "gzip",
		"Range":           "bytes=0-3",
	}); rec.Code != http.StatusOK {
		t.Fatalf("transcoded range = %d", rec.Code)
	}
	if up.count() != before+1 {
		t.Fatalf("transcoded range did not refetch: %d", up.count())
	}
}

func TestCacheStoredGzipAndIdentityDelivery(t *testing.T) {
	payload := []byte(strings.Repeat("compressed-", 40))
	compressed := gzipBytes(t, payload)
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
		w.Header().Set("Cache-Control", "max-age=3600")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(compressed)
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	cacheRequest(s, http.MethodGet, "/test/gz", nil)

	same := cacheRequest(s, http.MethodGet, "/test/gz", map[string]string{"Accept-Encoding": "gzip"})
	if same.Header().Get("Content-Encoding") != "gzip" || !bytes.Equal(same.Body.Bytes(), compressed) {
		t.Fatalf("stored gzip was not delivered untouched")
	}
	if up.count() != 1 {
		t.Fatalf("upstream = %d", up.count())
	}

	decoded := cacheRequest(s, http.MethodGet, "/test/gz", map[string]string{"Accept-Encoding": "identity"})
	if decoded.Header().Get("Content-Encoding") != "" {
		t.Fatalf("identity transcoding kept Content-Encoding")
	}
	got := decoded.Body.Bytes()
	if !bytes.Equal(got, payload) {
		t.Fatalf("gzip -> identity body changed")
	}
	if up.count() != 1 {
		t.Fatalf("upstream = %d", up.count())
	}
}

func TestCacheRefusesTranscodingDigestAndOCI(t *testing.T) {
	payload := strings.Repeat("digest-", 50)
	t.Run("digest header", func(t *testing.T) {
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			writeResponse(w, r, http.StatusOK, payload, map[string]string{
				"Cache-Control":         "max-age=3600",
				"Docker-Content-Digest": "sha256:" + strings.Repeat("a", 64),
			})
		})
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
		cacheRequest(s, http.MethodGet, "/test/digest", nil)
		before := up.count()
		rec := cacheRequest(s, http.MethodGet, "/test/digest", map[string]string{"Accept-Encoding": "gzip"})
		if rec.Code != http.StatusOK || rec.Header().Get("Docker-Content-Digest") == "" {
			t.Fatalf("digest response = %d", rec.Code)
		}
		if up.count() != before+1 {
			t.Fatalf("digest entry was transcoded locally")
		}
	})

	t.Run("OCI digest path", func(t *testing.T) {
		digest := strings.Repeat("b", 64)
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			writeResponse(w, r, http.StatusOK, payload, map[string]string{"Cache-Control": "max-age=3600"})
		})
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
			mirrors: []Mirror{{Name: "docker", Path: "docker"}},
		})
		target := "/docker/v2/library/nginx/blobs/sha256:" + digest
		cacheRequest(s, http.MethodGet, target, nil)
		before := up.count()
		rec := cacheRequest(s, http.MethodGet, target, map[string]string{"Accept-Encoding": "gzip"})
		if rec.Code != http.StatusOK {
			t.Fatalf("OCI response = %d", rec.Code)
		}
		if up.count() != before+1 {
			t.Fatalf("OCI digest path was transcoded locally")
		}
	})
}
