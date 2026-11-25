package server

import (
	"bytes"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

func disablePrefetchJitter(s *Server) {
	s.prefetch.jitter = func(string, time.Duration) time.Duration { return 0 }
}

func onlyPrefetchTask(t *testing.T, s *Server) *prefetchTask {
	t.Helper()
	s.prefetch.mu.Lock()
	defer s.prefetch.mu.Unlock()
	if len(s.prefetch.tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(s.prefetch.tasks))
	}
	for _, task := range s.prefetch.tasks {
		copy := *task
		return &copy
	}
	return nil
}

func dispatchAndWait(s *Server) {
	s.prefetch.dispatchDue()
	s.prefetch.wg.Wait()
}

func firstVariant(t *testing.T, store *hashstore.Store, key string) hashstore.Variant {
	t.Helper()
	variants, err := store.Variants(t.Context(), key)
	if err != nil {
		t.Fatalf("list variants: %v", err)
	}
	if len(variants) != 1 {
		t.Fatalf("variants = %d, want 1", len(variants))
	}
	return variants[0]
}

func TestPrefetchBackgroundRevalidation(t *testing.T) {
	var (
		mu          sync.Mutex
		body        = "v1"
		etag        = `"v1"`
		conditional int
	)
	_, up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("If-None-Match") == etag {
			conditional++
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", "max-age=120")
			w.Header().Set(RelayDirectiveHeader, "prefetch")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set(RelayDirectiveHeader, "prefetch")
		writeResponse(w, r, http.StatusOK, body, nil)
	})

	clock := newTestClock()
	s, store := newCacheServer(t, up.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{testMirror()}})
	disablePrefetchJitter(s)

	first := cacheRequest(s, http.MethodGet, "/test/thing", nil)
	if first.Code != http.StatusOK || first.Body.String() != "v1" {
		t.Fatalf("first GET = %d %q", first.Code, first.Body.String())
	}
	task := onlyPrefetchTask(t, s)
	if !task.hasNext {
		t.Fatal("task has no next refresh")
	}

	clock.Advance(60 * time.Second)
	dispatchAndWait(s)

	mu.Lock()
	gotConditional := conditional
	mu.Unlock()
	if gotConditional != 1 {
		t.Fatalf("conditional requests = %d, want 1", gotConditional)
	}
	v := firstVariant(t, store, "mirror://test/thing")
	if want := clock.Now().Add(120 * time.Second); !v.ExpiresAt.Equal(want) {
		t.Fatalf("expiry after 304 = %v, want %v", v.ExpiresAt, want)
	}
	if v.BodySize != int64(len("v1")) {
		t.Fatalf("304 changed the body size to %d", v.BodySize)
	}
}

func TestPrefetchBackgroundFullReplacement(t *testing.T) {
	var (
		mu          sync.Mutex
		current     = "v1"
		conditional int
	)
	_, up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("If-None-Match") != "" {
			conditional++
			current = "v2"
			w.Header().Set("ETag", `"v2"`)
			w.Header().Set("Cache-Control", "max-age=60")
			w.Header().Set(RelayDirectiveHeader, "prefetch")
			writeResponse(w, r, http.StatusOK, current, nil)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set(RelayDirectiveHeader, "prefetch")
		writeResponse(w, r, http.StatusOK, current, nil)
	})

	clock := newTestClock()
	s, store := newCacheServer(t, up.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{testMirror()}})
	disablePrefetchJitter(s)

	cacheRequest(s, http.MethodGet, "/test/thing", nil)
	clock.Advance(60 * time.Second)
	dispatchAndWait(s)

	mu.Lock()
	gotConditional := conditional
	mu.Unlock()
	if gotConditional != 1 {
		t.Fatalf("conditional requests = %d, want 1", gotConditional)
	}
	v := firstVariant(t, store, "mirror://test/thing")
	if v.ResponseHeaders.Get("ETag") != `"v2"` {
		t.Fatalf("ETag after replacement = %q", v.ResponseHeaders.Get("ETag"))
	}
	after := cacheRequest(s, http.MethodGet, "/test/thing", nil)
	if after.Body.String() != "v2" {
		t.Fatalf("served body after replacement = %q", after.Body.String())
	}
}

func TestPrefetchBackgroundDefiniteAbsenceDropsVariant(t *testing.T) {
	var refresh atomic.Bool
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if refresh.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set(RelayDirectiveHeader, "prefetch")
		writeResponse(w, r, http.StatusOK, "v1", nil)
	})

	clock := newTestClock()
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{testMirror()}})
	disablePrefetchJitter(s)

	cacheRequest(s, http.MethodGet, "/test/thing", nil)
	refresh.Store(true)
	clock.Advance(60 * time.Second)
	dispatchAndWait(s)

	variants, err := store.Variants(t.Context(), "mirror://test/thing")
	if err != nil {
		t.Fatalf("list variants: %v", err)
	}
	if len(variants) != 0 {
		t.Fatalf("404 left %d variants", len(variants))
	}
	if got := up.count(); got != 2 {
		t.Fatalf("upstream hits = %d, want 2", got)
	}
}

func TestPrefetchNoFreshnessDoesNotPoll(t *testing.T) {
	for _, cacheControl := range []string{"no-cache", ""} {
		t.Run(cacheControl, func(t *testing.T) {
			up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"v1"`)
				if cacheControl != "" {
					w.Header().Set("Cache-Control", cacheControl)
				}
				w.Header().Set(RelayDirectiveHeader, "prefetch")
				writeResponse(w, r, http.StatusOK, "body", nil)
			})
			clock := newTestClock()
			s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{testMirror()}})
			disablePrefetchJitter(s)

			cacheRequest(s, http.MethodGet, "/test/thing", nil)
			task := onlyPrefetchTask(t, s)
			if task.hasNext {
				t.Fatal("task was scheduled without positive freshness")
			}
			clock.Advance(24 * time.Hour)
			dispatchAndWait(s)
			if got := up.count(); got != 1 {
				t.Fatalf("upstream hits = %d, want only the real request", got)
			}
		})
	}
}

func TestPrefetchReplaysVariantHeaders(t *testing.T) {
	var (
		mu          sync.Mutex
		conditional http.Header
	)
	_, up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conditional = r.Header.Clone()
		mu.Unlock()
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Cache-Control", "max-age=60")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Vary", "Accept-Language")
		w.Header().Set(RelayDirectiveHeader, "prefetch")
		writeResponse(w, r, http.StatusOK, "body", nil)
	})
	clock := newTestClock()
	s, _ := newCacheServer(t, up.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{testMirror()}})
	disablePrefetchJitter(s)

	cacheRequest(s, http.MethodGet, "/test/thing", map[string]string{"Accept-Language": "zh-CN"})
	clock.Advance(60 * time.Second)
	dispatchAndWait(s)

	mu.Lock()
	got := conditional
	mu.Unlock()
	if got.Get("Accept-Language") != "zh-CN" {
		t.Fatalf("background Accept-Language = %q", got.Get("Accept-Language"))
	}
	if got.Get("Accept-Encoding") != "identity" {
		t.Fatalf("background Accept-Encoding = %q", got.Get("Accept-Encoding"))
	}
	if got.Get("If-None-Match") != `"v1"` {
		t.Fatalf("background If-None-Match = %q", got.Get("If-None-Match"))
	}
}

func TestPrefetchHeadRegistersButRefreshesWithGet(t *testing.T) {
	var (
		mu      sync.Mutex
		methods []string
	)
	_, up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set(RelayDirectiveHeader, "prefetch")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("body"))
		}
	})
	clock := newTestClock()
	s, store := newCacheServer(t, up.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{testMirror()}})
	disablePrefetchJitter(s)

	head := cacheRequest(s, http.MethodHead, "/test/thing", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD = %d body %d", head.Code, head.Body.Len())
	}
	task := onlyPrefetchTask(t, s)
	if !task.hasNext {
		t.Fatal("HEAD did not register a schedule")
	}
	variants, err := store.Variants(t.Context(), "mirror://test/thing")
	if err != nil {
		t.Fatalf("list variants: %v", err)
	}
	if len(variants) != 0 {
		t.Fatalf("HEAD created %d variants", len(variants))
	}

	clock.Advance(60 * time.Second)
	dispatchAndWait(s)

	mu.Lock()
	got := append([]string(nil), methods...)
	mu.Unlock()
	if len(got) != 2 || got[1] != http.MethodGet {
		t.Fatalf("background methods = %v, want HEAD then GET", got)
	}
	variants, err = store.Variants(t.Context(), "mirror://test/thing")
	if err != nil {
		t.Fatalf("list variants: %v", err)
	}
	if len(variants) != 1 {
		t.Fatalf("background GET stored %d variants", len(variants))
	}
}

func TestPrefetchRevalidationDiscoversLeaseAfterRestart(t *testing.T) {
	_, up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Cache-Control", "max-age=60")
			w.Header().Set(RelayDirectiveHeader, "prefetch")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set(RelayDirectiveHeader, "prefetch")
		writeResponse(w, r, http.StatusOK, "body", nil)
	})

	clock := newTestClock()
	dir := t.TempDir()
	s1, store1 := newCacheServer(t, up.URL, cacheServerOptions{dir: dir, clock: clock, mirrors: []Mirror{testMirror()}})
	disablePrefetchJitter(s1)
	cacheRequest(s1, http.MethodGet, "/test/thing", nil)
	if len(s1.prefetch.tasks) != 1 {
		t.Fatalf("first server tasks = %d", len(s1.prefetch.tasks))
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	// A restart keeps the variant but starts with no lease.
	s2, _ := newCacheServer(t, up.URL, cacheServerOptions{dir: dir, clock: clock, mirrors: []Mirror{testMirror()}})
	disablePrefetchJitter(s2)
	if len(s2.prefetch.tasks) != 0 {
		t.Fatalf("restart restored %d leases", len(s2.prefetch.tasks))
	}

	clock.Advance(60 * time.Second)
	resp := cacheRequest(s2, http.MethodGet, "/test/thing", nil)
	if resp.Code != http.StatusOK || resp.Body.String() != "body" {
		t.Fatalf("revalidated GET = %d %q", resp.Code, resp.Body.String())
	}
	if len(s2.prefetch.tasks) != 1 {
		t.Fatalf("304 with prefetch did not register a lease: %d", len(s2.prefetch.tasks))
	}
}

func TestPrefetchNoStoreConflictKeepsCacheEmpty(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set(RelayDirectiveHeader, "prefetch")
		writeResponse(w, r, http.StatusOK, "body", nil)
	})
	clock := newTestClock()
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{testMirror()}})
	disablePrefetchJitter(s)

	resp := cacheRequest(s, http.MethodGet, "/test/thing", nil)
	if resp.Body.String() != "body" {
		t.Fatalf("body = %q", resp.Body.String())
	}
	variants, err := store.Variants(t.Context(), "mirror://test/thing")
	if err != nil {
		t.Fatalf("list variants: %v", err)
	}
	if len(variants) != 0 {
		t.Fatalf("no-store response was cached: %d variants", len(variants))
	}
	task := onlyPrefetchTask(t, s)
	if !task.backgroundDisabled || task.hasNext {
		t.Fatalf("no-store task = disabled %v, hasNext %v", task.backgroundDisabled, task.hasNext)
	}
	clock.Advance(24 * time.Hour)
	dispatchAndWait(s)
	if got := up.count(); got != 1 {
		t.Fatalf("background wrote for a no-store task: %d upstream hits", got)
	}
}

// TestPrefetchBackgroundKeepsEscapedPath: the background replay must address the
// same resource the client did. %2F is a path character, not a separator, and
// rebuilding the URL from the decoded path would silently change the target.
func TestPrefetchBackgroundKeepsEscapedPath(t *testing.T) {
	var (
		mu       sync.Mutex
		escaped  []string
		requests []string
	)
	_, up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		escaped = append(escaped, r.URL.EscapedPath())
		requests = append(requests, r.RequestURI)
		mu.Unlock()
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set(RelayDirectiveHeader, "prefetch")
		writeResponse(w, r, http.StatusOK, "body", nil)
	})
	clock := newTestClock()
	s, _ := newCacheServer(t, up.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{testMirror()}})
	disablePrefetchJitter(s)

	cacheRequest(s, http.MethodGet, "/test/a%2Fb", nil)
	task := onlyPrefetchTask(t, s)
	if task.cacheKey != "mirror://test/a%2Fb" {
		t.Fatalf("task cache key = %q", task.cacheKey)
	}
	if task.clientEscapedPath != "/test/a%2Fb" {
		t.Fatalf("task escaped path = %q", task.clientEscapedPath)
	}

	clock.Advance(60 * time.Second)
	dispatchAndWait(s)

	mu.Lock()
	gotEscaped := append([]string(nil), escaped...)
	gotRequests := append([]string(nil), requests...)
	mu.Unlock()
	if len(gotEscaped) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(gotEscaped))
	}
	if gotEscaped[1] != "/test/a%2Fb" {
		t.Fatalf("background escaped path = %q, want /test/a%%2Fb", gotEscaped[1])
	}
	if gotRequests[1] != "/test/a%2Fb" {
		t.Fatalf("background request URI = %q, want /test/a%%2Fb", gotRequests[1])
	}
}

// TestPrefetchBackgroundOversizedUnknownLength: a chunked body has no declared
// length, so the budget check cannot run before the fetch. Put then reports
// ErrOversized after reading budget+1 bytes; retrying would re-download that
// every backoff step, so the lease must stop scheduling itself.
func TestPrefetchBackgroundOversizedUnknownLength(t *testing.T) {
	const budget = 1024
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"big"`)
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set(RelayDirectiveHeader, "prefetch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("a"), budget))
		if flusher, ok := w.(http.Flusher); ok {
			// Flushing without a declared length forces chunked encoding, so the
			// relay sees ContentLength -1.
			flusher.Flush()
		}
		_, _ = w.Write(bytes.Repeat([]byte("b"), budget))
	})
	clock := newTestClock()
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, maxSize: budget, mirrors: []Mirror{testMirror()}})
	disablePrefetchJitter(s)

	first := cacheRequest(s, http.MethodGet, "/test/big", nil)
	if first.Body.Len() != 2*budget {
		t.Fatalf("client body = %d bytes, want the complete %d", first.Body.Len(), 2*budget)
	}
	variants, err := store.Variants(t.Context(), "mirror://test/big")
	if err != nil {
		t.Fatalf("list variants: %v", err)
	}
	if len(variants) != 0 {
		t.Fatalf("oversized background body was stored: %d variants", len(variants))
	}
	task := onlyPrefetchTask(t, s)
	if !task.hasNext {
		t.Fatal("the response did not schedule a refresh")
	}

	clock.Advance(60 * time.Second)
	dispatchAndWait(s)

	task = onlyPrefetchTask(t, s)
	if task.hasNext {
		t.Fatalf("oversized body kept a schedule: nextRun %v", task.nextRun)
	}
	clock.Advance(24 * time.Hour)
	dispatchAndWait(s)
	if got := up.count(); got != 2 {
		t.Fatalf("oversized body was retried: %d upstream fetches, want 2", got)
	}
}
