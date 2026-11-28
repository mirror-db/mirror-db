package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// newDigestPrefetchTask builds the lease a prefetch scheduler would hold for a
// content-addressed path. Production only schedules paths whose response carried
// `prefetch`, but the refresh itself is the same call.
func newDigestPrefetchTask(m Mirror, target, rel, cacheKey string) prefetchTask {
	return prefetchTask{
		key:            "normal\x00" + cacheKey,
		kind:           prefetchKindNormal,
		mirror:         m,
		relPath:        rel,
		clientPath:     target,
		cacheKey:       cacheKey,
		noVary:         true,
		requestHeaders: http.Header{},
	}
}

// A background refresh applies the path's captured digest: an identity body that
// contradicts the address publishes nothing and leaves the last good variant
// alone.
func TestPrefetchDigestPolicyRefusesContradictoryBody(t *testing.T) {
	body := activeBody()
	real := bodyDigestHex(string(body))
	wrong := bodyDigestHex("some other body")
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "max-age=3600"}))
	mirror := Mirror{Name: "docker", Path: "docker"}
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{mirror},
		policies: activeOCIPolicies(),
	})
	target := "/docker/v2/app/blobs/sha256:" + wrong
	rel := "/v2/app/blobs/sha256:" + wrong
	key := "mirror://docker" + rel

	out := s.runNormalPrefetch(context.Background(), newDigestPrefetchTask(mirror, target, rel, key))
	if !errors.Is(out.err, hashstore.ErrDigestMismatch) {
		t.Fatalf("refresh error = %v, want ErrDigestMismatch", out.err)
	}
	if up.count() != 1 {
		t.Fatalf("upstream requests = %d, want 1", up.count())
	}
	if got := len(activeVariants(t, store, key)); got != 0 {
		t.Fatalf("refresh published %d variants", got)
	}
	if _, _, err := store.Get(context.Background(), hashstore.SHA256, real); !errors.Is(err, hashstore.ErrNotFound) {
		t.Fatalf("contradictory body was published: %v", err)
	}
}

// A non-identity response on a digest path is a negotiated representation, not
// the object. A refresh consumes it without writing a body or a variant, and
// keeps the response's own freshness as the next schedule.
func TestPrefetchDigestPolicySkipsNonIdentityBody(t *testing.T) {
	body := activeBody()
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(body); err != nil {
		t.Fatalf("gzip body: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	gzBody := compressed.Bytes()
	// The path names the gzip representation's own bytes, so only the explicit
	// non-identity refusal keeps them out of the store.
	wrong := bodyDigestHex(string(gzBody))
	up, upSrv := startUpstream(t, activeUpstreamHandler(gzBody, map[string]string{
		"Cache-Control":    "max-age=3600",
		"Content-Encoding": "gzip",
	}))
	mirror := Mirror{Name: "docker", Path: "docker"}
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{mirror},
		policies: activeOCIPolicies(),
	})
	target := "/docker/v2/app/blobs/sha256:" + wrong
	rel := "/v2/app/blobs/sha256:" + wrong
	key := "mirror://docker" + rel

	out := s.runNormalPrefetch(context.Background(), newDigestPrefetchTask(mirror, target, rel, key))
	if out.err != nil {
		t.Fatalf("refresh error = %v", out.err)
	}
	if out.next.IsZero() {
		t.Fatal("refresh lost the response freshness")
	}
	if up.count() != 1 {
		t.Fatalf("upstream requests = %d, want 1", up.count())
	}
	if got := len(activeVariants(t, store, key)); got != 0 {
		t.Fatalf("refresh published %d variants", got)
	}
	if _, _, err := store.Get(context.Background(), hashstore.SHA256, bodyDigestHex(string(gzBody))); !errors.Is(err, hashstore.ErrNotFound) {
		t.Fatalf("non-identity body was published: %v", err)
	}
}

// A contradictory refresh must leave the last good variant and its body in
// place: the background run learned only that this response is not the object,
// not that the stored one stopped being it.
func TestPrefetchDigestPolicyKeepsLastGoodOnContradiction(t *testing.T) {
	oldBody := activeBody()
	newBody := bytes.ToUpper(oldBody)
	oldDigest := bodyDigestHex(string(oldBody))
	newDigest := bodyDigestHex(string(newBody))
	up, upSrv := startUpstream(t, activeUpstreamHandler(newBody, map[string]string{"Cache-Control": "max-age=3600"}))
	mirror := Mirror{Name: "docker", Path: "docker"}
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{mirror},
		policies: activeOCIPolicies(),
	})
	target := "/docker/v2/app/blobs/sha256:" + oldDigest
	rel := "/v2/app/blobs/sha256:" + oldDigest
	key := "mirror://docker" + rel

	content, err := store.Put(context.Background(), bytes.NewReader(oldBody), int64(len(oldBody)))
	if err != nil {
		t.Fatalf("store last-good body: %v", err)
	}
	now := s.currentTime()
	old := hashstore.Variant{
		URL:             key,
		NoVary:          true,
		ResponseHeaders: http.Header{"Cache-Control": {"max-age=60"}, "ETag": {`"old"`}},
		BodySHA256:      content.SHA256,
		BodySize:        content.Size,
		StoredAt:        now,
		ExpiresAt:       now.Add(time.Minute),
	}
	if err := store.PutVariant(context.Background(), old); err != nil {
		t.Fatalf("store last-good variant: %v", err)
	}

	out := s.runNormalPrefetch(context.Background(), newDigestPrefetchTask(mirror, target, rel, key))
	if !errors.Is(out.err, hashstore.ErrDigestMismatch) {
		t.Fatalf("refresh error = %v, want ErrDigestMismatch", out.err)
	}
	if up.count() != 1 {
		t.Fatalf("upstream requests = %d, want 1", up.count())
	}
	variants := activeVariants(t, store, key)
	if len(variants) != 1 || variants[0].BodySHA256 != oldDigest {
		t.Fatalf("variants after a contradiction = %+v, want the last good one", variants)
	}
	rc, _, err := store.Get(context.Background(), hashstore.SHA256, oldDigest)
	if err != nil {
		t.Fatalf("last-good body was lost: %v", err)
	}
	defer rc.Close()
	kept, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read last-good body: %v", err)
	}
	if !bytes.Equal(kept, oldBody) {
		t.Fatalf("last-good body = %d bytes, want %d", len(kept), len(oldBody))
	}
	if _, _, err := store.Get(context.Background(), hashstore.SHA256, newDigest); !errors.Is(err, hashstore.ErrNotFound) {
		t.Fatalf("contradictory body was published: %v", err)
	}
}
