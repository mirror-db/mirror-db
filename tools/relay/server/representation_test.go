package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestRepresentationDeliveryMatrix drives the same externally observable
// matrix through both representation sources: a stored variant and a live
// active assembly. Anything the two disagree on is a semantic difference in
// delivery, not an implementation detail.
//
// The active side is seeded with a partial range so the object stays
// unpublished: every case must therefore be answered by the active entry, and
// the delivery counter proves it instead of silently landing on a URL variant.
func TestRepresentationDeliveryMatrix(t *testing.T) {
	body := activeBody()
	const lastModified = "Mon, 02 Jan 2006 15:04:05 GMT"
	const octet = "application/octet-stream"

	_, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control": "max-age=3600",
	}))
	ordinary, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors: []Mirror{{Name: "plain", Path: "plain"}},
	})
	active, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "plain", Path: "plain"}},
		policies: activePlainPolicies(),
	})

	tooManyRanges := "bytes=" + strings.Repeat("0-0,", maxMultipartRanges) + "0-0"

	type expectation struct {
		status        int
		body          []byte
		multipart     bool
		parts         []string
		contentType   string
		contentLength string
		contentRange  string
		acceptRanges  string
		etag          string
		age           bool
	}
	type matrixCase struct {
		name string
		req  map[string]string
		want expectation
	}

	full := expectation{
		status:        http.StatusOK,
		body:          body,
		contentType:   octet,
		contentLength: strconv.Itoa(len(body)),
		acceptRanges:  "bytes",
		etag:          `"v1"`,
		age:           true,
	}
	partial := func(payload []byte, contentRange string) expectation {
		return expectation{
			status:        http.StatusPartialContent,
			body:          payload,
			contentType:   octet,
			contentLength: strconv.Itoa(len(payload)),
			contentRange:  contentRange,
			acceptRanges:  "bytes",
			etag:          `"v1"`,
			age:           true,
		}
	}
	notModified := expectation{
		status:       http.StatusNotModified,
		acceptRanges: "bytes",
		etag:         `"v1"`,
		age:          true,
	}

	cases := []matrixCase{
		{name: "full", want: full},
		{name: "if-none-match", req: map[string]string{"If-None-Match": `"v1"`}, want: notModified},
		{name: "if-none-match-miss", req: map[string]string{"If-None-Match": `"other"`}, want: full},
		{name: "if-match", req: map[string]string{"If-Match": `"v1"`}, want: full},
		{name: "if-match-miss", req: map[string]string{"If-Match": `"other"`}, want: expectation{status: http.StatusPreconditionFailed}},
		{name: "if-modified-since", req: map[string]string{"If-Modified-Since": lastModified}, want: notModified},
		{name: "if-unmodified-since", req: map[string]string{"If-Unmodified-Since": "Sun, 01 Jan 2006 00:00:00 GMT"}, want: expectation{status: http.StatusPreconditionFailed}},
		{name: "single-range", req: map[string]string{"Range": "bytes=10-19"}, want: partial(body[10:20], "bytes 10-19/4096")},
		{name: "suffix-range", req: map[string]string{"Range": "bytes=-10"}, want: partial(body[4086:], "bytes 4086-4095/4096")},
		{name: "open-range", req: map[string]string{"Range": "bytes=4090-"}, want: partial(body[4090:], "bytes 4090-4095/4096")},
		{name: "invalid-range", req: map[string]string{"Range": "bytes=abc"}, want: full},
		{name: "multipart-range", req: map[string]string{"Range": "bytes=0-4,10-14"}, want: expectation{
			status:       http.StatusPartialContent,
			multipart:    true,
			parts:        []string{"bytes 0-4/4096:" + string(body[0:5]), "bytes 10-14/4096:" + string(body[10:15])},
			acceptRanges: "bytes",
			etag:         `"v1"`,
			age:          true,
		}},
		{name: "unsatisfiable-range", req: map[string]string{"Range": "bytes=999999-"}, want: expectation{
			status:        http.StatusRequestedRangeNotSatisfiable,
			contentType:   octet,
			contentLength: "0",
			contentRange:  "bytes */4096",
			acceptRanges:  "bytes",
			etag:          `"v1"`,
			age:           true,
		}},
		{name: "too-many-ranges", req: map[string]string{"Range": tooManyRanges}, want: full},
		{name: "if-range-match", req: map[string]string{"Range": "bytes=2-5", "If-Range": `"v1"`}, want: partial(body[2:6], "bytes 2-5/4096")},
		{name: "if-range-weak", req: map[string]string{"Range": "bytes=2-5", "If-Range": `W/"v1"`}, want: full},
		{name: "if-range-wrong-date", req: map[string]string{"Range": "bytes=2-5", "If-Range": "Tue, 03 Jan 2006 15:04:05 GMT"}, want: full},
		{name: "if-range-date", req: map[string]string{"Range": "bytes=2-5", "If-Range": lastModified}, want: partial(body[2:6], "bytes 2-5/4096")},
	}

	assertDelivery := func(t *testing.T, rec *httptest.ResponseRecorder, want expectation) {
		t.Helper()
		if rec.Code != want.status {
			t.Fatalf("status = %d, want %d", rec.Code, want.status)
		}
		if want.multipart {
			mediaType, _, err := mime.ParseMediaType(rec.Header().Get("Content-Type"))
			if err != nil || mediaType != "multipart/byteranges" {
				t.Fatalf("Content-Type = %q (%v)", rec.Header().Get("Content-Type"), err)
			}
			if got := parseMultipartParts(t, rec); strings.Join(got, "|") != strings.Join(want.parts, "|") {
				t.Fatalf("parts = %v, want %v", got, want.parts)
			}
		} else {
			if got := rec.Header().Get("Content-Type"); got != want.contentType {
				t.Fatalf("Content-Type = %q, want %q", got, want.contentType)
			}
			if !bytes.Equal(rec.Body.Bytes(), want.body) {
				t.Fatalf("body = %d bytes, want %d", rec.Body.Len(), len(want.body))
			}
		}
		if got := rec.Header().Get("Content-Length"); got != want.contentLength {
			t.Fatalf("Content-Length = %q, want %q", got, want.contentLength)
		}
		if got := rec.Header().Get("Content-Range"); got != want.contentRange {
			t.Fatalf("Content-Range = %q, want %q", got, want.contentRange)
		}
		if got := rec.Header().Get("Accept-Ranges"); got != want.acceptRanges {
			t.Fatalf("Accept-Ranges = %q, want %q", got, want.acceptRanges)
		}
		if got := rec.Header().Get("ETag"); got != want.etag {
			t.Fatalf("ETag = %q, want %q", got, want.etag)
		}
		age := rec.Header().Get("Age")
		if want.age {
			if _, err := strconv.ParseInt(age, 10, 64); age == "" || err != nil {
				t.Fatalf("Age = %q, want a number", age)
			}
			return
		}
		if age != "" {
			t.Fatalf("Age = %q, want none", age)
		}
	}

	sources := []struct {
		name   string
		srv    *Server
		active bool
	}{
		{name: "ordinary", srv: ordinary},
		{name: "active", srv: active, active: true},
	}
	for _, src := range sources {
		for i, tc := range cases {
			t.Run(src.name+"/"+tc.name, func(t *testing.T) {
				target := fmt.Sprintf("/plain/blob/%s-%d", src.name, i)
				if src.active {
					seed := cacheRequest(src.srv, http.MethodGet, target, map[string]string{"Range": "bytes=0-99"})
					if seed.Code != http.StatusPartialContent || !bytes.Equal(seed.Body.Bytes(), body[:100]) {
						t.Fatalf("active seed = %d, %d bytes", seed.Code, seed.Body.Len())
					}
				} else {
					seed := cacheRequest(src.srv, http.MethodGet, target, nil)
					if seed.Code != http.StatusOK || !bytes.Equal(seed.Body.Bytes(), body) {
						t.Fatalf("ordinary prime = %d, %d bytes", seed.Code, seed.Body.Len())
					}
				}

				before := src.srv.activeDeliveries.Load()
				rec := cacheRequest(src.srv, http.MethodGet, target, tc.req)
				assertDelivery(t, rec, tc.want)
				if !src.active {
					return
				}
				if delta := src.srv.activeDeliveries.Load() - before; delta != 1 {
					t.Fatalf("active deliveries delta = %d, want 1: the response did not come from the active entry", delta)
				}
			})
		}
	}
}

// TestOrdinaryRangeBodyLossRepairsBeforeHeaders is the multipart regression: a
// stored variant whose body vanished must not start a 206 and then discover the
// loss mid-frame. The Range probe happens before any header, so the request is
// repaired through the ordinary miss path (which forwards the client's own
// Range) exactly like the previous ensureBody step.
func TestOrdinaryRangeBodyLossRepairsBeforeHeaders(t *testing.T) {
	body := activeBody()
	dir := t.TempDir()
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"loss"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		if strings.TrimSpace(r.Header.Get("Range")) != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-4/%d", len(body)))
			w.Header().Set("Content-Length", "5")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body[:5])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		dir:     dir,
		mirrors: []Mirror{testMirror()},
	})
	target := "/test/loss"

	if rec := cacheRequest(s, http.MethodGet, target, nil); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("prime = %d, %d bytes", rec.Code, rec.Body.Len())
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

	rec := cacheRequest(s, http.MethodGet, target, map[string]string{"Range": "bytes=0-4,10-14"})
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), body[:5]) {
		t.Fatalf("lost-body multipart = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if mediaType, _, err := mime.ParseMediaType(rec.Header().Get("Content-Type")); err == nil && mediaType == "multipart/byteranges" {
		t.Fatal("truncated multipart body written before the missing body was detected")
	}
	if up.count() != 2 {
		t.Fatalf("lost body did not reach upstream: %d", up.count())
	}

	// The stale variant is gone, so the next plain GET is a cold miss again.
	before := up.count()
	if rec := cacheRequest(s, http.MethodGet, target, nil); rec.Code != http.StatusOK || up.count() != before+1 {
		t.Fatalf("variant survived repair: %d, upstream %d", rec.Code, up.count())
	}
}

// staticRepresentationSource is a complete, always-present representation.
type staticRepresentationSource struct {
	data []byte
}

var _ representationSource = staticRepresentationSource{}

func (s staticRepresentationSource) probe(context.Context) error { return nil }

func (s staticRepresentationSource) open(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

func (s staticRepresentationSource) openSection(_ context.Context, offset, length int64) (io.ReadCloser, error) {
	end := offset + length
	if offset < 0 || end > int64(len(s.data)) || end < offset {
		return nil, errRangeUnsatisfiable
	}
	return io.NopCloser(bytes.NewReader(s.data[offset:end])), nil
}

// readerFromWriter records whether a body copy used ReaderFrom. net/http's
// ResponseWriter implements it, which is the sendfile fast path.
type readerFromWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	readFrom int
}

func (w *readerFromWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *readerFromWriter) WriteHeader(code int) { w.status = code }

func (w *readerFromWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func (w *readerFromWriter) ReadFrom(r io.Reader) (int64, error) {
	w.readFrom++
	return w.body.ReadFrom(r)
}

// TestDeliveryKeepsReaderFromFastPath proves the unattributed ordinary copy
// still reaches io.ReaderFrom (sendfile), while the active tier's error
// attribution path does not. Both deliver the same bytes exactly once.
func TestDeliveryKeepsReaderFromFastPath(t *testing.T) {
	body := []byte("reader-from-body")
	newRep := func(track bool) representation {
		return representation{
			validators:      make(http.Header),
			header:          make(http.Header),
			size:            int64(len(body)),
			acceptRanges:    true,
			trackCopyErrors: track,
			source:          staticRepresentationSource{data: body},
			label:           "fast",
		}
	}

	t.Run("full body", func(t *testing.T) {
		w := &readerFromWriter{}
		res := deliverRepresentation(w, httptest.NewRequest(http.MethodGet, "/fast", nil), newRep(false))
		if !res.Delivered || !res.BodyComplete || !res.WholeRepresentation {
			t.Fatalf("result = %+v", res)
		}
		if w.readFrom != 1 {
			t.Fatalf("ReaderFrom calls = %d, want 1", w.readFrom)
		}
		if !bytes.Equal(w.body.Bytes(), body) {
			t.Fatalf("body = %q", w.body.Bytes())
		}
	})

	t.Run("single range", func(t *testing.T) {
		w := &readerFromWriter{}
		req := httptest.NewRequest(http.MethodGet, "/fast", nil)
		req.Header.Set("Range", "bytes=2-5")
		res := deliverRepresentation(w, req, newRep(false))
		if !res.Delivered || !res.BodyComplete {
			t.Fatalf("result = %+v", res)
		}
		if w.readFrom != 1 {
			t.Fatalf("ReaderFrom calls = %d, want 1", w.readFrom)
		}
		if !bytes.Equal(w.body.Bytes(), body[2:6]) {
			t.Fatalf("body = %q", w.body.Bytes())
		}
	})

	t.Run("active attribution", func(t *testing.T) {
		w := &readerFromWriter{}
		res := deliverRepresentation(w, httptest.NewRequest(http.MethodGet, "/fast", nil), newRep(true))
		if !res.Delivered || !res.BodyComplete {
			t.Fatalf("result = %+v", res)
		}
		if w.readFrom != 0 {
			t.Fatalf("ReaderFrom calls = %d, want 0 when copy errors are attributed", w.readFrom)
		}
		if !bytes.Equal(w.body.Bytes(), body) {
			t.Fatalf("body = %q", w.body.Bytes())
		}
	})
}
