package server

import (
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseMultipartParts returns each part's Content-Range header and payload.
func parseMultipartParts(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(rec.Header().Get("Content-Type"))
	if err != nil || mediaType != "multipart/byteranges" {
		t.Fatalf("multipart type = %q %v", mediaType, err)
	}
	reader := multipart.NewReader(rec.Body, params["boundary"])
	var parts []string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return parts
		}
		if err != nil {
			t.Fatalf("part: %v", err)
		}
		data, _ := io.ReadAll(part)
		parts = append(parts, part.Header.Get("Content-Range")+":"+string(data))
	}
}

// 1: more than maxMultipartRanges intervals must be ignored (full 200), not
// answered as an unbounded multipart body and not as a 416. Repeated intervals
// count individually.
func TestCacheMultipartRangeLimit(t *testing.T) {
	body := strings.Repeat("0123456789", 10) // 100 bytes
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, body, map[string]string{"Cache-Control": "max-age=3600"})
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	cacheRequest(s, http.MethodGet, "/test/limits", nil)

	distinct := make([]string, maxMultipartRanges)
	for i := range distinct {
		distinct[i] = fmt.Sprintf("%d-%d", i, i)
	}
	rangeField := "bytes=" + strings.Join(distinct, ",")

	atLimit := cacheRequest(s, http.MethodGet, "/test/limits", map[string]string{
		"Range": rangeField,
	})
	if atLimit.Code != http.StatusPartialContent {
		t.Fatalf("%d ranges = %d, want 206", len(distinct), atLimit.Code)
	}
	if parts := parseMultipartParts(t, atLimit); len(parts) != maxMultipartRanges {
		t.Fatalf("multipart parts = %d, want %d", len(parts), maxMultipartRanges)
	}

	overLimit := rangeField + fmt.Sprintf(",%d-%d", len(distinct), len(distinct))
	rec := cacheRequest(s, http.MethodGet, "/test/limits", map[string]string{
		"Range": overLimit,
	})
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("%d ranges = %d body %q", len(distinct)+1, rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "" {
		t.Fatalf("ignored Range still sent Content-Range %q", got)
	}
	if mediaType, _, _ := mime.ParseMediaType(rec.Header().Get("Content-Type")); mediaType == "multipart/byteranges" {
		t.Fatal("ignored Range produced a multipart response")
	}

	dupes := make([]string, maxMultipartRanges+1)
	for i := range dupes {
		dupes[i] = "0-0"
	}
	rec = cacheRequest(s, http.MethodGet, "/test/limits", map[string]string{
		"Range": "bytes=" + strings.Join(dupes, ","),
	})
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("%d duplicate ranges = %d body %q", len(dupes), rec.Code, rec.Body.String())
	}

	if up.count() != 1 {
		t.Fatalf("range limit requests reached the upstream: %d", up.count())
	}
}

// 2a: a declared length above the store budget is delivered in full but never
// teed, so no temporary file and no variant is created.
func TestCacheKnownOversizedNotStored(t *testing.T) {
	const body = "0123456789" // 10 bytes, budget 8
	dir := t.TempDir()
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, body, map[string]string{"Cache-Control": "max-age=3600"})
	})
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		dir:     dir,
		maxSize: 8,
		mirrors: []Mirror{testMirror()},
	})

	rec := cacheRequest(s, http.MethodGet, "/test/oversized", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("oversized response = %d %q", rec.Code, rec.Body.String())
	}
	if got := store.Stats().Objects; got != 0 {
		t.Fatalf("oversized body created %d objects", got)
	}
	variants, err := store.Variants(context.Background(), cacheKey(testMirror(), "/oversized", nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 0 {
		t.Fatalf("oversized body created %d variants", len(variants))
	}
	entries, err := os.ReadDir(filepath.Join(dir, ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("oversized body left %d temporary files", len(entries))
	}

	// Nothing was stored, so a second request goes upstream again.
	rec = cacheRequest(s, http.MethodGet, "/test/oversized", nil)
	if rec.Body.String() != body {
		t.Fatalf("second oversized response = %q", rec.Body.String())
	}
	if up.count() != 2 {
		t.Fatalf("oversized body was cached: upstream %d", up.count())
	}
}

// 2b: a declared length within the budget takes the normal tee-and-store path.
func TestCacheKnownLengthStored(t *testing.T) {
	const body = "12345678" // exactly the 8-byte budget
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, body, map[string]string{"Cache-Control": "max-age=3600"})
	})
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		maxSize: 8,
		mirrors: []Mirror{testMirror()},
	})

	if rec := cacheRequest(s, http.MethodGet, "/test/at-budget", nil); rec.Body.String() != body {
		t.Fatalf("first response = %q", rec.Body.String())
	}
	variants, err := store.Variants(context.Background(), cacheKey(testMirror(), "/at-budget", nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 1 || variants[0].BodySize != int64(len(body)) {
		t.Fatalf("at-budget variant = %+v", variants)
	}
	if rec := cacheRequest(s, http.MethodGet, "/test/at-budget", nil); rec.Body.String() != body {
		t.Fatalf("second response = %q", rec.Body.String())
	}
	if up.count() != 1 {
		t.Fatalf("at-budget body did not come from the cache: upstream %d", up.count())
	}
}

// 3: a zero-byte stored body cannot satisfy any range; the answer is 416 with
// the complete-length form of Content-Range.
func TestCacheEmptyBodyRange(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "", map[string]string{"Cache-Control": "max-age=3600"})
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	cacheRequest(s, http.MethodGet, "/test/empty", nil)

	for _, spec := range []string{"bytes=0-", "bytes=0-0", "bytes=-5"} {
		rec := cacheRequest(s, http.MethodGet, "/test/empty", map[string]string{"Range": spec})
		if rec.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("%q = %d, want 416", spec, rec.Code)
		}
		if got := rec.Header().Get("Content-Range"); got != "bytes */0" {
			t.Fatalf("%q Content-Range = %q", spec, got)
		}
		if got := rec.Header().Get("Content-Length"); got != "0" {
			t.Fatalf("%q Content-Length = %q", spec, got)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("%q body = %q", spec, rec.Body.String())
		}
	}
	if up.count() != 1 {
		t.Fatalf("empty-body range reached the upstream: %d", up.count())
	}
}
