package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tiered "github.com/geektr-cloud/go-tiered-cache"
	"github.com/mirror-db/mirror-db/tools/relay/activecache"
	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

const activeBodySize = 4096

func activeBody() []byte {
	body := make([]byte, activeBodySize)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	return body
}

func activeSHA1(body []byte) string {
	sum := sha1.Sum(body)
	return hex.EncodeToString(sum[:])
}

func activeOCIPolicies() []Policy {
	return []Policy{{
		Name:  "immutable-oci",
		Rules: PolicyRules{Immutable: stringRule(workerImmutableOCI)},
	}}
}

func activeAPTPolicies() []Policy {
	return []Policy{{
		Name:  "immutable-apt",
		Rules: PolicyRules{Immutable: stringRule(workerImmutableAPT)},
	}}
}

func activePlainPolicies() []Policy {
	return []Policy{{
		Name:  "immutable-plain",
		Rules: PolicyRules{Immutable: stringRule(`^/blob/`)},
	}}
}

// parseSingleRange understands the one-range forms the test upstream serves.
func parseSingleRange(value string, size int64) (start, end int64, ok bool) {
	unit, spec, found := strings.Cut(strings.TrimSpace(value), "=")
	if !found || !strings.EqualFold(strings.TrimSpace(unit), "bytes") || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	spec = strings.TrimSpace(spec)
	if strings.HasPrefix(spec, "-") {
		suffix, err := strconv.ParseInt(spec[1:], 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, true
	}
	rawStart, rawEnd, hasEnd := strings.Cut(spec, "-")
	start, err := strconv.ParseInt(strings.TrimSpace(rawStart), 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end = size - 1
	if hasEnd && strings.TrimSpace(rawEnd) != "" {
		end, err = strconv.ParseInt(strings.TrimSpace(rawEnd), 10, 64)
		if err != nil || end < start {
			return 0, 0, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true
}

// activeUpstreamHandler serves a complete body and single byte ranges, and
// answers a matching If-None-Match with 304.
func activeUpstreamHandler(body []byte, extra map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for name, value := range extra {
			w.Header().Set(name, value)
		}
		if w.Header().Get("ETag") == "" {
			w.Header().Set("ETag", `"v1"`)
		}
		if w.Header().Get("Last-Modified") == "" {
			w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		}
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		w.Header().Set("Accept-Ranges", "bytes")

		if inm := r.Header.Get("If-None-Match"); inm != "" && inm == w.Header().Get("ETag") {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		rangeHeader := strings.TrimSpace(r.Header.Get("Range"))
		if rangeHeader == "" || r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(body)
			}
			return
		}
		start, end, ok := parseSingleRange(rangeHeader, int64(len(body)))
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(body)))
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body[start : end+1])
		}
	}
}

func activeVariants(t *testing.T, store *hashstore.Store, key string) []hashstore.Variant {
	t.Helper()
	variants, err := store.Variants(context.Background(), key)
	if err != nil {
		t.Fatalf("list variants %q: %v", key, err)
	}
	return variants
}

// TestActiveImmutableDigestColdGet covers a cold full GET and every local
// delivery shape the resulting URL variant must answer.
func TestActiveImmutableDigestColdGet(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control": "max-age=3600",
	}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})

	target := "/docker/v2/app/blobs/sha256:" + digest
	key := "mirror://docker/v2/app/blobs/sha256:" + digest

	first := cacheRequest(s, http.MethodGet, target, nil)
	if first.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), body) {
		t.Fatalf("first GET = %d, %d bytes", first.Code, first.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("first GET hit upstream %d times", up.count())
	}
	if got := len(activeVariants(t, store, key)); got != 1 {
		t.Fatalf("published URL variants = %d, want 1", got)
	}

	second := cacheRequest(s, http.MethodGet, target, nil)
	if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), body) {
		t.Fatalf("second GET = %d, %d bytes", second.Code, second.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("published GET hit upstream: %d", up.count())
	}

	head := cacheRequest(s, http.MethodHead, target, nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD = %d, %d bytes", head.Code, head.Body.Len())
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("HEAD Content-Length = %q", got)
	}

	single := cacheRequest(s, http.MethodGet, target, map[string]string{"Range": "bytes=10-19"})
	if single.Code != http.StatusPartialContent || !bytes.Equal(single.Body.Bytes(), body[10:20]) {
		t.Fatalf("single range = %d %q", single.Code, single.Body.Bytes())
	}
	wantRange := fmt.Sprintf("bytes 10-19/%d", len(body))
	if got := single.Header().Get("Content-Range"); got != wantRange {
		t.Fatalf("Content-Range = %q, want %q", got, wantRange)
	}

	multi := cacheRequest(s, http.MethodGet, target, map[string]string{"Range": "bytes=0-4,10-14"})
	if multi.Code != http.StatusPartialContent {
		t.Fatalf("multipart range = %d", multi.Code)
	}
	mediaType, params, err := mime.ParseMediaType(multi.Header().Get("Content-Type"))
	if err != nil || mediaType != "multipart/byteranges" {
		t.Fatalf("multipart Content-Type = %q (%v)", multi.Header().Get("Content-Type"), err)
	}
	reader := multipart.NewReader(bytes.NewReader(multi.Body.Bytes()), params["boundary"])
	part, err := reader.NextPart()
	if err != nil {
		t.Fatalf("multipart first part: %v", err)
	}
	partBody := new(bytes.Buffer)
	_, _ = partBody.ReadFrom(part)
	if !bytes.Equal(partBody.Bytes(), body[0:5]) {
		t.Fatalf("multipart first part = %q", partBody.Bytes())
	}

	unsatisfiable := cacheRequest(s, http.MethodGet, target, map[string]string{"Range": "bytes=999999-"})
	if unsatisfiable.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("416 = %d", unsatisfiable.Code)
	}
	if got := unsatisfiable.Header().Get("Content-Range"); got != fmt.Sprintf("bytes */%d", len(body)) {
		t.Fatalf("416 Content-Range = %q", got)
	}

	notModified := cacheRequest(s, http.MethodGet, target, map[string]string{"If-None-Match": `"v1"`})
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
		t.Fatalf("conditional = %d, %d bytes", notModified.Code, notModified.Body.Len())
	}

	ifRange := cacheRequest(s, http.MethodGet, target, map[string]string{
		"Range":    "bytes=0-4",
		"If-Range": `"does-not-match"`,
	})
	if ifRange.Code != http.StatusOK || !bytes.Equal(ifRange.Body.Bytes(), body) {
		t.Fatalf("If-Range mismatch = %d, %d bytes", ifRange.Code, ifRange.Body.Len())
	}

	if up.count() != 1 {
		t.Fatalf("local deliveries hit upstream: %d", up.count())
	}
}

// TestActiveColdRangeFillsLater covers an active entry seeded by a partial
// range: local multipart/416/conditional deliveries, then a full read that
// completes the object and publishes it.
func TestActiveColdRangeFillsLater(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control": "max-age=3600",
	}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	target := "/docker/v2/app/blobs/sha256:" + digest
	key := "mirror://docker/v2/app/blobs/sha256:" + digest

	first := cacheRequest(s, http.MethodGet, target, map[string]string{"Range": "bytes=100-199"})
	if first.Code != http.StatusPartialContent || !bytes.Equal(first.Body.Bytes(), body[100:200]) {
		t.Fatalf("cold range = %d, %d bytes", first.Code, first.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("cold range hit upstream %d times", up.count())
	}
	if got := len(activeVariants(t, store, key)); got != 0 {
		t.Fatalf("partial range published %d variants", got)
	}

	multi := cacheRequest(s, http.MethodGet, target, map[string]string{"Range": "bytes=100-109,190-199"})
	if multi.Code != http.StatusPartialContent {
		t.Fatalf("active multipart = %d", multi.Code)
	}
	if !bytes.Contains(multi.Body.Bytes(), body[100:110]) || !bytes.Contains(multi.Body.Bytes(), body[190:200]) {
		t.Fatalf("active multipart body lacks requested parts")
	}

	unsatisfiable := cacheRequest(s, http.MethodGet, target, map[string]string{"Range": "bytes=999999-"})
	if unsatisfiable.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("active 416 = %d", unsatisfiable.Code)
	}

	notModified := cacheRequest(s, http.MethodGet, target, map[string]string{"If-None-Match": `"v1"`})
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("active conditional = %d", notModified.Code)
	}
	if up.count() != 1 {
		t.Fatalf("partial local deliveries hit upstream: %d", up.count())
	}

	full := cacheRequest(s, http.MethodGet, target, map[string]string{
		"Range":    "bytes=0-9",
		"If-Range": `"does-not-match"`,
	})
	if full.Code != http.StatusOK || !bytes.Equal(full.Body.Bytes(), body) {
		t.Fatalf("active full after partial = %d, %d bytes", full.Code, full.Body.Len())
	}

	if got := len(activeVariants(t, store, key)); got != 1 {
		t.Fatalf("completed object published %d variants", got)
	}
	before := up.count()
	again := cacheRequest(s, http.MethodGet, target, nil)
	if again.Code != http.StatusOK || !bytes.Equal(again.Body.Bytes(), body) {
		t.Fatalf("GET after publish = %d, %d bytes", again.Code, again.Body.Len())
	}
	if up.count() != before {
		t.Fatalf("published object hit upstream again: %d -> %d", before, up.count())
	}
}

// TestActiveLateReadersShareOneUpstream starts a second full GET and a range
// while the first body is still arriving: neither may open another upstream.
func TestActiveLateReadersShareOneUpstream(t *testing.T) {
	body := make([]byte, 8192)
	for i := range body {
		body[i] = byte('A' + i%26)
	}
	digest := bodyDigestHex(string(body))
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once

	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("ETag", `"slow"`)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Cache-Control", "max-age=3600")
		w.WriteHeader(http.StatusOK)
		half := len(body) / 2
		_, _ = w.Write(body[:half])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		startOnce.Do(func() { close(started) })
		<-release
		_, _ = w.Write(body[half:])
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	target := "/docker/v2/app/blobs/sha256:" + digest

	creatorDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		creatorDone <- cacheRequest(s, http.MethodGet, target, nil)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never sent the first half")
	}
	// Let the creator register its full read before the late range can win the
	// scheduler race; a partial plan that wins fails only itself and falls back.
	time.Sleep(50 * time.Millisecond)

	lateFullDone := make(chan *httptest.ResponseRecorder, 1)
	lateRangeDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		lateFullDone <- cacheRequest(s, http.MethodGet, target, nil)
	}()
	go func() {
		lateRangeDone <- cacheRequest(s, http.MethodGet, target, map[string]string{"Range": "bytes=2000-2099"})
	}()

	// Give both late requests time to acquire the entry and wait for bytes.
	time.Sleep(50 * time.Millisecond)
	close(release)

	select {
	case rec := <-creatorDone:
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("creator = %d, %d bytes", rec.Code, rec.Body.Len())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("creator did not finish")
	}
	select {
	case rec := <-lateFullDone:
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("late full = %d, %d bytes", rec.Code, rec.Body.Len())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late full GET did not finish")
	}
	select {
	case rec := <-lateRangeDone:
		if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), body[2000:2100]) {
			t.Fatalf("late range = %d, %d bytes", rec.Code, rec.Body.Len())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late range did not finish")
	}
	if up.count() != 1 {
		t.Fatalf("concurrent readers opened %d upstream requests", up.count())
	}
}

// TestActiveDigestSharedAcrossMirrors proves the digest key is global: a body
// published through one mount serves a digest path on another mount.
func TestActiveDigestSharedAcrossMirrors(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control": "max-age=3600",
	}))
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors: []Mirror{
			{Name: "alpha", Path: "alpha"},
			{Name: "beta", Path: "beta"},
		},
		policies: activeOCIPolicies(),
	})

	if rec := cacheRequest(s, http.MethodGet, "/alpha/v2/app/blobs/sha256:"+digest, nil); rec.Code != http.StatusOK {
		t.Fatalf("publish GET = %d", rec.Code)
	}
	if up.count() != 1 {
		t.Fatalf("publish GET upstream = %d", up.count())
	}

	rec := cacheRequest(s, http.MethodGet, "/beta/v2/app/blobs/sha256:"+digest, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("cross-mirror digest GET = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	head := cacheRequest(s, http.MethodHead, "/beta/v2/app/blobs/sha256:"+digest, nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("cross-mirror HEAD = %d, %d bytes", head.Code, head.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("cross-mirror digest hit upstream: %d", up.count())
	}
}

// TestActiveNoDigestPublishesURLVariant covers the immutable path whose pattern
// captures no digest: the URL variant is the only way a later request can be
// local.
func TestActiveNoDigestPublishesURLVariant(t *testing.T) {
	body := activeBody()
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control": "max-age=3600",
	}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "plain", Path: "plain"}},
		policies: activePlainPolicies(),
	})
	target := "/plain/blob/x.bin"
	key := "mirror://plain/blob/x.bin"

	if rec := cacheRequest(s, http.MethodGet, target, nil); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("first GET = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	variants := activeVariants(t, store, key)
	if len(variants) != 1 || variants[0].BodySHA256 != bodyDigestHex(string(body)) {
		t.Fatalf("URL variants = %+v", variants)
	}
	if rec := cacheRequest(s, http.MethodGet, target, nil); rec.Code != http.StatusOK || up.count() != 1 {
		t.Fatalf("second GET = %d, upstream %d", rec.Code, up.count())
	}

	// Without a captured digest there is no content address, so losing the URL
	// variant must go back upstream.
	if err := store.DeleteVariant(context.Background(), variants[0]); err != nil {
		t.Fatalf("delete variant: %v", err)
	}
	if rec := cacheRequest(s, http.MethodGet, target, nil); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("GET after variant loss = %d", rec.Code)
	}
	if up.count() != 2 {
		t.Fatalf("no-digest path did not refetch: %d", up.count())
	}
}

// TestActiveResponseProhibitions checks every response-side reason an immutable
// body may not enter the active segment.
func TestActiveResponseProhibitions(t *testing.T) {
	digestOf := func(body []byte) string { return bodyDigestHex(string(body)) }

	t.Run("no-store", func(t *testing.T) {
		body := activeBody()
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "no-store"}))
		s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
			mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
			policies: activeOCIPolicies(),
		})
		target := "/docker/v2/app/blobs/sha256:" + digestOf(body)
		for i := 0; i < 2; i++ {
			rec := cacheRequest(s, http.MethodGet, target, nil)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
				t.Fatalf("GET %d = %d", i, rec.Code)
			}
		}
		if up.count() != 2 {
			t.Fatalf("no-store upstream = %d", up.count())
		}
		if got := len(activeVariants(t, store, "mirror://docker/v2/app/blobs/sha256:"+digestOf(body))); got != 0 {
			t.Fatalf("no-store variants = %d", got)
		}
	})

	t.Run("private", func(t *testing.T) {
		body := activeBody()
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "private"}))
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
			mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
			policies: activeOCIPolicies(),
		})
		target := "/docker/v2/app/blobs/sha256:" + digestOf(body)
		for i := 0; i < 2; i++ {
			if rec := cacheRequest(s, http.MethodGet, target, nil); rec.Code != http.StatusOK {
				t.Fatalf("GET %d = %d", i, rec.Code)
			}
		}
		if up.count() != 2 {
			t.Fatalf("private upstream = %d", up.count())
		}
	})

	t.Run("vary", func(t *testing.T) {
		body := activeBody()
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			"Cache-Control": "max-age=3600",
			"Vary":          "Accept-Language",
		}))
		s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
			mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
			policies: activeOCIPolicies(),
		})
		target := "/docker/v2/app/blobs/sha256:" + digestOf(body)
		key := "mirror://docker/v2/app/blobs/sha256:" + digestOf(body)
		for i := 0; i < 2; i++ {
			if rec := cacheRequest(s, http.MethodGet, target, nil); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
				t.Fatalf("GET %d = %d", i, rec.Code)
			}
		}
		// A Vary the ordinary chain can represent must still be stored there;
		// the active segment only declines to share it as a byte-addressed
		// object.
		if up.count() != 1 {
			t.Fatalf("vary response was not stored by the ordinary chain: upstream %d", up.count())
		}
		if got := len(activeVariants(t, store, key)); got != 1 {
			t.Fatalf("vary variants = %d, want 1", got)
		}
	})

	t.Run("non-identity coding", func(t *testing.T) {
		plain := activeBody()
		var compressed bytes.Buffer
		zw := gzip.NewWriter(&compressed)
		_, _ = zw.Write(plain)
		_ = zw.Close()
		body := compressed.Bytes()

		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			"Cache-Control":    "max-age=3600",
			"Content-Encoding": "gzip",
		}))
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
			mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
			policies: activeOCIPolicies(),
		})
		target := "/docker/v2/app/blobs/sha256:" + digestOf(plain)
		for i := 0; i < 2; i++ {
			rec := cacheRequest(s, http.MethodGet, target, map[string]string{"Accept-Encoding": "identity"})
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
				t.Fatalf("gzip GET %d = %d, %d bytes", i, rec.Code, rec.Body.Len())
			}
		}
		if up.count() != 2 {
			t.Fatalf("non-identity upstream = %d", up.count())
		}
	})

	t.Run("client no-store", func(t *testing.T) {
		body := activeBody()
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "max-age=3600"}))
		s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
			mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
			policies: activeOCIPolicies(),
		})
		target := "/docker/v2/app/blobs/sha256:" + digestOf(body)
		for i := 0; i < 2; i++ {
			rec := cacheRequest(s, http.MethodGet, target, map[string]string{"Cache-Control": "no-store"})
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
				t.Fatalf("no-store GET %d = %d", i, rec.Code)
			}
		}
		if up.count() != 2 {
			t.Fatalf("client no-store upstream = %d", up.count())
		}
		if got := len(activeVariants(t, store, "mirror://docker/v2/app/blobs/sha256:"+digestOf(body))); got != 0 {
			t.Fatalf("client no-store variants = %d", got)
		}
	})
}

func TestActivePassthroughStaysOnProxy(t *testing.T) {
	body := activeBody()
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "max-age=3600"}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors: []Mirror{{Name: "plain", Path: "plain"}},
		policies: []Policy{{
			Name:  "passthrough",
			Match: PolicyMatch{PathPrefix: "/blob/"},
			Rules: PolicyRules{
				Passthrough: boolRule(true),
				Immutable:   stringRule(`^/blob/`),
			},
		}},
	})
	target := "/plain/blob/x.bin"
	for i := 0; i < 2; i++ {
		rec := cacheRequest(s, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("passthrough GET %d = %d", i, rec.Code)
		}
	}
	if up.count() != 2 {
		t.Fatalf("passthrough upstream = %d", up.count())
	}
	if got := len(activeVariants(t, store, "mirror://plain/blob/x.bin")); got != 0 {
		t.Fatalf("passthrough variants = %d", got)
	}
}

func TestActiveForceRevalidateReachesUpstream(t *testing.T) {
	body := activeBody()
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "max-age=3600"}))
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	target := "/docker/v2/app/blobs/sha256:" + bodyDigestHex(string(body))

	if rec := cacheRequest(s, http.MethodGet, target, nil); rec.Code != http.StatusOK {
		t.Fatalf("publish GET = %d", rec.Code)
	}
	rec := cacheRequest(s, http.MethodGet, target, map[string]string{"Cache-Control": "no-cache"})
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("no-cache GET = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if up.count() != 2 {
		t.Fatalf("no-cache did not revalidate upstream: %d", up.count())
	}
	rec = cacheRequest(s, http.MethodGet, target, map[string]string{"Cache-Control": "max-age=0"})
	if rec.Code != http.StatusOK || up.count() != 3 {
		t.Fatalf("max-age=0 GET = %d, upstream %d", rec.Code, up.count())
	}
}

func TestActiveHeadDoesNotCreateEntry(t *testing.T) {
	body := activeBody()
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "max-age=3600"}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	target := "/docker/v2/app/blobs/sha256:" + bodyDigestHex(string(body))
	for i := 0; i < 2; i++ {
		rec := cacheRequest(s, http.MethodHead, target, nil)
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
			t.Fatalf("HEAD %d = %d, %d bytes", i, rec.Code, rec.Body.Len())
		}
	}
	if up.count() != 2 {
		t.Fatalf("HEAD upstream = %d", up.count())
	}
	if got := len(activeVariants(t, store, "mirror://docker/v2/app/blobs/sha256:"+bodyDigestHex(string(body)))); got != 0 {
		t.Fatalf("HEAD created %d variants", got)
	}
}

func TestActiveDigestMismatchNeverPublishes(t *testing.T) {
	body := activeBody()
	wrong := bodyDigestHex("some other body")
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "max-age=3600"}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	target := "/docker/v2/app/blobs/sha256:" + wrong
	key := "mirror://docker/v2/app/blobs/sha256:" + wrong

	for i := 0; i < 2; i++ {
		rec := cacheRequest(s, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("GET %d = %d, %d bytes", i, rec.Code, rec.Body.Len())
		}
	}
	if up.count() != 2 {
		t.Fatalf("digest mismatch should never be reused: upstream %d", up.count())
	}
	if got := len(activeVariants(t, store, key)); got != 0 {
		t.Fatalf("digest mismatch published %d variants", got)
	}
}

func TestActiveCapacityFallsBackToOrdinaryResponse(t *testing.T) {
	body := activeBody()
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "max-age=3600"}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:        []Mirror{{Name: "docker", Path: "docker"}},
		policies:       activeOCIPolicies(),
		activeCapacity: 64,
	})
	target := "/docker/v2/app/blobs/sha256:" + bodyDigestHex(string(body))
	key := "mirror://docker/v2/app/blobs/sha256:" + bodyDigestHex(string(body))

	rec := cacheRequest(s, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("capacity fallback = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	// The first response was fetched for the active entry and admission refused
	// it. That response is delivered through the ordinary chain, which stores it
	// normally: an admission refusal must not cost a second upstream read.
	if up.count() != 1 {
		t.Fatalf("capacity fallback upstream = %d, want 1", up.count())
	}
	if got := len(activeVariants(t, store, key)); got != 1 {
		t.Fatalf("ordinary chain variants = %d, want 1", got)
	}
	second := cacheRequest(s, http.MethodGet, target, nil)
	if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), body) || up.count() != 1 {
		t.Fatalf("capacity fallback second GET = %d, upstream %d", second.Code, up.count())
	}
}

func TestActiveNonImmutablePathKeepsOrdinaryChain(t *testing.T) {
	body := activeBody()
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "max-age=3600"}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors: []Mirror{{Name: "plain", Path: "plain"}},
	})
	target := "/plain/data.bin"
	first := cacheRequest(s, http.MethodGet, target, nil)
	if first.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), body) {
		t.Fatalf("first GET = %d", first.Code)
	}
	if got := len(activeVariants(t, store, "mirror://plain/data.bin")); got != 1 {
		t.Fatalf("ordinary chain variants = %d", got)
	}
	second := cacheRequest(s, http.MethodGet, target, nil)
	if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), body) || up.count() != 1 {
		t.Fatalf("ordinary chain second GET = %d, upstream %d", second.Code, up.count())
	}
}

func TestActiveAPTHashPublishesBothAlgorithms(t *testing.T) {
	t.Run("sha256", func(t *testing.T) {
		body := activeBody()
		sha256Hex := bodyDigestHex(string(body))
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "max-age=3600"}))
		s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
			mirrors: []Mirror{
				{Name: "debian", Path: "debian"},
				{Name: "debian-other", Path: "debian-other"},
			},
			policies: activeAPTPolicies(),
		})

		path := "/debian/dists/bookworm/main/binary-amd64/by-hash/SHA256/" + sha256Hex
		if rec := cacheRequest(s, http.MethodGet, path, nil); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("SHA256 by-hash GET = %d", rec.Code)
		}
		if size, ok, err := store.Stat(context.Background(), hashstore.SHA256, sha256Hex); err != nil || !ok || size != int64(len(body)) {
			t.Fatalf("SHA256 body not published: size=%d ok=%v err=%v", size, ok, err)
		}
		if rec := cacheRequest(s, http.MethodGet, path, nil); rec.Code != http.StatusOK || up.count() != 1 {
			t.Fatalf("SHA256 second GET = %d, upstream %d", rec.Code, up.count())
		}

		// The same content address on another mount resolves without a request.
		crossPath := "/debian-other/dists/bookworm/main/binary-amd64/by-hash/SHA256/" + sha256Hex
		if rec := cacheRequest(s, http.MethodGet, crossPath, nil); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("cross-mount SHA256 by-hash = %d", rec.Code)
		}
		if up.count() != 1 {
			t.Fatalf("cross-mount by-hash hit upstream: %d", up.count())
		}
	})

	t.Run("sha1", func(t *testing.T) {
		body := bytes.Repeat([]byte("sha1-only-body-"), 256)
		sha1Hex := activeSHA1(body)
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "max-age=3600"}))
		s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
			mirrors:  []Mirror{{Name: "debian", Path: "debian"}},
			policies: activeAPTPolicies(),
		})

		path := "/debian/dists/bookworm/main/binary-amd64/by-hash/SHA1/" + sha1Hex
		if rec := cacheRequest(s, http.MethodGet, path, nil); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("SHA1 by-hash GET = %d", rec.Code)
		}
		if size, ok, err := store.Stat(context.Background(), hashstore.SHA1, sha1Hex); err != nil || !ok || size != int64(len(body)) {
			t.Fatalf("SHA1 body not published: size=%d ok=%v err=%v", size, ok, err)
		}
		if rec := cacheRequest(s, http.MethodGet, path, nil); rec.Code != http.StatusOK || up.count() != 1 {
			t.Fatalf("SHA1 second GET = %d, upstream %d", rec.Code, up.count())
		}
	})
}

// TestActiveFollowUpRequestsCarryOnlySafeHeaders proves a later range request
// keeps the relay host and identity coding but never the creator's credentials.
func TestActiveFollowUpRequestsCarryOnlySafeHeaders(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "max-age=3600"}))
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	target := "/docker/v2/app/blobs/sha256:" + digest

	first := cacheRequest(s, http.MethodGet, target, map[string]string{
		"Range":         "bytes=0-9",
		"Cookie":        "session=creator-secret",
		"Authorization": "Bearer creator-secret",
	})
	if first.Code != http.StatusPartialContent || !bytes.Equal(first.Body.Bytes(), body[0:10]) {
		t.Fatalf("seed range = %d", first.Code)
	}
	second := cacheRequest(s, http.MethodGet, target, map[string]string{"Range": "bytes=100-199"})
	if second.Code != http.StatusPartialContent || !bytes.Equal(second.Body.Bytes(), body[100:200]) {
		t.Fatalf("follow-up range = %d, %d bytes", second.Code, second.Body.Len())
	}
	if up.count() < 2 {
		t.Fatalf("follow-up range never reached upstream: %d", up.count())
	}
	last := up.lastHeader()
	if last.Get("Cookie") != "" || last.Get("Authorization") != "" {
		t.Fatalf("follow-up request leaked credentials: %+v", last)
	}
	if got := last.Get(RelayHostHeader); got != "relay.example.com" {
		t.Fatalf("follow-up relay host = %q", got)
	}
	if got := last.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("follow-up Accept-Encoding = %q", got)
	}
}

// TestActiveCloseCancelsInFlightBody proves Manager.Close can tear down a body
// whose creator is still reading, instead of waiting for the client.
func TestActiveCloseCancelsInFlightBody(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once

	_, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("ETag", `"slow"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body[:1024])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		startOnce.Do(func() { close(started) })
		<-release
		_, _ = w.Write(body[1024:])
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	target := "/docker/v2/app/blobs/sha256:" + digest

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cacheRequest(s, http.MethodGet, target, nil)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never started")
	}
	// The handler flushes before the relay has necessarily finished Init; give
	// the creator time to attach the body so this tests cancellation, not a
	// Close that raced initialization.
	time.Sleep(50 * time.Millisecond)
	if err := s.active.Close(); err != nil {
		t.Fatalf("active Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("in-flight body survived Manager.Close")
	}
	close(release)
}

// TestActiveUnknownSizeFallsBack covers a 200 without a declared length: the
// manager cannot reserve it, so the ordinary chain delivers (and may store) it.
func TestActiveUnknownSizeFallsBack(t *testing.T) {
	body := activeBody()
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"chunked"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		// No Content-Length and an explicit flush force chunked encoding.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body[:2048])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write(body[2048:])
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	target := "/docker/v2/app/blobs/sha256:" + bodyDigestHex(string(body))
	for i := 0; i < 2; i++ {
		rec := cacheRequest(s, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("chunked GET %d = %d, %d bytes", i, rec.Code, rec.Body.Len())
		}
	}
	// The active entry refused the unknown length; the already-fetched response
	// went through the ordinary writable path, so a normal cacheable chunked
	// body is stored once and served locally afterwards.
	if up.count() != 1 {
		t.Fatalf("unknown-size fallback fetched upstream %d times, want 1", up.count())
	}
}

// TestActiveWholeObjectRangePublishesVariant covers a Range that completes the
// whole object: publication must be awaited and a URL variant written, exactly
// like a full GET.
func TestActiveWholeObjectRangePublishesVariant(t *testing.T) {
	t.Run("single range", func(t *testing.T) {
		body := activeBody()
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "max-age=3600"}))
		s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
			mirrors:  []Mirror{{Name: "plain", Path: "plain"}},
			policies: activePlainPolicies(),
		})
		target := "/plain/blob/x.bin"
		key := "mirror://plain/blob/x.bin"

		rec := cacheRequest(s, http.MethodGet, target, map[string]string{
			"Range": fmt.Sprintf("bytes=0-%d", len(body)-1),
		})
		if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("whole-object range = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if got := len(activeVariants(t, store, key)); got != 1 {
			t.Fatalf("whole-object range published %d variants", got)
		}
		before := up.count()
		if again := cacheRequest(s, http.MethodGet, target, nil); again.Code != http.StatusOK || !bytes.Equal(again.Body.Bytes(), body) {
			t.Fatalf("GET after whole-object range = %d", again.Code)
		}
		if up.count() != before {
			t.Fatalf("whole-object range did not publish: %d -> %d", before, up.count())
		}
	})

	t.Run("multipart tiling the object", func(t *testing.T) {
		body := activeBody()
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "max-age=3600"}))
		s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
			mirrors:  []Mirror{{Name: "plain", Path: "plain"}},
			policies: activePlainPolicies(),
		})
		target := "/plain/blob/x.bin"
		key := "mirror://plain/blob/x.bin"

		half := len(body) / 2
		rec := cacheRequest(s, http.MethodGet, target, map[string]string{
			"Range": fmt.Sprintf("bytes=0-%d,%d-%d", half-1, half, len(body)-1),
		})
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("tiling multipart = %d", rec.Code)
		}
		if got := len(activeVariants(t, store, key)); got != 1 {
			t.Fatalf("tiling multipart published %d variants", got)
		}
		before := up.count()
		if again := cacheRequest(s, http.MethodGet, target, nil); again.Code != http.StatusOK || !bytes.Equal(again.Body.Bytes(), body) {
			t.Fatalf("GET after tiling multipart = %d", again.Code)
		}
		if up.count() != before {
			t.Fatalf("tiling multipart did not publish: %d -> %d", before, up.count())
		}
	})
}

// TestActiveOverBudgetFallsBackBeforeInit checks the store's per-body budget is
// applied before an entry is admitted, so no upstream response is fetched twice.
func TestActiveOverBudgetFallsBackBeforeInit(t *testing.T) {
	body := activeBody()
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{"Cache-Control": "max-age=3600"}))
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:        []Mirror{{Name: "docker", Path: "docker"}},
		policies:       activeOCIPolicies(),
		maxSize:        64,
		activeCapacity: 64,
	})
	target := "/docker/v2/app/blobs/sha256:" + bodyDigestHex(string(body))

	rec := cacheRequest(s, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("over-budget GET = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("over-budget seed was fetched %d times", up.count())
	}
	second := cacheRequest(s, http.MethodGet, target, nil)
	if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), body) {
		t.Fatalf("over-budget second GET = %d", second.Code)
	}
	if up.count() != 2 {
		t.Fatalf("over-budget body was stored: upstream %d", up.count())
	}
}

// activeStaticSource is a range provider for the classification unit test.
type activeStaticSource struct{ data []byte }

func (s *activeStaticSource) ReadSection(ctx context.Context, offset, size int64, extents chan<- tiered.Extent) (io.ReadCloser, error) {
	if extents != nil {
		select {
		case extents <- tiered.Extent{End: int64(len(s.data))}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	end := offset + size
	if end > int64(len(s.data)) {
		end = int64(len(s.data))
	}
	if offset >= end {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	return io.NopCloser(bytes.NewReader(s.data[offset:end])), nil
}

// TestActiveReadFailureClassification keeps the two failures that are a
// scheduling detail from poisoning an entry, while a broken source fails it at
// once: a later request must rebuild rather than reuse a hole.
func TestActiveReadFailureClassification(t *testing.T) {
	_, upSrv := startUpstream(t, activeUpstreamHandler(activeBody(), nil))
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	data := []byte("classify me")
	entry, created, err := s.active.Acquire(context.Background(), "classify")
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if err := entry.Init(&activeStaticSource{data: data}, activeMetadata{size: int64(len(data))}, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/classify", nil)
	s.activeReadFailed(req, "classify", entry, errActivePartialFirst)
	if err := entry.Err(); err != nil {
		t.Fatalf("partial-plan scheduling error failed the entry: %v", err)
	}

	s.activeReadFailed(req, "classify", entry, errors.New("dial tcp: connection reset"))
	if err := entry.Err(); err == nil {
		t.Fatal("broken upstream body did not fail the entry")
	}
	entry.Release()
}

func TestServerCloseStopsActiveBeforeHashStore(t *testing.T) {
	_, upSrv := startUpstream(t, activeUpstreamHandler(activeBody(), map[string]string{"Cache-Control": "max-age=3600"}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})

	s.Close()

	if _, _, err := s.active.Acquire(context.Background(), "probe"); !errors.Is(err, activecache.ErrClosed) {
		t.Fatalf("active Acquire after Close = %v, want ErrClosed", err)
	}
	if _, err := store.Variants(context.Background(), "probe"); !errors.Is(err, hashstore.ErrClosed) {
		t.Fatalf("hash store after Close = %v, want ErrClosed", err)
	}
}

// trackingTransport counts closed upstream response bodies. It proves a
// fallback that does not hand its response to the adapter still releases it.
type trackingTransport struct {
	base http.RoundTripper

	mu     sync.Mutex
	closes int
}

func (t *trackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if resp != nil && resp.Body != nil {
		resp.Body = &trackingBody{ReadCloser: resp.Body, parent: t}
	}
	return resp, err
}

func (t *trackingTransport) closed() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closes
}

type trackingBody struct {
	io.ReadCloser
	parent *trackingTransport
	once   sync.Once
}

func (b *trackingBody) Close() error {
	b.once.Do(func() {
		b.parent.mu.Lock()
		b.parent.closes++
		b.parent.mu.Unlock()
	})
	return b.ReadCloser.Close()
}

// TestActiveAbortedFullGetDoesNotPoisonEntry covers the terminal-notification
// path: the creator's full body ends early, the entry leaves the index at once,
// and the next client gets a complete body from a fresh assembly instead of a
// truncated cached body carrying the old Content-Length.
func TestActiveAbortedFullGetDoesNotPoisonEntry(t *testing.T) {
	body := activeBody()
	var (
		mu    sync.Mutex
		reqNo int
	)
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqNo++
		n := reqNo
		mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"abort"`)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			// Return before the declared length: the relay's exact reader sees
			// io.ErrUnexpectedEOF, which must fail the shared entry.
			_, _ = w.Write(body[:len(body)/2])
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			return
		}
		_, _ = w.Write(body)
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	target := "/docker/v2/app/blobs/sha256:" + bodyDigestHex(string(body))

	first := cacheRequest(s, http.MethodGet, target, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first GET = %d", first.Code)
	}

	second := cacheRequest(s, http.MethodGet, target, nil)
	if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), body) {
		t.Fatalf("second GET after aborted assembly = %d, %d bytes", second.Code, second.Body.Len())
	}
	if up.count() != 2 {
		t.Fatalf("aborted assembly was reused: upstream %d", up.count())
	}

	third := cacheRequest(s, http.MethodGet, target, nil)
	if third.Code != http.StatusOK || !bytes.Equal(third.Body.Bytes(), body) {
		t.Fatalf("third GET after rebuild = %d, %d bytes", third.Code, third.Body.Len())
	}
	if up.count() != 2 {
		t.Fatalf("rebuilt entry did not serve locally: upstream %d", up.count())
	}
}

// TestActiveLateRange200FallsBackToCompleteGet covers a 206 seed followed by a
// full GET whose hole fetches are answered with a 200. The origin ignored Range,
// so each hole is filled by a complete GET with its prefix discarded: the second
// client gets every byte instead of a truncated 200, and the third is local.
func TestActiveLateRange200FallsBackToCompleteGet(t *testing.T) {
	body := activeBody()
	var (
		mu       sync.Mutex
		requests []string
	)
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Header.Get("Range"))
		n := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"hole"`)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Cache-Control", "max-age=3600")
		if n == 1 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 100-199/%d", len(body)))
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body[100:200])
			return
		}
		// Range is ignored: every answer is the complete representation.
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	digest := bodyDigestHex(string(body))
	target := "/docker/v2/app/blobs/sha256:" + digest

	seed := cacheRequest(s, http.MethodGet, target, map[string]string{"Range": "bytes=100-199"})
	if seed.Code != http.StatusPartialContent || !bytes.Equal(seed.Body.Bytes(), body[100:200]) {
		t.Fatalf("cold range seed = %d, %d bytes", seed.Code, seed.Body.Len())
	}

	second := cacheRequest(s, http.MethodGet, target, nil)
	if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), body) {
		t.Fatalf("full GET over two holes = %d, %d bytes (want %d)", second.Code, second.Body.Len(), len(body))
	}
	mu.Lock()
	seen := append([]string(nil), requests...)
	mu.Unlock()
	// Seed, then one range attempt and one complete GET per hole ([0,100) and
	// [200,end)).
	if len(seen) != 5 {
		t.Fatalf("upstream requests = %d (%v), want 5", len(seen), seen)
	}
	if seen[len(seen)-1] != "" {
		t.Fatalf("last hole fill was not a complete GET: %v", seen)
	}
	key := "mirror://docker/v2/app/blobs/sha256:" + digest
	if got := len(activeVariants(t, store, key)); got != 1 {
		t.Fatalf("published variants = %d, want 1", got)
	}

	before := up.count()
	again := cacheRequest(s, http.MethodGet, target, nil)
	if again.Code != http.StatusOK || !bytes.Equal(again.Body.Bytes(), body) {
		t.Fatalf("local hit = %d, %d bytes", again.Code, again.Body.Len())
	}
	if up.count() != before {
		t.Fatalf("completed body not local: upstream %d -> %d", before, up.count())
	}
}

// TestActive200ToRangeUsesOrdinaryStore covers an origin that ignores a seed
// Range. The fetched 200 must go through the ordinary writable path: it is
// stored, the body is closed, and the second request is local.
func TestActive200ToRangeUsesOrdinaryStore(t *testing.T) {
	body := activeBody()
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control": "max-age=3600",
	}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	tracker := &trackingTransport{base: http.DefaultTransport}
	s.client.Transport = tracker
	target := "/docker/v2/app/blobs/sha256:" + bodyDigestHex(string(body))

	// The handler honours the Range, so force the ignored-Range case with a
	// handler that never returns 206 for this request.
	up.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"ignored"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}

	first := cacheRequest(s, http.MethodGet, target, map[string]string{"Range": "bytes=10-19"})
	if first.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), body) {
		t.Fatalf("200-to-range = %d, %d bytes", first.Code, first.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("200-to-range fetched twice: %d", up.count())
	}
	if tracker.closed() == 0 {
		t.Fatal("fallback response body was not closed")
	}
	if got := len(activeVariants(t, store, "mirror://docker/v2/app/blobs/sha256:"+bodyDigestHex(string(body)))); got != 1 {
		t.Fatalf("ordinary 200 was not stored: %d variants", got)
	}

	second := cacheRequest(s, http.MethodGet, target, map[string]string{"Range": "bytes=20-29"})
	if second.Code != http.StatusPartialContent || !bytes.Equal(second.Body.Bytes(), body[20:30]) {
		t.Fatalf("second request after ordinary store = %d, %d bytes", second.Code, second.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("stored 200 not reused: upstream %d", up.count())
	}
}

// TestActiveVaryAcceptStoredByOrdinaryChain proves a Vary the active byte cache
// cannot represent is still storable by the ordinary variant chain.
func TestActiveVaryAcceptStoredByOrdinaryChain(t *testing.T) {
	body := activeBody()
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control": "max-age=3600",
		"Vary":          "Accept",
	}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	target := "/docker/v2/app/blobs/sha256:" + bodyDigestHex(string(body))
	key := "mirror://docker/v2/app/blobs/sha256:" + bodyDigestHex(string(body))

	for i := 0; i < 2; i++ {
		rec := cacheRequest(s, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("GET %d = %d, %d bytes", i, rec.Code, rec.Body.Len())
		}
	}
	if up.count() != 1 {
		t.Fatalf("Vary: Accept was not stored by the ordinary chain: upstream %d", up.count())
	}
	if got := len(activeVariants(t, store, key)); got != 1 {
		t.Fatalf("Vary: Accept variants = %d, want 1", got)
	}
}

// TestActiveManifestMediaTypeAcrossMounts covers the metadata rule: a manifest
// is URL-keyed, never a synthetic digest-only hit, so another mount fetches it
// and keeps the upstream media type.
func TestActiveManifestMediaTypeAcrossMounts(t *testing.T) {
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	digest := bodyDigestHex(string(body))
	const mediaType = "application/vnd.oci.image.manifest.v1+json"
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control": "max-age=3600",
		"Content-Type":  mediaType,
	}))
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors: []Mirror{
			{Name: "alpha", Path: "alpha"},
			{Name: "beta", Path: "beta"},
		},
		policies: activeOCIPolicies(),
	})
	alpha := "/alpha/v2/app/manifests/sha256:" + digest
	beta := "/beta/v2/app/manifests/sha256:" + digest

	first := cacheRequest(s, http.MethodGet, alpha, nil)
	if first.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), body) {
		t.Fatalf("alpha manifest = %d", first.Code)
	}
	if got := first.Header().Get("Content-Type"); got != mediaType {
		t.Fatalf("alpha Content-Type = %q", got)
	}
	if rec := cacheRequest(s, http.MethodGet, alpha, nil); rec.Code != http.StatusOK || up.count() != 1 {
		t.Fatalf("alpha manifest not local: %d, upstream %d", rec.Code, up.count())
	}

	cross := cacheRequest(s, http.MethodGet, beta, nil)
	if cross.Code != http.StatusOK || !bytes.Equal(cross.Body.Bytes(), body) {
		t.Fatalf("beta manifest = %d", cross.Code)
	}
	if got := cross.Header().Get("Content-Type"); got != mediaType {
		t.Fatalf("cross-mount manifest Content-Type = %q, want %q", got, mediaType)
	}
	if up.count() != 2 {
		t.Fatalf("cross-mount manifest used a synthetic digest hit: upstream %d", up.count())
	}
	if rec := cacheRequest(s, http.MethodGet, beta, nil); rec.Code != http.StatusOK || up.count() != 2 {
		t.Fatalf("beta manifest not local after its own fetch: %d, upstream %d", rec.Code, up.count())
	}
}

// TestActiveMultipartFirstIntervalUnsatisfiable covers a multipart Range whose
// first interval the origin rejects. The seed's 416 must not answer the whole
// request; the ordinary chain evaluates the original Range.
func TestActiveMultipartFirstIntervalUnsatisfiable(t *testing.T) {
	body := activeBody()
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"multi"`)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Cache-Control", "max-age=3600")
		rng := strings.TrimSpace(r.Header.Get("Range"))
		switch {
		case rng == "bytes=999999-1000000":
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(body)))
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		case strings.Contains(rng, ","):
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-9/%d", len(body)))
			w.Header().Set("Content-Length", "10")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body[:10])
		default:
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "plain", Path: "plain"}},
		policies: activePlainPolicies(),
	})
	target := "/plain/blob/x.bin"

	rec := cacheRequest(s, http.MethodGet, target, map[string]string{
		"Range": "bytes=999999-1000000,0-9",
	})
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), body[:10]) {
		t.Fatalf("multipart with unsatisfiable first interval = %d, %q", rec.Code, rec.Body.Bytes())
	}
	if up.count() != 2 {
		t.Fatalf("original multipart Range was not re-evaluated: upstream %d", up.count())
	}
}

// TestActiveIdentityHitWhenTranscodingForbidden covers an OCI digest path and a
// client that asks for gzip after identity was published: the stored identity
// is delivered locally, never transcoded and never refetched.
func TestActiveIdentityHitWhenTranscodingForbidden(t *testing.T) {
	body := activeBody()
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control": "max-age=3600",
	}))
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	target := "/docker/v2/app/blobs/sha256:" + bodyDigestHex(string(body))

	if rec := cacheRequest(s, http.MethodGet, target, nil); rec.Code != http.StatusOK {
		t.Fatalf("publish GET = %d", rec.Code)
	}
	rec := cacheRequest(s, http.MethodGet, target, map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("gzip request = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("forbidden transcode delivered Content-Encoding %q", got)
	}
	if up.count() != 1 {
		t.Fatalf("identity hit went upstream: %d", up.count())
	}
}

// TestActiveCrossURLJoinerDoesNotCommitCreatorHeaders proves a joiner on a
// different URL shares the digest-keyed bytes but does not write the creator's
// response headers as a variant for its own URL.
func TestActiveCrossURLJoinerDoesNotCommitCreatorHeaders(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once

	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"creator"`)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		half := len(body) / 2
		_, _ = w.Write(body[:half])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		startOnce.Do(func() { close(started) })
		<-release
		_, _ = w.Write(body[half:])
	})
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors: []Mirror{
			{Name: "alpha", Path: "alpha"},
			{Name: "beta", Path: "beta"},
		},
		policies: activeOCIPolicies(),
	})
	alphaTarget := "/alpha/v2/app/blobs/sha256:" + digest
	betaTarget := "/beta/v2/app/blobs/sha256:" + digest

	alphaDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { alphaDone <- cacheRequest(s, http.MethodGet, alphaTarget, nil) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never started")
	}
	time.Sleep(50 * time.Millisecond)

	betaDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { betaDone <- cacheRequest(s, http.MethodGet, betaTarget, nil) }()
	time.Sleep(50 * time.Millisecond)
	close(release)

	for _, done := range []chan *httptest.ResponseRecorder{alphaDone, betaDone} {
		select {
		case rec := <-done:
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
				t.Fatalf("concurrent GET = %d, %d bytes", rec.Code, rec.Body.Len())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent GET did not finish")
		}
	}
	if up.count() != 1 {
		t.Fatalf("cross-URL joiner opened another upstream: %d", up.count())
	}
	if got := len(activeVariants(t, store, "mirror://alpha/v2/app/blobs/sha256:"+digest)); got != 1 {
		t.Fatalf("creator variant = %d, want 1", got)
	}
	if got := len(activeVariants(t, store, "mirror://beta/v2/app/blobs/sha256:"+digest)); got != 0 {
		t.Fatalf("joiner committed creator headers to its own URL: %d variants", got)
	}
}
