package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A: a HEAD that would need transcoding must obey the same prohibitions as
// GET. A client that accepts the stored coding is still served locally; only a
// client that cannot take it reaches upstream.
func TestCacheHeadTranscodingForbidden(t *testing.T) {
	digestPath := "/test/v2/library/nginx/blobs/sha256:" + strings.Repeat("c", 64)
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		headers := map[string]string{"Cache-Control": "max-age=3600"}
		switch r.URL.Path {
		case "/test/no-transform":
			headers["Cache-Control"] = "max-age=3600, no-transform"
		case "/test/digest":
			headers["Docker-Content-Digest"] = "sha256:" + strings.Repeat("a", 64)
		}
		writeResponse(w, r, http.StatusOK, "body", headers)
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

	for _, target := range []string{"/test/no-transform", "/test/digest", digestPath} {
		cacheRequest(s, http.MethodGet, target, nil)
		before := up.count()
		rec := cacheRequest(s, http.MethodHead, target, map[string]string{"Accept-Encoding": "gzip"})
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("%s HEAD = %d body %d", target, rec.Code, rec.Body.Len())
		}
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("%s HEAD delivered a forbidden coding %q", target, got)
		}
		if up.count() != before {
			t.Fatalf("%s HEAD went upstream even though identity was acceptable", target)
		}
	}
}

// B: after a stale revalidation is answered with a full 200, the client's own
// conditional headers (stripped from the upstream request) must be evaluated
// against the new response before its body is delivered.
func TestCacheRevalidation200ReevaluatesClientConditions(t *testing.T) {
	newServer := func(t *testing.T) (*Server, *recordingUpstream) {
		t.Helper()
		var mu sync.Mutex
		body, etag, freshness := "one", `"1"`, "max-age=0"
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			currentBody, currentETag, currentFreshness := body, etag, freshness
			mu.Unlock()
			writeResponse(w, r, http.StatusOK, currentBody, map[string]string{
				"ETag":          currentETag,
				"Cache-Control": currentFreshness,
			})
		})
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
		cacheRequest(s, http.MethodGet, "/test/rev", nil)
		mu.Lock()
		body, etag, freshness = "two", `"2"`, "max-age=3600"
		mu.Unlock()
		return s, up
	}

	t.Run("if-none-match matches new etag", func(t *testing.T) {
		s, up := newServer(t)
		rec := cacheRequest(s, http.MethodGet, "/test/rev", map[string]string{"If-None-Match": `"2"`})
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Fatalf("revalidated 200 with matching condition = %d body %d", rec.Code, rec.Body.Len())
		}
		if up.count() != 2 {
			t.Fatalf("upstream = %d", up.count())
		}
		// The 304 must not have written the new representation: the old stale
		// entry still forces another revalidation.
		rec = cacheRequest(s, http.MethodGet, "/test/rev", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "two" {
			t.Fatalf("post-304 GET = %d %q", rec.Code, rec.Body.String())
		}
		if up.count() != 3 {
			t.Fatalf("304 wrote new metadata for a different validator: %d", up.count())
		}
	})

	t.Run("if-match fails against new etag", func(t *testing.T) {
		s, up := newServer(t)
		rec := cacheRequest(s, http.MethodGet, "/test/rev", map[string]string{"If-Match": `"1"`})
		if rec.Code != http.StatusPreconditionFailed {
			t.Fatalf("revalidated 200 with failing If-Match = %d", rec.Code)
		}
		if up.count() != 2 {
			t.Fatalf("upstream = %d", up.count())
		}
		rec = cacheRequest(s, http.MethodGet, "/test/rev", nil)
		if rec.Body.String() != "two" || up.count() != 3 {
			t.Fatalf("412 wrote new metadata: body %q upstream %d", rec.Body.String(), up.count())
		}
	})
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (w *flushRecorder) Flush() {
	w.flushed = true
	w.ResponseRecorder.Flush()
}

// C: after the client has every byte, the response is flushed before the
// handler waits for the store, and the commit still lands.
func TestCacheMissFlushesBeforeStoreWait(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "flushed", map[string]string{"Cache-Control": "max-age=3600"})
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

	req := httptest.NewRequest(http.MethodGet, "http://relay.example.com/test/flush", nil)
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.handleRequest(rec, req)
	if rec.Body.String() != "flushed" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if !rec.flushed {
		t.Fatal("response was not flushed after the last client byte")
	}
	if rec := cacheRequest(s, http.MethodGet, "/test/flush", nil); rec.Body.String() != "flushed" || up.count() != 1 {
		t.Fatalf("flush path did not commit the variant: upstream %d", up.count())
	}
}

// E: request-side Cache-Control. no-cache/max-age=0 force revalidation,
// no-store skips lookup and write, Pragma only applies without Cache-Control,
// and inbound X-MDB-Relay is not a request cache directive.
func TestCacheRequestCacheControl(t *testing.T) {
	t.Run("no-cache forces revalidation", func(t *testing.T) {
		up, upSrv := startRevalidatingServer(t)
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
		cacheRequest(s, http.MethodGet, "/test/nc", nil)
		rec := cacheRequest(s, http.MethodGet, "/test/nc", map[string]string{"Cache-Control": "no-cache"})
		if rec.Code != http.StatusOK || rec.Body.String() != "one" {
			t.Fatalf("no-cache response = %d %q", rec.Code, rec.Body.String())
		}
		if up.count() != 2 {
			t.Fatalf("no-cache did not revalidate: %d", up.count())
		}
	})

	t.Run("max-age zero forces revalidation", func(t *testing.T) {
		up, upSrv := startRevalidatingServer(t)
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
		cacheRequest(s, http.MethodGet, "/test/max0", nil)
		cacheRequest(s, http.MethodGet, "/test/max0", map[string]string{"Cache-Control": "max-age=0"})
		if up.count() != 2 {
			t.Fatalf("max-age=0 did not revalidate: %d", up.count())
		}
	})

	t.Run("pragma only without cache-control", func(t *testing.T) {
		up, upSrv := startRevalidatingServer(t)
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
		cacheRequest(s, http.MethodGet, "/test/pragma", nil)
		cacheRequest(s, http.MethodGet, "/test/pragma", map[string]string{"Pragma": "no-cache"})
		if up.count() != 2 {
			t.Fatalf("Pragma: no-cache did not revalidate: %d", up.count())
		}
		// With a Cache-Control that does not ask for revalidation, Pragma is
		// ignored.
		cacheRequest(s, http.MethodGet, "/test/pragma", map[string]string{
			"Cache-Control": "max-age=3600",
			"Pragma":        "no-cache",
		})
		if up.count() != 2 {
			t.Fatalf("Pragma overrode Cache-Control: %d", up.count())
		}
	})

	t.Run("request no-store skips read and write", func(t *testing.T) {
		var mu sync.Mutex
		version := 1
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			current := version
			mu.Unlock()
			body := "one"
			if current == 2 {
				body = "two"
			}
			writeResponse(w, r, http.StatusOK, body, map[string]string{"Cache-Control": "max-age=3600"})
		})
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
		cacheRequest(s, http.MethodGet, "/test/req-ns", nil)
		mu.Lock()
		version = 2
		mu.Unlock()
		rec := cacheRequest(s, http.MethodGet, "/test/req-ns", map[string]string{"Cache-Control": "no-store"})
		if rec.Body.String() != "two" {
			t.Fatalf("no-store read the cache: %q", rec.Body.String())
		}
		if up.count() != 2 {
			t.Fatalf("upstream = %d", up.count())
		}
		// The no-store exchange wrote nothing, so the original entry is intact.
		rec = cacheRequest(s, http.MethodGet, "/test/req-ns", nil)
		if rec.Body.String() != "one" || up.count() != 2 {
			t.Fatalf("no-store request wrote a variant: %q upstream %d", rec.Body.String(), up.count())
		}
	})

	t.Run("inbound X-MDB-Relay is not request cache control", func(t *testing.T) {
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			writeResponse(w, r, http.StatusOK, "body", map[string]string{"Cache-Control": "max-age=3600"})
		})
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
		cacheRequest(s, http.MethodGet, "/test/relay-directive", nil)
		// Direct client: X-MDB-Relay is stripped, so it must behave like a
		// normal fresh hit.
		rec := cacheRequest(s, http.MethodGet, "/test/relay-directive", map[string]string{RelayDirectiveHeader: "no-store"})
		if rec.Body.String() != "body" || up.count() != 1 {
			t.Fatalf("inbound X-MDB-Relay acted as no-store: %q upstream %d", rec.Body.String(), up.count())
		}
	})
}

func startRevalidatingServer(t *testing.T) (*recordingUpstream, *httptest.Server) {
	t.Helper()
	return startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"one"` {
			writeResponse(w, r, http.StatusNotModified, "", map[string]string{
				"ETag":          `"one"`,
				"Cache-Control": "max-age=3600",
			})
			return
		}
		writeResponse(w, r, http.StatusOK, "one", map[string]string{
			"ETag":          `"one"`,
			"Cache-Control": "max-age=3600",
		})
	})
}

// F: a 304 that newly declares no-store/private is verified but must not
// refresh metadata; the old entry is deleted and the next request starts over.
func TestCacheRevalidation304NonStorable(t *testing.T) {
	cases := map[string]map[string]string{
		"cache-control no-store": {"Cache-Control": "max-age=60, no-store"},
		"cache-control private":  {"Cache-Control": "max-age=60, private"},
		"relay no-store":         {"Cache-Control": "max-age=60", RelayDirectiveHeader: "no-store"},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("If-None-Match") == `"1"` {
					h := map[string]string{"ETag": `"1"`}
					for k, v := range headers {
						h[k] = v
					}
					writeResponse(w, r, http.StatusNotModified, "", h)
					return
				}
				writeResponse(w, r, http.StatusOK, "one", map[string]string{
					"ETag":          `"1"`,
					"Cache-Control": "max-age=0",
				})
			})
			s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
			cacheRequest(s, http.MethodGet, "/test/rev-ns", nil)

			rec := cacheRequest(s, http.MethodGet, "/test/rev-ns", nil)
			if rec.Code != http.StatusOK || rec.Body.String() != "one" {
				t.Fatalf("verified body response = %d %q", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get(RelayDirectiveHeader); got != "" {
				t.Fatalf("X-MDB-Relay leaked: %q", got)
			}
			if up.count() != 2 {
				t.Fatalf("304 caused a second upstream fetch: %d", up.count())
			}

			// The old variant is gone, so the next request is a fresh MISS.
			rec = cacheRequest(s, http.MethodGet, "/test/rev-ns", nil)
			if rec.Code != http.StatusOK || rec.Body.String() != "one" {
				t.Fatalf("post-delete response = %d %q", rec.Code, rec.Body.String())
			}
			if up.count() != 3 {
				t.Fatalf("non-storable 304 kept metadata: %d", up.count())
			}
		})
	}
}

// G: shared-cache freshness order is s-maxage, max-age, Expires, ttl.
func TestCacheFreshnessPriority(t *testing.T) {
	clock := newTestClock()
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/test/smax":
			writeResponse(w, r, http.StatusOK, "smax", map[string]string{
				"Cache-Control": "s-maxage=1, max-age=100",
				"X-MDB-Relay":   "ttl=1000",
			})
		case "/test/max-wins":
			writeResponse(w, r, http.StatusOK, "max", map[string]string{
				"Cache-Control": "max-age=100",
				"Expires":       clock.Now().Add(-time.Hour).UTC().Format(http.TimeFormat),
				"X-MDB-Relay":   "ttl=1",
			})
		case "/test/expires-wins":
			writeResponse(w, r, http.StatusOK, "expires", map[string]string{
				"Expires":     clock.Now().Add(time.Minute).UTC().Format(http.TimeFormat),
				"X-MDB-Relay": "ttl=1",
			})
		}
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{clock: clock, mirrors: []Mirror{testMirror()}})

	cacheRequest(s, http.MethodGet, "/test/smax", nil)
	clock.Advance(2 * time.Second)
	cacheRequest(s, http.MethodGet, "/test/smax", nil)
	if up.count() != 2 {
		t.Fatalf("s-maxage did not beat max-age: %d", up.count())
	}

	cacheRequest(s, http.MethodGet, "/test/max-wins", nil)
	clock.Advance(2 * time.Second)
	cacheRequest(s, http.MethodGet, "/test/max-wins", nil)
	if up.count() != 3 {
		t.Fatalf("max-age did not beat Expires/ttl: %d", up.count())
	}

	cacheRequest(s, http.MethodGet, "/test/expires-wins", nil)
	clock.Advance(2 * time.Second)
	cacheRequest(s, http.MethodGet, "/test/expires-wins", nil)
	if up.count() != 4 {
		t.Fatalf("Expires did not beat ttl: %d", up.count())
	}
}

// Low priority: an unsupported Content-Encoding is delivered but never stored.
func TestCacheUnsupportedResponseEncodingNotStored(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "lzma")
		writeResponse(w, r, http.StatusOK, "raw", map[string]string{"Cache-Control": "max-age=3600"})
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	if rec := cacheRequest(s, http.MethodGet, "/test/lzma", nil); rec.Header().Get("Content-Encoding") != "lzma" {
		t.Fatalf("unsupported coding not delivered: %q", rec.Header().Get("Content-Encoding"))
	}
	cacheRequest(s, http.MethodGet, "/test/lzma", nil)
	if up.count() != 2 {
		t.Fatalf("unsupported coding was stored: %d", up.count())
	}
}

// Low priority: a cache MISS preserves the upstream base path's raw form when
// the inbound path carries escapes.
func TestCacheUpstreamBaseRawPath(t *testing.T) {
	for _, base := range []string{"/base", "/base%20dir"} {
		var mu sync.Mutex
		var gotURI string
		_, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			gotURI = r.RequestURI
			mu.Unlock()
			writeResponse(w, r, http.StatusOK, "body", map[string]string{"Cache-Control": "max-age=3600"})
		})
		s, _ := newCacheServer(t, upSrv.URL+base, cacheServerOptions{mirrors: []Mirror{testMirror()}})
		rec := cacheRequest(s, http.MethodGet, "/test/a%2Fb?x=1", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "body" {
			t.Fatalf("%s response = %d %q", base, rec.Code, rec.Body.String())
		}
		mu.Lock()
		got := gotURI
		mu.Unlock()
		want := base + "/test/a%2Fb?x=1"
		if got != want {
			t.Fatalf("upstream RequestURI = %q, want %q", got, want)
		}
	}
}
