package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// Response-declared immutability (ADR 0017): a response that says
// X-MDB-Relay: immutable may seed the active segment on its first MISS, even
// though no policy declared the path content-addressed. These tests pin the
// admission rules, the fallback contract (same response, one upstream request),
// and the persistent freshness a stored variant of such a response carries.

func responseImmutableMirror() Mirror { return Mirror{Name: "files", Path: "files"} }

// responseImmutableServer starts a cache server for one plain path mount. Its
// active segment budget is activeCapacity (zero = the default).
func responseImmutableServer(t *testing.T, upstream string, activeCapacity int64, policies []Policy) (*Server, *hashstore.Store) {
	t.Helper()
	return newCacheServer(t, upstream, cacheServerOptions{
		clock:          newTestClock(),
		mirrors:        []Mirror{responseImmutableMirror()},
		policies:       policies,
		activeCapacity: activeCapacity,
	})
}

func responseImmutableDirective(digest string) string {
	return "immutable, sha256=" + digest
}

// TestResponseImmutableFirstMissPublishes proves the core behaviour: the first
// MISS assembles the object, publishes it, writes the URL variant with the
// persistent lifetime, and the next request is local. The response states a zero
// lifetime to show the directive, not upstream freshness, decides.
func TestResponseImmutableFirstMissPublishes(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	clock := newTestClock()
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control":      "max-age=0, no-transform",
		RelayDirectiveHeader: responseImmutableDirective(digest),
	}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		clock:   clock,
		mirrors: []Mirror{responseImmutableMirror()},
	})
	key := "mirror://files/blob"

	first := cacheRequest(s, http.MethodGet, "/files/blob", nil)
	if first.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), body) {
		t.Fatalf("first GET = %d, %d bytes", first.Code, first.Body.Len())
	}
	if got := first.Header().Get(RelayDirectiveHeader); got != "" {
		t.Fatalf("relay directive leaked to the client: %q", got)
	}
	if up.count() != 1 {
		t.Fatalf("first GET hit upstream %d times", up.count())
	}

	variants := activeVariants(t, store, key)
	if len(variants) != 1 {
		t.Fatalf("variants = %d, want 1", len(variants))
	}
	v := variants[0]
	if v.BodySHA256 != digest {
		t.Fatalf("variant body digest = %q, want %q", v.BodySHA256, digest)
	}
	if want := clock.Now().Add(persistentVariantLifetime); !v.ExpiresAt.Equal(want) {
		t.Fatalf("variant expiry = %v, want %v", v.ExpiresAt, want)
	}
	cacheControl := v.ResponseHeaders.Get("Cache-Control")
	if !strings.Contains(cacheControl, "max-age=31536000") {
		t.Fatalf("stored Cache-Control = %q, want the persistent lifetime", cacheControl)
	}
	if !strings.Contains(cacheControl, "no-transform") {
		t.Fatalf("stored Cache-Control lost an orthogonal directive: %q", cacheControl)
	}
	if _, ok, err := store.Stat(context.Background(), hashstore.SHA256, digest); err != nil || !ok {
		t.Fatalf("published body stat: ok=%v err=%v", ok, err)
	}

	second := cacheRequest(s, http.MethodGet, "/files/blob", nil)
	if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), body) {
		t.Fatalf("second GET = %d, %d bytes", second.Code, second.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("published body was not local: %d upstream requests", up.count())
	}
	if got := second.Header().Get("Cache-Control"); !strings.Contains(got, "max-age=31536000") {
		t.Fatalf("hit Cache-Control = %q, want the persistent lifetime", got)
	}

	head := cacheRequest(s, http.MethodHead, "/files/blob", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD = %d, %d bytes", head.Code, head.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("HEAD after publication hit upstream: %d", up.count())
	}
}

// TestResponseImmutableRangeSeedAssembles covers the 206 admission: a range
// request that covers the whole object seeds the entry, publishes it and writes
// the URL variant. A 206 digest is never an Adopt expectation, so a wrong one
// cannot block publication.
func TestResponseImmutableRangeSeedAssembles(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	wrong := bodyDigestHex("not-the-body")
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		RelayDirectiveHeader: responseImmutableDirective(wrong),
	}))
	s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
	key := "mirror://files/blob"

	first := cacheRequest(s, http.MethodGet, "/files/blob", map[string]string{
		"Range": fmt.Sprintf("bytes=0-%d", len(body)-1),
	})
	if first.Code != http.StatusPartialContent || !bytes.Equal(first.Body.Bytes(), body) {
		t.Fatalf("range seed = %d, %d bytes", first.Code, first.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("range seed hit upstream %d times", up.count())
	}
	if got := len(activeVariants(t, store, key)); got != 1 {
		t.Fatalf("variants after a whole-object range = %d, want 1", got)
	}
	if _, ok, err := store.Stat(context.Background(), hashstore.SHA256, digest); err != nil || !ok {
		t.Fatalf("range-seeded body stat: ok=%v err=%v", ok, err)
	}

	again := cacheRequest(s, http.MethodGet, "/files/blob", nil)
	if again.Code != http.StatusOK || !bytes.Equal(again.Body.Bytes(), body) {
		t.Fatalf("local GET = %d, %d bytes", again.Code, again.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("published range seed was not local: %d", up.count())
	}
}

// TestResponseImmutablePartialRangeSeed documents the limit of a 206 seed: only
// the range it answered is cached, so it publishes nothing, and a later full
// request is stored through the ordinary chain instead of joining it.
func TestResponseImmutablePartialRangeSeed(t *testing.T) {
	body := activeBody()
	wrong := bodyDigestHex("not-the-body")
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		RelayDirectiveHeader: responseImmutableDirective(wrong),
	}))
	s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
	key := "mirror://files/blob"

	first := cacheRequest(s, http.MethodGet, "/files/blob", map[string]string{
		"Range": "bytes=1000-1999",
	})
	if first.Code != http.StatusPartialContent || !bytes.Equal(first.Body.Bytes(), body[1000:2000]) {
		t.Fatalf("partial seed = %d, %d bytes", first.Code, first.Body.Len())
	}
	if got := len(activeVariants(t, store, key)); got != 0 {
		t.Fatalf("partial seed published %d variants", got)
	}

	full := cacheRequest(s, http.MethodGet, "/files/blob", nil)
	if full.Code != http.StatusOK || !bytes.Equal(full.Body.Bytes(), body) {
		t.Fatalf("full GET = %d, %d bytes", full.Code, full.Body.Len())
	}
	if up.count() != 2 {
		t.Fatalf("upstream = %d, want the partial seed plus one ordinary fetch", up.count())
	}
	if got := len(activeVariants(t, store, key)); got != 1 {
		t.Fatalf("ordinary variant after the full GET = %d, want 1", got)
	}
	if again := cacheRequest(s, http.MethodGet, "/files/blob", nil); again.Code != http.StatusOK || up.count() != 2 {
		t.Fatalf("ordinary variant was not local: %d upstream requests", up.count())
	}
}

// TestResponseImmutableExpectation checks both digest algorithms and the mismatch
// contract: the client body is unaffected, nothing is published, and the next
// request is a fresh MISS.
func TestResponseImmutableExpectation(t *testing.T) {
	body := activeBody()

	t.Run("sha256 match", func(t *testing.T) {
		digest := bodyDigestHex(string(body))
		_, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			RelayDirectiveHeader: responseImmutableDirective(digest),
		}))
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		rec := cacheRequest(s, http.MethodGet, "/files/blob", nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("GET = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if got := len(activeVariants(t, store, "mirror://files/blob")); got != 1 {
			t.Fatalf("variants = %d, want 1", got)
		}
	})

	t.Run("sha1 match", func(t *testing.T) {
		sum := activeSHA1(body)
		_, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			RelayDirectiveHeader: "immutable, sha1=" + sum,
		}))
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		rec := cacheRequest(s, http.MethodGet, "/files/blob", nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("GET = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if got := len(activeVariants(t, store, "mirror://files/blob")); got != 1 {
			t.Fatalf("variants = %d, want 1", got)
		}
	})

	t.Run("sha256 mismatch publishes nothing", func(t *testing.T) {
		wrong := bodyDigestHex("not-the-body")
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			RelayDirectiveHeader: responseImmutableDirective(wrong),
		}))
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		rec := cacheRequest(s, http.MethodGet, "/files/blob", nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("mismatch GET = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if got := len(activeVariants(t, store, "mirror://files/blob")); got != 0 {
			t.Fatalf("mismatched body produced %d variants", got)
		}
		if _, ok, err := store.Stat(context.Background(), hashstore.SHA256, bodyDigestHex(string(body))); err != nil || ok {
			t.Fatalf("mismatched body was published: ok=%v err=%v", ok, err)
		}
		second := cacheRequest(s, http.MethodGet, "/files/blob", nil)
		if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), body) {
			t.Fatalf("second GET = %d, %d bytes", second.Code, second.Body.Len())
		}
		if up.count() != 2 {
			t.Fatalf("unpublished body was answered locally: %d upstream requests", up.count())
		}
	})
}

// TestResponseImmutablePolicyPrecedence keeps the pre-request decision: a policy
// that already captured a digest keeps its own key and expectation, whatever the
// response declares.
func TestResponseImmutablePolicyPrecedence(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	wrong := bodyDigestHex("not-the-body")
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		RelayDirectiveHeader: responseImmutableDirective(wrong),
	}))
	policies := []Policy{{
		Name:  "immutable-digest",
		Rules: PolicyRules{Immutable: stringRule(`^/blob/(?<sha256>[0-9a-f]{64})$`)},
	}}
	s, store := responseImmutableServer(t, upSrv.URL, 0, policies)

	target := "/files/blob/" + digest
	key := "mirror://files/blob/" + digest
	rec := cacheRequest(s, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("GET = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("policy seed hit upstream %d times", up.count())
	}
	if got := len(activeVariants(t, store, key)); got != 1 {
		t.Fatalf("variants = %d, want 1 (the response digest must not override policy)", got)
	}
	again := cacheRequest(s, http.MethodGet, target, nil)
	if again.Code != http.StatusOK || !bytes.Equal(again.Body.Bytes(), body) {
		t.Fatalf("second GET = %d, %d bytes", again.Code, again.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("policy-published body was not local: %d", up.count())
	}
}

// TestResponseImmutableRefusals covers every response fact that must keep the
// ordinary chain. A wrong digest distinguishes the two paths: only the ordinary
// path stores a variant.
func TestResponseImmutableRefusals(t *testing.T) {
	body := activeBody()
	wrong := bodyDigestHex("not-the-body")
	directive := responseImmutableDirective(wrong)

	t.Run("no-store", func(t *testing.T) {
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			"Cache-Control":      "no-store",
			RelayDirectiveHeader: directive,
		}))
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		rec := cacheRequest(s, http.MethodGet, "/files/blob", nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("GET = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if up.count() != 1 {
			t.Fatalf("upstream = %d, want 1", up.count())
		}
		if got := len(activeVariants(t, store, "mirror://files/blob")); got != 0 {
			t.Fatalf("no-store response left %d variants", got)
		}
		if second := cacheRequest(s, http.MethodGet, "/files/blob", nil); second.Code != http.StatusOK {
			t.Fatalf("second GET = %d", second.Code)
		}
		if up.count() != 2 {
			t.Fatalf("no-store response was cached: %d upstream requests", up.count())
		}
	})

	t.Run("private", func(t *testing.T) {
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			"Cache-Control":      "private, max-age=600",
			RelayDirectiveHeader: directive,
		}))
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		cacheRequest(s, http.MethodGet, "/files/blob", nil)
		if up.count() != 1 {
			t.Fatalf("upstream = %d, want 1", up.count())
		}
		if got := len(activeVariants(t, store, "mirror://files/blob")); got != 0 {
			t.Fatalf("private response left %d variants", got)
		}
	})

	t.Run("gzip representation", func(t *testing.T) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(body); err != nil {
			t.Fatalf("gzip: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
		gzipped := buf.Bytes()
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(gzipped)))
			w.Header().Set(RelayDirectiveHeader, directive)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(gzipped)
		})
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		rec := cacheRequest(s, http.MethodGet, "/files/blob", map[string]string{
			"Accept-Encoding": "gzip",
		})
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), gzipped) {
			t.Fatalf("gzip GET = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if up.count() != 1 {
			t.Fatalf("upstream = %d, want 1", up.count())
		}
		// The ordinary chain stored the gzip representation; the active segment
		// must not have adopted it as an identity object.
		v := firstVariant(t, store, "mirror://files/blob")
		if v.ResponseHeaders.Get("Content-Encoding") != "gzip" {
			t.Fatalf("stored coding = %q, want gzip (ordinary path)", v.ResponseHeaders.Get("Content-Encoding"))
		}
	})

	t.Run("dangerous vary", func(t *testing.T) {
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			"Vary":               "Accept-Language",
			RelayDirectiveHeader: directive,
		}))
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		headers := map[string]string{"Accept-Language": "en"}
		rec := cacheRequest(s, http.MethodGet, "/files/blob", headers)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("GET = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if up.count() != 1 {
			t.Fatalf("upstream = %d, want 1", up.count())
		}
		if got := len(activeVariants(t, store, "mirror://files/blob")); got != 1 {
			t.Fatalf("variants = %d, want the ordinary varied variant", got)
		}
		// The ordinary variant identity still discriminates.
		cacheRequest(s, http.MethodGet, "/files/blob", headers)
		if up.count() != 1 {
			t.Fatalf("same variant was not local: %d", up.count())
		}
		cacheRequest(s, http.MethodGet, "/files/blob", map[string]string{"Accept-Language": "fr"})
		if up.count() != 2 {
			t.Fatalf("different variant reused the stored one: %d", up.count())
		}
	})

	t.Run("unknown length", func(t *testing.T) {
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(RelayDirectiveHeader, directive)
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			_, _ = w.Write(body)
		})
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		rec := cacheRequest(s, http.MethodGet, "/files/blob", nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("GET = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if up.count() != 1 {
			t.Fatalf("upstream = %d, want 1", up.count())
		}
		if got := len(activeVariants(t, store, "mirror://files/blob")); got != 1 {
			t.Fatalf("variants = %d, want the ordinary stored variant", got)
		}
	})

	t.Run("range answered with a full 200", func(t *testing.T) {
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Header().Set(RelayDirectiveHeader, directive)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		})
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		rec := cacheRequest(s, http.MethodGet, "/files/blob", map[string]string{
			"Range": "bytes=0-99",
		})
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("ranged GET = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if up.count() != 1 {
			t.Fatalf("upstream = %d, want 1", up.count())
		}
		if got := len(activeVariants(t, store, "mirror://files/blob")); got != 1 {
			t.Fatalf("variants = %d, want the ordinary full-body variant", got)
		}
	})

	t.Run("head never seeds", func(t *testing.T) {
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			RelayDirectiveHeader: directive,
		}))
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		rec := cacheRequest(s, http.MethodHead, "/files/blob", nil)
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("HEAD = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if up.count() != 1 {
			t.Fatalf("HEAD hit upstream %d times", up.count())
		}
		if got := len(activeVariants(t, store, "mirror://files/blob")); got != 0 {
			t.Fatalf("HEAD created %d variants", got)
		}
	})

	t.Run("conditional request", func(t *testing.T) {
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			RelayDirectiveHeader: directive,
		}))
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		rec := cacheRequest(s, http.MethodGet, "/files/blob", map[string]string{
			"If-None-Match": `"other"`,
		})
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("conditional GET = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if up.count() != 1 {
			t.Fatalf("upstream = %d, want 1", up.count())
		}
		if got := len(activeVariants(t, store, "mirror://files/blob")); got != 1 {
			t.Fatalf("variants = %d, want the ordinary stored variant", got)
		}
	})

	t.Run("client no-store", func(t *testing.T) {
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			RelayDirectiveHeader: directive,
		}))
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		rec := cacheRequest(s, http.MethodGet, "/files/blob", map[string]string{
			"Cache-Control": "no-store",
		})
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("client no-store GET = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if up.count() != 1 {
			t.Fatalf("upstream = %d, want 1", up.count())
		}
		if got := len(activeVariants(t, store, "mirror://files/blob")); got != 0 {
			t.Fatalf("client no-store left %d variants", got)
		}
	})

	t.Run("client forced revalidation", func(t *testing.T) {
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			RelayDirectiveHeader: directive,
		}))
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		rec := cacheRequest(s, http.MethodGet, "/files/blob", map[string]string{
			"Cache-Control": "no-cache",
		})
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("no-cache GET = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if up.count() != 1 {
			t.Fatalf("upstream = %d, want 1", up.count())
		}
		if got := len(activeVariants(t, store, "mirror://files/blob")); got != 1 {
			t.Fatalf("variants = %d, want the ordinary stored variant", got)
		}
	})

	t.Run("stale variant", func(t *testing.T) {
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			RelayDirectiveHeader: directive,
		}))
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		old := putStaleVariant(t, store, "mirror://files/blob", string(body), s.currentTime())
		rec := cacheRequest(s, http.MethodGet, "/files/blob", map[string]string{
			"Range": "bytes=100-199",
		})
		if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), body[100:200]) {
			t.Fatalf("ranged GET = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if up.count() != 1 {
			t.Fatalf("upstream = %d, want 1", up.count())
		}
		variants := activeVariants(t, store, "mirror://files/blob")
		if len(variants) != 1 || variants[0].BodySHA256 != old.BodySHA256 {
			t.Fatalf("stale variant was replaced: %+v", variants)
		}
	})
}

// TestResponseImmutableCapacityFallback proves an admission refusal delivers the
// already-fetched response exactly once, stores it through the ordinary chain,
// and does not re-enter the active path.
func TestResponseImmutableCapacityFallback(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control":      "max-age=30",
		RelayDirectiveHeader: responseImmutableDirective(digest),
	}))
	// A 1 KiB segment cannot admit a 4 KiB body; the store still can.
	s, store := responseImmutableServer(t, upSrv.URL, 1<<10, nil)

	first := cacheRequest(s, http.MethodGet, "/files/blob", nil)
	if first.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), body) {
		t.Fatalf("fallback GET = %d, %d bytes", first.Code, first.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("fallback fetched upstream %d times, want 1", up.count())
	}
	v := firstVariant(t, store, "mirror://files/blob")
	if v.BodySHA256 != digest {
		t.Fatalf("stored digest = %q, want %q", v.BodySHA256, digest)
	}
	if want := s.currentTime().Add(persistentVariantLifetime); !v.ExpiresAt.Equal(want) {
		t.Fatalf("ordinary fallback expiry = %v, want %v", v.ExpiresAt, want)
	}

	second := cacheRequest(s, http.MethodGet, "/files/blob", nil)
	if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), body) {
		t.Fatalf("second GET = %d, %d bytes", second.Code, second.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("fallback-stored variant was not local: %d", up.count())
	}

	// Even a persistent lifetime stays a shared-cache lifetime: a client that
	// demands revalidation still reaches upstream.
	third := cacheRequest(s, http.MethodGet, "/files/blob", map[string]string{
		"Cache-Control": "no-cache",
	})
	if third.Code != http.StatusOK || !bytes.Equal(third.Body.Bytes(), body) {
		t.Fatalf("forced revalidation GET = %d, %d bytes", third.Code, third.Body.Len())
	}
	if up.count() != 2 {
		t.Fatalf("client no-cache was answered locally: %d upstream requests", up.count())
	}
}

// TestResponseImmutableDigestAloneIsNotImmutable keeps the directive boundary: a
// digest without the immutable keyword is not a reason to assemble, so a wrong
// digest cannot block the ordinary store.
func TestResponseImmutableDigestAloneIsNotImmutable(t *testing.T) {
	body := activeBody()
	wrong := bodyDigestHex("not-the-body")
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		RelayDirectiveHeader: "sha256=" + wrong,
	}))
	s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
	rec := cacheRequest(s, http.MethodGet, "/files/blob", nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("GET = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if got := len(activeVariants(t, store, "mirror://files/blob")); got != 1 {
		t.Fatalf("variants = %d, want the ordinary stored variant", got)
	}
	if up.count() != 1 {
		t.Fatalf("upstream = %d, want 1", up.count())
	}
}

// TestResponseImmutableAmbiguousVariant covers the ambiguity guard: a re-fetch
// that is cleaning up several variant identities must not also seed an assembly.
func TestResponseImmutableAmbiguousVariant(t *testing.T) {
	body := activeBody()
	wrong := bodyDigestHex("not-the-body")
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Vary":               "Accept-Language",
		RelayDirectiveHeader: responseImmutableDirective(wrong),
	}))
	s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
	content, err := store.Put(context.Background(), strings.NewReader(string(body)), int64(len(body)))
	if err != nil {
		t.Fatalf("put body: %v", err)
	}
	// Two identities that both match the same request: one no-vary and one
	// varied, which is exactly the ambiguity a re-fetch must resolve.
	for _, v := range []hashstore.Variant{
		{
			URL:             "mirror://files/blob",
			NoVary:          true,
			ResponseHeaders: http.Header{"Cache-Control": {"max-age=0"}},
			StoredAt:        s.currentTime(),
			ExpiresAt:       s.currentTime().Add(-time.Hour),
		},
		{
			URL:             "mirror://files/blob",
			Vary:            []string{"accept-language"},
			RequestHeaders:  http.Header{"Accept-Language": {"en"}},
			ResponseHeaders: http.Header{"Vary": {"Accept-Language"}, "Cache-Control": {"max-age=0"}},
			StoredAt:        s.currentTime(),
			ExpiresAt:       s.currentTime().Add(-time.Hour),
		},
	} {
		v.BodySHA256 = content.SHA256
		v.BodySize = content.Size
		if err := store.PutVariant(context.Background(), v); err != nil {
			t.Fatalf("put variant: %v", err)
		}
	}
	if got := len(activeVariants(t, store, "mirror://files/blob")); got != 2 {
		t.Fatalf("setup variants = %d, want 2", got)
	}

	rec := cacheRequest(s, http.MethodGet, "/files/blob", map[string]string{"Accept-Language": "en"})
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("ambiguous GET = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("upstream = %d, want 1", up.count())
	}
	// The response was stored by the ordinary cleanup path, so the wrong digest
	// never reached Adopt.
	variants := activeVariants(t, store, "mirror://files/blob")
	if len(variants) != 1 {
		t.Fatalf("variants after ambiguity cleanup = %d, want 1", len(variants))
	}
	if variants[0].BodySHA256 != bodyDigestHex(string(body)) {
		t.Fatalf("cleanup stored %q", variants[0].BodySHA256)
	}
}

// TestResponseImmutablePrefetchObservedOnce checks the response is observed
// exactly once for prefetch when it seeds the active segment, and that the
// observation still reaches the lease table.
func TestResponseImmutablePrefetchObservedOnce(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control":      "max-age=60",
		RelayDirectiveHeader: "immutable, prefetch, sha256=" + digest,
	}))
	s, _ := responseImmutableServer(t, upSrv.URL, 0, nil)
	disablePrefetchJitter(s)
	obs := &countingObservation{next: prefetchObservation{s}}
	s.observer = obs

	rec := cacheRequest(s, http.MethodGet, "/files/blob", nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("GET = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if got := obs.headerETags(); len(got) != 1 {
		t.Fatalf("observations = %d, want exactly 1", len(got))
	}
	if headers, revalidated := obs.counts(); headers != 1 || revalidated != 0 {
		t.Fatalf("observations = %d headers / %d revalidated, want 1/0", headers, revalidated)
	}
	task := onlyPrefetchTask(t, s)
	if task.cacheKey != "mirror://files/blob" {
		t.Fatalf("prefetch task key = %q", task.cacheKey)
	}
	if task.mirror.Name != "files" {
		t.Fatalf("prefetch task mirror = %q", task.mirror.Name)
	}
	if up.count() != 1 {
		t.Fatalf("upstream = %d, want 1", up.count())
	}
}

// TestResponseImmutableDeliveryHeaders keeps delivery and storage apart: the
// first client always sees the upstream Cache-Control, whether the object was
// assembled in the active segment or stored by the ordinary tee. Only the stored
// variant carries the persistent lifetime, which a later hit advertises.
func TestResponseImmutableDeliveryHeaders(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	const upstreamCacheControl = "max-age=0, no-transform"

	run := func(t *testing.T, activeCapacity int64) (firstCacheControl string) {
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			"Cache-Control":      upstreamCacheControl,
			RelayDirectiveHeader: responseImmutableDirective(digest),
		}))
		s, store := responseImmutableServer(t, upSrv.URL, activeCapacity, nil)

		first := cacheRequest(s, http.MethodGet, "/files/blob", nil)
		if first.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), body) {
			t.Fatalf("first GET = %d, %d bytes", first.Code, first.Body.Len())
		}
		if got := first.Header().Get("Cache-Control"); got != upstreamCacheControl {
			t.Fatalf("first response Cache-Control = %q, want the upstream value %q", got, upstreamCacheControl)
		}
		if up.count() != 1 {
			t.Fatalf("upstream = %d, want 1", up.count())
		}

		v := firstVariant(t, store, "mirror://files/blob")
		if !v.Persistent {
			t.Fatal("stored variant is not marked persistent")
		}
		if got := v.ResponseHeaders.Get("Cache-Control"); !strings.Contains(got, "max-age=31536000") || !strings.Contains(got, "no-transform") {
			t.Fatalf("stored Cache-Control = %q", got)
		}
		if want := s.currentTime().Add(persistentVariantLifetime); !v.ExpiresAt.Equal(want) {
			t.Fatalf("stored expiry = %v, want %v", v.ExpiresAt, want)
		}

		second := cacheRequest(s, http.MethodGet, "/files/blob", nil)
		if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), body) {
			t.Fatalf("second GET = %d, %d bytes", second.Code, second.Body.Len())
		}
		if got := second.Header().Get("Cache-Control"); !strings.Contains(got, "max-age=31536000") {
			t.Fatalf("hit Cache-Control = %q, want the persistent lifetime", got)
		}
		if up.count() != 1 {
			t.Fatalf("second GET hit upstream: %d", up.count())
		}
		return first.Header().Get("Cache-Control")
	}

	active := run(t, 0)
	ordinary := run(t, 1<<10)
	if active != ordinary {
		t.Fatalf("first-response Cache-Control differs by path: active %q, ordinary %q", active, ordinary)
	}
}

// TestResponseImmutablePersistentAcross304 proves the response-declared
// persistence is variant metadata, not a header coincidence: a forced
// revalidation answered by a 304 with max-age=0 cannot make the variant stale,
// and the 304 need not repeat X-MDB-Relay. The ordinary (capacity fallback) and
// active storage paths are both covered.
func TestResponseImmutablePersistentAcross304(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))

	cases := []struct {
		name           string
		activeCapacity int64
	}{
		{"active variant", 0},
		{"ordinary variant", 1 << 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"v1"`)
				if r.Header.Get("If-None-Match") == `"v1"` {
					// The 304 deliberately states a zero lifetime and omits the
					// relay directive.
					w.Header().Set("Cache-Control", "max-age=0")
					w.WriteHeader(http.StatusNotModified)
					return
				}
				w.Header().Set("Cache-Control", "max-age=0")
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.Header().Set(RelayDirectiveHeader, responseImmutableDirective(digest))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(body)
			})
			s, store := responseImmutableServer(t, upSrv.URL, tc.activeCapacity, nil)
			key := "mirror://files/blob"

			if rec := cacheRequest(s, http.MethodGet, "/files/blob", nil); rec.Code != http.StatusOK {
				t.Fatalf("first GET = %d", rec.Code)
			}
			if got := len(activeVariants(t, store, key)); got != 1 {
				t.Fatalf("variants after first GET = %d", got)
			}

			rec := cacheRequest(s, http.MethodGet, "/files/blob", map[string]string{
				"Cache-Control": "no-cache",
			})
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
				t.Fatalf("forced revalidation = %d, %d bytes", rec.Code, rec.Body.Len())
			}
			if up.count() != 2 {
				t.Fatalf("forced revalidation did not reach upstream: %d", up.count())
			}

			v := firstVariant(t, store, key)
			if !v.Persistent {
				t.Fatal("304 dropped the persistent property")
			}
			if want := s.currentTime().Add(persistentVariantLifetime); !v.ExpiresAt.Equal(want) {
				t.Fatalf("expiry after 304 = %v, want %v", v.ExpiresAt, want)
			}
			if got := v.ResponseHeaders.Get("Cache-Control"); !strings.Contains(got, "max-age=31536000") {
				t.Fatalf("stored Cache-Control after 304 = %q", got)
			}

			before := up.count()
			local := cacheRequest(s, http.MethodGet, "/files/blob", nil)
			if local.Code != http.StatusOK || !bytes.Equal(local.Body.Bytes(), body) {
				t.Fatalf("local GET = %d, %d bytes", local.Code, local.Body.Len())
			}
			if up.count() != before {
				t.Fatalf("variant was stale after the 304: %d -> %d", before, up.count())
			}
		})
	}
}

// TestResponseImmutablePrefetchScheduling checks that a response declaring both
// prefetch and immutability is scheduled at the persistent lifetime, not at its
// stated max-age=0, while no-store still disables background writes.
func TestResponseImmutablePrefetchScheduling(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))

	t.Run("immutable schedule uses the persistent lifetime", func(t *testing.T) {
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			"Cache-Control":      "max-age=0",
			RelayDirectiveHeader: "immutable, prefetch, sha256=" + digest,
		}))
		s, _ := responseImmutableServer(t, upSrv.URL, 0, nil)
		disablePrefetchJitter(s)

		if rec := cacheRequest(s, http.MethodGet, "/files/blob", nil); rec.Code != http.StatusOK {
			t.Fatalf("GET = %d", rec.Code)
		}
		if up.count() != 1 {
			t.Fatalf("upstream = %d, want 1", up.count())
		}
		task := onlyPrefetchTask(t, s)
		if !task.hasNext {
			t.Fatal("max-age=0 silently disabled the refresh")
		}
		if want := s.currentTime().Add(persistentVariantLifetime); !task.nextRun.Equal(want) {
			t.Fatalf("nextRun = %v, want %v", task.nextRun, want)
		}
	})

	t.Run("no-store still disables background writes", func(t *testing.T) {
		_, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			"Cache-Control":      "no-store",
			RelayDirectiveHeader: "immutable, prefetch, sha256=" + digest,
		}))
		s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
		disablePrefetchJitter(s)

		if rec := cacheRequest(s, http.MethodGet, "/files/blob", nil); rec.Code != http.StatusOK {
			t.Fatalf("GET = %d", rec.Code)
		}
		if got := len(activeVariants(t, store, "mirror://files/blob")); got != 0 {
			t.Fatalf("no-store response left %d variants", got)
		}
		task := onlyPrefetchTask(t, s)
		if !task.backgroundDisabled {
			t.Fatal("no-store did not disable background writes")
		}
		if task.hasNext {
			t.Fatal("no-store response kept a scheduled refresh")
		}
	})
}

// TestResponseImmutableConcurrentMissDoesNotJoin pins the created==false
// contract: a second cold request that already holds a response is delivered
// ordinarily instead of waiting for the first assembly. Both clients receive the
// complete body, upstream is asked twice, and one variant remains.
func TestResponseImmutableConcurrentMissDoesNotJoin(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce, releaseOnce sync.Once

	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set(RelayDirectiveHeader, responseImmutableDirective(digest))
		w.WriteHeader(http.StatusOK)
		if n != 1 {
			_, _ = w.Write(body)
			return
		}
		// The first response stalls half-way, so its entry exists but its body is
		// still incomplete while the second request runs.
		_, _ = w.Write(body[:len(body)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		startOnce.Do(func() { close(started) })
		<-release
		_, _ = w.Write(body[len(body)/2:])
	})
	s, store := responseImmutableServer(t, upSrv.URL, 0, nil)
	key := "mirror://files/blob"

	firstCh := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstCh <- cacheRequest(s, http.MethodGet, "/files/blob", nil) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the upstream")
	}
	// Give the first request time to acquire its active entry; it acquires
	// immediately after the response headers arrive.
	time.Sleep(100 * time.Millisecond)

	secondCh := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondCh <- cacheRequest(s, http.MethodGet, "/files/blob", nil) }()
	select {
	case second := <-secondCh:
		if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), body) {
			releaseOnce.Do(func() { close(release) })
			t.Fatalf("second GET = %d, %d bytes", second.Code, second.Body.Len())
		}
	case <-time.After(5 * time.Second):
		releaseOnce.Do(func() { close(release) })
		t.Fatal("second request blocked on the first assembly: it joined")
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case first := <-firstCh:
		if first.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), body) {
			t.Fatalf("first GET = %d, %d bytes", first.Code, first.Body.Len())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not finish after its body was released")
	}

	if up.count() != 2 {
		t.Fatalf("upstream requests = %d, want one per client", up.count())
	}
	variants := activeVariants(t, store, key)
	if len(variants) != 1 {
		t.Fatalf("variants = %d, want 1 (no ambiguous pair)", len(variants))
	}
	if variants[0].BodySHA256 != digest {
		t.Fatalf("variant body = %q, want %q", variants[0].BodySHA256, digest)
	}
}

// TestResponseImmutableContentRangeLengthMismatch keeps a malformed 206 on the
// ordinary path: the adapter would close the body while rejecting it, so the
// eligibility check must refuse it first and the same response is delivered.
func TestResponseImmutableContentRangeLengthMismatch(t *testing.T) {
	body := activeBody()
	wrong := bodyDigestHex("not-the-body")
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// A 100-byte interval announced with a 10-byte body: the adapter rejects
		// this, so the relay must not hand it the response.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-99/%d", len(body)))
		w.Header().Set("Content-Length", "10")
		w.Header().Set(RelayDirectiveHeader, responseImmutableDirective(wrong))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[:10])
	})
	s, store := responseImmutableServer(t, upSrv.URL, 0, nil)

	rec := cacheRequest(s, http.MethodGet, "/files/blob", map[string]string{
		"Range": "bytes=0-99",
	})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("mismatched 206 = %d, want the upstream status", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), body[:10]) {
		t.Fatalf("mismatched 206 body = %d bytes, want 10", rec.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("upstream = %d, want 1", up.count())
	}
	if got := len(activeVariants(t, store, "mirror://files/blob")); got != 0 {
		t.Fatalf("mismatched 206 produced %d variants", got)
	}
}
