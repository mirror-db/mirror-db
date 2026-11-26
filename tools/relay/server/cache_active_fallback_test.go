package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tiered "github.com/geektr-cloud/go-tiered-cache"
	httpadapter "github.com/geektr-cloud/go-tiered-cache/adapters/http"
	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// The immutable active segment assembles one object from several upstream
// requests. When a hole must be filled and the origin refuses byte ranges — it
// answers 200 to a Range request, or the range request fails — the active source
// falls back to a complete GET and discards the prefix while streaming. These
// tests pin that behaviour on both delivery shapes and prove the entry survives
// the range refusal instead of handing the client a truncated 200/206.

// activeFallbackBody is 256 KiB of deterministic bytes: large enough that a
// 32 KiB client write limit leaves a real hole behind.
func activeFallbackBody() []byte {
	body := make([]byte, 256<<10)
	for i := range body {
		body[i] = byte(i % 251)
	}
	return body
}

type fallbackRequest struct {
	rangeHeader   string
	cookie        string
	authorization string
}

// fallbackOrigin's first response is a slow 200 that stalls after half the body,
// so a client can disconnect with a partial extent already cached. Later
// requests answer Range according to rangeStatus (0 honours it, 200 ignores it,
// any other value is the error status) and a plain GET with the complete body.
type fallbackOrigin struct {
	body        []byte
	rangeStatus int
	failAll     atomic.Bool

	mu       sync.Mutex
	requests []fallbackRequest

	started chan struct{}
	release chan struct{}
	gone    chan struct{}

	startOnce sync.Once
	goneOnce  sync.Once
}

func newFallbackOrigin(body []byte, rangeStatus int) *fallbackOrigin {
	return &fallbackOrigin{
		body:        body,
		rangeStatus: rangeStatus,
		started:     make(chan struct{}),
		release:     make(chan struct{}),
		gone:        make(chan struct{}),
	}
}

func (o *fallbackOrigin) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.requests)
}

func (o *fallbackOrigin) recorded() []fallbackRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]fallbackRequest(nil), o.requests...)
}

func (o *fallbackOrigin) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.requests = append(o.requests, fallbackRequest{
			rangeHeader:   r.Header.Get("Range"),
			cookie:        r.Header.Get("Cookie"),
			authorization: r.Header.Get("Authorization"),
		})
		n := len(o.requests)
		o.mu.Unlock()

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"gated"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		if o.rangeStatus == 0 {
			w.Header().Set("Accept-Ranges", "bytes")
		}

		if n == 1 {
			o.serveSlowSeed(w, r)
			return
		}
		if o.failAll.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if r.Header.Get("Range") != "" {
			if o.rangeStatus == http.StatusOK {
				// The origin ignored Range and sent the complete representation.
				o.serveBody(w)
				return
			}
			http.Error(w, "boom", o.rangeStatus)
			return
		}
		o.serveBody(w)
	}
}

func (o *fallbackOrigin) serveSlowSeed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Length", strconv.Itoa(len(o.body)))
	w.WriteHeader(http.StatusOK)
	half := len(o.body) / 2
	if _, err := w.Write(o.body[:half]); err != nil {
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	o.startOnce.Do(func() { close(o.started) })
	select {
	case <-o.release:
	case <-r.Context().Done():
		o.goneOnce.Do(func() { close(o.gone) })
		return
	}
	_, _ = w.Write(o.body[half:])
}

func (o *fallbackOrigin) serveBody(w http.ResponseWriter) {
	w.Header().Set("Content-Length", strconv.Itoa(len(o.body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(o.body)
}

// fallbackAbortWriter emulates a client that disconnects after limit bytes: the
// write fails and the request context is cancelled, like a dropped connection.
type fallbackAbortWriter struct {
	header  http.Header
	status  int
	limit   int
	written int
	cancel  context.CancelFunc
	mu      sync.Mutex
}

func (w *fallbackAbortWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *fallbackAbortWriter) WriteHeader(code int) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = code
	}
	w.mu.Unlock()
}

func (w *fallbackAbortWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.written >= w.limit {
		w.cancel()
		return 0, errors.New("client gone")
	}
	n := len(p)
	if w.written+n > w.limit {
		n = w.limit - w.written
	}
	w.written += n
	if n < len(p) {
		w.cancel()
		return n, errors.New("client gone")
	}
	return n, nil
}

func (w *fallbackAbortWriter) statusCode() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func abortActiveGet(s *Server, target string, limit int, o *fallbackOrigin) error {
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "http://relay.example.com"+target, nil).WithContext(ctx)
	w := &fallbackAbortWriter{limit: limit, cancel: cancel}
	s.handleRequest(w, req)
	if got := w.statusCode(); got != http.StatusOK {
		return fmt.Errorf("aborted GET status = %d", got)
	}
	select {
	case <-o.gone:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("relay never cancelled its upstream body after the client left")
	}
}

type abortedActiveEntry struct {
	body   []byte
	target string
	key    string
	server *Server
	store  *hashstore.Store
	origin *fallbackOrigin
}

// setupAbortedActiveEntry seeds an immutable entry with a full 200 whose first
// client disconnects after 32 KiB. The origin's seed body is released, so the
// partial extent is fixed before the next request.
func setupAbortedActiveEntry(t *testing.T, rangeStatus int) *abortedActiveEntry {
	t.Helper()
	body := activeFallbackBody()
	origin := newFallbackOrigin(body, rangeStatus)
	upSrv := httptest.NewServer(origin.handler())
	t.Cleanup(upSrv.Close)

	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	digest := bodyDigestHex(string(body))
	e := &abortedActiveEntry{
		body:   body,
		target: "/docker/v2/app/blobs/sha256:" + digest,
		key:    "mirror://docker/v2/app/blobs/sha256:" + digest,
		server: s,
		store:  store,
		origin: origin,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- abortActiveGet(s, e.target, 32<<10, origin) }()
	select {
	case <-origin.started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never sent the first half")
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	close(origin.release)
	return e
}

// TestActiveHoleRangeUnavailableFallsBackToFullGet covers both shapes of the
// truncation: a full GET and a late Range on the same partial entry, each
// against an origin that ignores Range and one that fails it. The first
// post-abort response must carry every byte, the entry must publish once whole,
// and the following request must be local.
func TestActiveHoleRangeUnavailableFallsBackToFullGet(t *testing.T) {
	cases := []struct {
		name        string
		rangeStatus int
	}{
		{"range ignored", http.StatusOK},
		{"range 500", http.StatusInternalServerError},
	}
	shapes := []struct {
		name    string
		isRange bool
	}{
		{"full get", false},
		{"late range", true},
	}

	for _, tc := range cases {
		for _, shape := range shapes {
			t.Run(tc.name+"/"+shape.name, func(t *testing.T) {
				e := setupAbortedActiveEntry(t, tc.rangeStatus)

				// A relayed request must never forward the client's credentials
				// to a later range or fallback request.
				headers := map[string]string{
					"Cookie":        "session=secret",
					"Authorization": "Bearer secret",
				}
				var second *httptest.ResponseRecorder
				if shape.isRange {
					tail := len(e.body) - 1024
					headers["Range"] = fmt.Sprintf("bytes=%d-%d", tail, len(e.body)-1)
					second = cacheRequest(e.server, http.MethodGet, e.target, headers)
					if second.Code != http.StatusPartialContent || !bytes.Equal(second.Body.Bytes(), e.body[tail:]) {
						t.Fatalf("late range after abort = %d, %d bytes (want 1024)", second.Code, second.Body.Len())
					}
				} else {
					second = cacheRequest(e.server, http.MethodGet, e.target, headers)
					if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), e.body) {
						t.Fatalf("full get after abort = %d, %d bytes (want %d)", second.Code, second.Body.Len(), len(e.body))
					}
				}

				// One range attempt established the refusal, then one complete
				// GET produced the section. A refusal that was answered with the
				// full body (200) is discarded; the fallback re-reads with an
				// explicit complete GET.
				reqs := e.origin.recorded()
				if want := 3; len(reqs) != want {
					t.Fatalf("upstream requests = %d (%v), want %d", len(reqs), reqs, want)
				}
				if reqs[1].rangeHeader == "" {
					t.Fatalf("second request did not try a range: %v", reqs)
				}
				if reqs[2].rangeHeader != "" {
					t.Fatalf("fallback was not a complete GET: %v", reqs)
				}
				for i, req := range reqs[1:] {
					if req.cookie != "" || req.authorization != "" {
						t.Fatalf("request %d leaked credentials: cookie=%q authorization=%q", i+1, req.cookie, req.authorization)
					}
				}

				if shape.isRange {
					// A range delivery does not publish; a complete GET fills the
					// remaining hole and then the object is local.
					full := cacheRequest(e.server, http.MethodGet, e.target, nil)
					if full.Code != http.StatusOK || !bytes.Equal(full.Body.Bytes(), e.body) {
						t.Fatalf("full get after hole = %d, %d bytes", full.Code, full.Body.Len())
					}
					if last := e.origin.recorded()[e.origin.count()-1]; last.rangeHeader != "" {
						t.Fatalf("last hole fill was not a complete GET: %v", e.origin.recorded())
					}
				}
				if got := len(activeVariants(t, e.store, e.key)); got != 1 {
					t.Fatalf("published variants = %d, want 1", got)
				}

				before := e.origin.count()
				again := cacheRequest(e.server, http.MethodGet, e.target, nil)
				if again.Code != http.StatusOK || !bytes.Equal(again.Body.Bytes(), e.body) {
					t.Fatalf("local hit = %d, %d bytes", again.Code, again.Body.Len())
				}
				if got := e.origin.count(); got != before {
					t.Fatalf("published body still hit upstream: %d -> %d", before, got)
				}
			})
		}
	}
}

// TestActiveFailedFallbackEndsEntry proves the other half of the contract: when
// the complete GET fails too, the entry is terminal and a later request builds
// a fresh one instead of being served the truncated bytes.
func TestActiveFailedFallbackEndsEntry(t *testing.T) {
	e := setupAbortedActiveEntry(t, http.StatusInternalServerError)
	e.origin.failAll.Store(true)

	second := cacheRequest(e.server, http.MethodGet, e.target, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("failed hole fill status = %d", second.Code)
	}
	if bytes.Equal(second.Body.Bytes(), e.body) {
		t.Fatal("failed hole fill delivered the complete body")
	}
	if got := second.Header().Get("Content-Length"); got != strconv.Itoa(len(e.body)) {
		t.Fatalf("failed hole fill Content-Length = %q, want %d", got, len(e.body))
	}

	e.origin.failAll.Store(false)
	third := cacheRequest(e.server, http.MethodGet, e.target, nil)
	if third.Code != http.StatusOK || !bytes.Equal(third.Body.Bytes(), e.body) {
		t.Fatalf("recovery get = %d, %d bytes (want %d)", third.Code, third.Body.Len(), len(e.body))
	}
	if got := len(activeVariants(t, e.store, e.key)); got != 1 {
		t.Fatalf("recovered variants = %d, want 1", got)
	}
}

// TestActiveSourceFullGetFallbackShortBody checks the section wrapper reports a
// body that ends inside the discarded prefix as io.ErrUnexpectedEOF instead of
// returning a short section.
func TestActiveSourceFullGetFallbackShortBody(t *testing.T) {
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("0123456789"))
	}))
	t.Cleanup(upSrv.Close)

	adapter, err := httpadapter.NewUpstreamFromURL(upSrv.URL)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	src := &activeSource{upstream: adapter, size: 100}

	rc, err := src.ReadSection(context.Background(), 50, 20, nil)
	if err != nil {
		t.Fatalf("establish fallback: %v", err)
	}
	_, err = io.ReadAll(rc)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short body error = %v, want io.ErrUnexpectedEOF", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestActiveSourceFallbackBounds rejects a section outside the known extent
// before spending a complete GET on it.
func TestActiveSourceFallbackBounds(t *testing.T) {
	var full atomic.Int32
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		full.Add(1)
		w.Header().Set("Content-Length", "64")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 64))
	}))
	t.Cleanup(upSrv.Close)

	adapter, err := httpadapter.NewUpstreamFromURL(upSrv.URL)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	src := &activeSource{upstream: adapter, size: 64}

	if _, err := src.ReadSection(context.Background(), 64, 8, nil); !errors.Is(err, tiered.ErrOutOfRange) {
		t.Fatalf("offset at extent = %v, want ErrOutOfRange", err)
	}
	if _, err := src.ReadSection(context.Background(), 32, 40, nil); !errors.Is(err, tiered.ErrInvalidRange) {
		t.Fatalf("section past extent = %v, want ErrInvalidRange", err)
	}
	if got := full.Load(); got != 0 {
		t.Fatalf("out-of-range sections issued %d complete GETs", got)
	}
}

// TestActiveSourceFallbackOpenFailure checks that a failing complete GET keeps
// the upstream status visible and does not retry the range.
func TestActiveSourceFallbackOpenFailure(t *testing.T) {
	var requests atomic.Int32
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(upSrv.Close)

	adapter, err := httpadapter.NewUpstreamFromURL(upSrv.URL)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	src := &activeSource{upstream: adapter, size: 64}

	if _, err := src.ReadSection(context.Background(), 0, 32, nil); err == nil {
		t.Fatal("failing fallback returned a reader")
	} else {
		var statusErr *httpadapter.HTTPStatusError
		if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusInternalServerError {
			t.Fatalf("fallback error = %v, want HTTPStatusError 500", err)
		}
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("upstream requests = %d, want 2 (one range, one complete GET)", got)
	}
}

// TestActiveSourceFallbackCancellationClosesBody checks the fallback body is
// released when the plan's context is cancelled, not left for the server to
// time out.
func TestActiveSourceFallbackCancellationClosesBody(t *testing.T) {
	var (
		gone    = make(chan struct{})
		release = make(chan struct{})
		once    sync.Once
	)
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
			once.Do(func() { close(gone) })
		}
	}))
	t.Cleanup(upSrv.Close)

	adapter, err := httpadapter.NewUpstreamFromURL(upSrv.URL)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	src := &activeSource{upstream: adapter, size: 1024}

	ctx, cancel := context.WithCancel(context.Background())
	rc, err := src.ReadSection(ctx, 0, 1024, nil)
	if err != nil {
		t.Fatalf("establish fallback: %v", err)
	}
	cancel()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("fallback body still open after cancellation")
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
