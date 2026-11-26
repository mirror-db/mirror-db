package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

const digestTestBody = "0123456789abcdefghijklmnopqrstuvwxyz"

// bodyDigestHex is the SHA-256 the store records for body.
func bodyDigestHex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// digestTestMirror is a path mount shaped like the docker mirror, so an OCI
// request path survives into the cache key unchanged.
func digestTestMirror() Mirror { return Mirror{Name: "docker", Path: "docker"} }

// digestUpstreamServer starts an upstream that answers any request with a full
// 200, so a request that reaches it is visible as a hit regardless of method.
func digestUpstreamServer(t *testing.T) (*recordingUpstream, *Server, *hashstore.Store) {
	t.Helper()
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "from-upstream", map[string]string{"Cache-Control": "max-age=3600"})
	})
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		clock:   newTestClock(),
		mirrors: []Mirror{digestTestMirror()},
	})
	return up, s, store
}

// putStaleVariant stores body under key with metadata that has already expired,
// the state a long-lived layer reaches after its freshness window.
func putStaleVariant(t *testing.T, store *hashstore.Store, key, body string, now time.Time) hashstore.Variant {
	t.Helper()
	content, err := store.Put(context.Background(), strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("put body: %v", err)
	}
	v := hashstore.Variant{
		URL:    key,
		NoVary: true,
		ResponseHeaders: http.Header{
			"Etag":          {`"blob-v1"`},
			"Cache-Control": {"public, max-age=31536000, immutable"},
			"Content-Type":  {"application/octet-stream"},
		},
		BodySHA256: content.SHA256,
		BodySize:   content.Size,
		StoredAt:   now.Add(-48 * time.Hour),
		ExpiresAt:  now.Add(-24 * time.Hour),
	}
	if err := store.PutVariant(context.Background(), v); err != nil {
		t.Fatalf("put variant: %v", err)
	}
	return v
}

func TestCacheStaleDigestHit(t *testing.T) {
	blobPath := "/v2/library/nginx/blobs/sha256:"
	setup := func(t *testing.T) (*recordingUpstream, *Server, *hashstore.Store, string) {
		t.Helper()
		up, s, store := digestUpstreamServer(t)
		digest := bodyDigestHex(digestTestBody)
		putStaleVariant(t, store, "mirror://docker"+blobPath+digest, digestTestBody, s.currentTime())
		return up, s, store, "/docker" + blobPath + digest
	}

	t.Run("GET and HEAD are served locally", func(t *testing.T) {
		up, s, _, target := setup(t)

		rec := cacheRequest(s, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != digestTestBody {
			t.Fatalf("GET = %d %q", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Age") == "" {
			t.Fatal("GET delivered no Age")
		}
		if rec.Header().Get("ETag") != `"blob-v1"` {
			t.Fatalf("ETag = %q", rec.Header().Get("ETag"))
		}

		head := cacheRequest(s, http.MethodHead, target, nil)
		if head.Code != http.StatusOK || head.Body.Len() != 0 {
			t.Fatalf("HEAD = %d body %d", head.Code, head.Body.Len())
		}
		if got := head.Header().Get("Content-Length"); got != strconv.Itoa(len(digestTestBody)) {
			t.Fatalf("HEAD Content-Length = %q", got)
		}
		if up.count() != 0 {
			t.Fatalf("stale digest GET/HEAD hit the upstream: %d", up.count())
		}
	})

	t.Run("range is served locally", func(t *testing.T) {
		up, s, _, target := setup(t)

		rec := cacheRequest(s, http.MethodGet, target, map[string]string{"Range": "bytes=0-0"})
		if rec.Code != http.StatusPartialContent || rec.Body.String() != "0" {
			t.Fatalf("range = %d %q", rec.Code, rec.Body.String())
		}
		want := fmt.Sprintf("bytes 0-0/%d", len(digestTestBody))
		if got := rec.Header().Get("Content-Range"); got != want {
			t.Fatalf("Content-Range = %q, want %q", got, want)
		}
		if up.count() != 0 {
			t.Fatalf("stale digest range hit the upstream: %d", up.count())
		}
	})

	t.Run("conditional requests are evaluated locally", func(t *testing.T) {
		up, s, _, target := setup(t)

		notModified := cacheRequest(s, http.MethodGet, target, map[string]string{"If-None-Match": `"blob-v1"`})
		if notModified.Code != http.StatusNotModified {
			t.Fatalf("If-None-Match = %d", notModified.Code)
		}
		failed := cacheRequest(s, http.MethodGet, target, map[string]string{"If-Match": `"other"`})
		if failed.Code != http.StatusPreconditionFailed {
			t.Fatalf("If-Match = %d", failed.Code)
		}
		full := cacheRequest(s, http.MethodGet, target, map[string]string{"If-None-Match": `"other"`})
		if full.Code != http.StatusOK || full.Body.String() != digestTestBody {
			t.Fatalf("non-matching If-None-Match = %d %q", full.Code, full.Body.String())
		}
		if up.count() != 0 {
			t.Fatalf("conditional requests hit the upstream: %d", up.count())
		}
	})

	t.Run("uppercase digest in the path matches the stored lowercase body", func(t *testing.T) {
		up, s, store := digestUpstreamServer(t)
		upper := strings.ToUpper(bodyDigestHex(digestTestBody))
		path := blobPath + upper
		putStaleVariant(t, store, "mirror://docker"+path, digestTestBody, s.currentTime())

		rec := cacheRequest(s, http.MethodGet, "/docker"+path, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != digestTestBody || up.count() != 0 {
			t.Fatalf("uppercase digest = %d %q, upstream %d", rec.Code, rec.Body.String(), up.count())
		}
	})

	t.Run("path digest mismatch falls back", func(t *testing.T) {
		up, s, _, _ := setup(t)
		other := "/docker" + blobPath + strings.Repeat("a", 64)
		rec := cacheRequest(s, http.MethodGet, other, nil)
		if rec.Body.String() != "from-upstream" || up.count() != 1 {
			t.Fatalf("mismatched path = %q, upstream %d", rec.Body.String(), up.count())
		}
	})

	t.Run("missing body falls back", func(t *testing.T) {
		up, s, store, target := setup(t)
		if err := store.Delete(context.Background(), hashstore.SHA256, bodyDigestHex(digestTestBody)); err != nil {
			t.Fatalf("delete body: %v", err)
		}
		rec := cacheRequest(s, http.MethodGet, target, nil)
		if rec.Body.String() != "from-upstream" || up.count() != 1 {
			t.Fatalf("missing body = %q, upstream %d", rec.Body.String(), up.count())
		}
	})

	t.Run("size mismatch falls back", func(t *testing.T) {
		up, s, store := digestUpstreamServer(t)
		digest := bodyDigestHex(digestTestBody)
		path := blobPath + digest
		// Write the metadata before the body: with no local body record the
		// store cannot cross-check the declared size, which is exactly how a
		// metadata/body disagreement can exist.
		v := hashstore.Variant{
			URL:    "mirror://docker" + path,
			NoVary: true,
			ResponseHeaders: http.Header{
				"Cache-Control": {"public, max-age=31536000, immutable"},
			},
			BodySHA256: digest,
			BodySize:   int64(len(digestTestBody)) + 1,
			StoredAt:   s.currentTime().Add(-48 * time.Hour),
			ExpiresAt:  s.currentTime().Add(-24 * time.Hour),
		}
		if err := store.PutVariant(context.Background(), v); err != nil {
			t.Fatalf("put variant: %v", err)
		}
		if _, err := store.Put(context.Background(), strings.NewReader(digestTestBody), int64(len(digestTestBody))); err != nil {
			t.Fatalf("put body: %v", err)
		}
		rec := cacheRequest(s, http.MethodGet, "/docker"+path, nil)
		if rec.Body.String() != "from-upstream" || up.count() != 1 {
			t.Fatalf("size mismatch = %q, upstream %d", rec.Body.String(), up.count())
		}
	})

	t.Run("tag still revalidates", func(t *testing.T) {
		up, s, store := digestUpstreamServer(t)
		tagPath := "/v2/library/nginx/manifests/latest"
		putStaleVariant(t, store, "mirror://docker"+tagPath, "tagged", s.currentTime())
		rec := cacheRequest(s, http.MethodGet, "/docker"+tagPath, nil)
		if rec.Body.String() != "from-upstream" || up.count() != 1 {
			t.Fatalf("tag = %q, upstream %d", rec.Body.String(), up.count())
		}
	})

	t.Run("sha512 path is not served by digest identity", func(t *testing.T) {
		up, s, store := digestUpstreamServer(t)
		sha512Path := "/v2/library/nginx/manifests/sha512:" + strings.Repeat("b", 128)
		putStaleVariant(t, store, "mirror://docker"+sha512Path, "sha512-body", s.currentTime())
		rec := cacheRequest(s, http.MethodGet, "/docker"+sha512Path, nil)
		if rec.Body.String() != "from-upstream" || up.count() != 1 {
			t.Fatalf("sha512 path = %q, upstream %d", rec.Body.String(), up.count())
		}
	})

	t.Run("client revalidation still reaches upstream", func(t *testing.T) {
		for _, headers := range []map[string]string{
			{"Cache-Control": "no-cache"},
			{"Cache-Control": "max-age=0"},
			{"Pragma": "no-cache"},
		} {
			up, s, _, target := setup(t)
			rec := cacheRequest(s, http.MethodGet, target, headers)
			if rec.Body.String() != "from-upstream" || up.count() != 1 {
				t.Fatalf("%v = %q, upstream %d", headers, rec.Body.String(), up.count())
			}
		}
	})

	t.Run("range with client revalidation reaches upstream", func(t *testing.T) {
		up, s, _, target := setup(t)
		cacheRequest(s, http.MethodGet, target, map[string]string{
			"Cache-Control": "no-cache",
			"Range":         "bytes=0-0",
		})
		if up.count() != 1 {
			t.Fatalf("forceRevalidate range did not reach upstream: %d", up.count())
		}
	})
}
