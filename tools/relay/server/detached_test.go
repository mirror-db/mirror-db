package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/activecache"
	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// blockingAdopter holds publication open until the test releases it. It is the
// active segment's publication destination, so the active path can be observed
// between "client has every byte" and "the body has an address".
type blockingAdopter struct {
	entered chan struct{}
	release chan struct{}
	enter   sync.Once
	letGo   sync.Once

	mu    sync.Mutex
	store *hashstore.Store
}

func newBlockingAdopter() *blockingAdopter {
	return &blockingAdopter{entered: make(chan struct{}), release: make(chan struct{})}
}

func (a *blockingAdopter) attach(store *hashstore.Store) {
	a.mu.Lock()
	a.store = store
	a.mu.Unlock()
}

func (a *blockingAdopter) open() { a.enter.Do(func() { close(a.entered) }) }

func (a *blockingAdopter) releaseAll() { a.letGo.Do(func() { close(a.release) }) }

func (a *blockingAdopter) Adopt(ctx context.Context, path string, size int64, algo hashstore.Algorithm, digest string) (hashstore.Content, error) {
	a.open()
	select {
	case <-a.release:
	case <-ctx.Done():
		return hashstore.Content{}, ctx.Err()
	}
	a.mu.Lock()
	store := a.store
	a.mu.Unlock()
	if store == nil {
		return hashstore.Content{}, errors.New("blocking adopter has no store")
	}
	return store.Adopt(ctx, path, size, algo, digest)
}

// await fails the test when a signal never arrives. It is a bounded test wait,
// not a timing assertion.
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A complete active GET flushes and returns while publication is still held: the
// client's EOF never waits for the store. Releasing publication then writes the
// URL variant from the detached waiter.
func TestActiveCompleteGetReturnsBeforePublish(t *testing.T) {
	body := activeBody()
	blocker := newBlockingAdopter()
	t.Cleanup(blocker.releaseAll)

	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control": "max-age=3600",
	}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
		activeMutate: func(c *activecache.Config) {
			c.Adopt = blocker
		},
	})
	blocker.attach(store)

	digest := bodyDigestHex(string(body))
	target := "/docker/v2/app/blobs/sha256:" + digest
	key := "mirror://docker/v2/app/blobs/sha256:" + digest

	req := httptest.NewRequest(http.MethodGet, "http://relay.example.com"+target, nil)
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		s.handleRequest(rec, req)
		close(done)
	}()

	await(t, blocker.entered, "publication to start")
	await(t, done, "the handler to return")
	if rec.Body.Len() != len(body) || rec.Body.String() != string(body) {
		t.Fatalf("delivered body = %d bytes", rec.Body.Len())
	}
	if !rec.flushed {
		t.Fatal("a complete response was not flushed")
	}
	if got := len(activeVariants(t, store, key)); got != 0 {
		t.Fatalf("a variant appeared before publication: %d", got)
	}

	blocker.releaseAll()
	s.background().drain()

	variants := activeVariants(t, store, key)
	if len(variants) != 1 || variants[0].BodySHA256 != digest {
		t.Fatalf("variant after release = %+v", variants)
	}
	if got := s.activeCommits.size(); got != 0 {
		t.Fatalf("publication waiter still registered after publish: %d", got)
	}
	if up.count() != 1 {
		t.Fatalf("upstream requests = %d, want 1", up.count())
	}
}

// The ordinary tee's Put is held inside its publish window. The handler still
// returns once the client has every byte, and the variant appears only after the
// store finishes. Both a declared length and a chunked response are covered,
// because only returning from the handler emits the chunked EOF.
func TestOrdinaryCompleteGetReturnsBeforeStore(t *testing.T) {
	cases := []struct {
		name   string
		length bool
	}{
		{"content-length", true},
		{"chunked", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Repeat("ordinary-store-body-", 1024)
			entered := make(chan struct{})
			release := make(chan struct{})
			var enterOnce sync.Once
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

			up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"ordinary"`)
				w.Header().Set("Cache-Control", "max-age=3600")
				if tc.length {
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			})
			s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
				mirrors: []Mirror{testMirror()},
				storeMutate: func(c *hashstore.Config) {
					c.TestAfterPublish = func() {
						enterOnce.Do(func() { close(entered) })
						<-release
					}
				},
			})

			req := httptest.NewRequest(http.MethodGet, "http://relay.example.com/test/held", nil)
			rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
			done := make(chan struct{})
			go func() {
				s.handleRequest(rec, req)
				close(done)
			}()

			await(t, entered, "the store publish window")
			await(t, done, "the handler to return")
			if rec.Body.String() != body {
				t.Fatalf("delivered body = %d bytes", rec.Body.Len())
			}
			if !rec.flushed {
				t.Fatal("a complete response was not flushed")
			}
			if got := len(activeVariants(t, store, "mirror://test/held")); got != 0 {
				t.Fatalf("a variant appeared before the store finished: %d", got)
			}
			if up.count() != 1 {
				t.Fatalf("upstream requests = %d, want 1", up.count())
			}

			releaseOnce.Do(func() { close(release) })
			s.background().drain()

			variants := activeVariants(t, store, "mirror://test/held")
			if len(variants) != 1 || variants[0].BodySize != int64(len(body)) {
				t.Fatalf("variant after store finished = %+v", variants)
			}
		})
	}
}

// A client that stops early leaves no variant, even though the handler has long
// returned before the background task finishes.
func TestOrdinaryAbortedClientStillWritesNoVariant(t *testing.T) {
	body := strings.Repeat("aborted-body-", 4096)
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"aborted"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})

	req := httptest.NewRequest(http.MethodGet, "http://relay.example.com/test/aborted", nil)
	aw := &abortWriter{header: make(http.Header), limit: 64}
	s.handleRequest(aw, req)
	s.background().drain()

	if got := len(activeVariants(t, store, "mirror://test/aborted")); got != 0 {
		t.Fatalf("an aborted client wrote %d variants", got)
	}
	// The store may hold its own body, but no URL variant may name it.
	rec := cacheRequest(s, http.MethodGet, "/test/aborted", nil)
	if rec.Body.String() != body {
		t.Fatalf("refetch body = %d bytes", rec.Body.Len())
	}
	if up.count() != 2 {
		t.Fatalf("aborted body was reused through a variant: upstream %d", up.count())
	}
}

// Concurrent readers of one assembly share one publication waiter, and it is
// gone once the entry settles.
func TestActivePublishWaiterIsDeduplicated(t *testing.T) {
	body := activeBody()
	blocker := newBlockingAdopter()
	t.Cleanup(blocker.releaseAll)

	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control": "max-age=3600",
	}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
		activeMutate: func(c *activecache.Config) {
			c.Adopt = blocker
		},
	})
	blocker.attach(store)

	digest := bodyDigestHex(string(body))
	target := "/docker/v2/app/blobs/sha256:" + digest
	key := "mirror://docker/v2/app/blobs/sha256:" + digest

	const readers = 4
	recs := make([]*httptest.ResponseRecorder, readers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			recs[i] = cacheRequestNoDrain(s, http.MethodGet, target)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, rec := range recs {
		if rec.Code != http.StatusOK || rec.Body.String() != string(body) {
			t.Fatalf("reader %d = %d, %d bytes", i, rec.Code, rec.Body.Len())
		}
	}
	await(t, blocker.entered, "publication to start")
	if got := s.activeCommits.size(); got != 1 {
		t.Fatalf("publication waiters = %d, want 1", got)
	}

	blocker.releaseAll()
	s.background().drain()

	if got := s.activeCommits.size(); got != 0 {
		t.Fatalf("publication waiter leaked after settle: %d", got)
	}
	if got := len(activeVariants(t, store, key)); got != 1 {
		t.Fatalf("variants = %d, want 1", got)
	}
	if up.count() != 1 {
		t.Fatalf("upstream requests = %d, want 1", up.count())
	}
}

// cacheRequestNoDrain runs the handler without waiting for detached work, so a
// test can inspect the state while publication is held.
func cacheRequestNoDrain(s *Server, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://relay.example.com"+target, nil)
	rec := httptest.NewRecorder()
	s.handleRequest(rec, req)
	return rec
}

// A panic inside a detached task is contained: the task settles and Close does
// not hang on work that already died.
func TestBackgroundTaskPanicIsContained(t *testing.T) {
	b := newBgTasks()
	panicked := make(chan struct{})
	if !b.start(func() {
		defer close(panicked)
		panic("detached boom")
	}) {
		t.Fatal("task was refused")
	}
	await(t, panicked, "the panicking task to run")
	// drain returns only after the task's done; with no recovery it would
	// unwind the whole test binary instead.
	b.close()
}

// Server.Close drains detached work before closing the store. It must not
// return while a task is still blocked, and must return once that task is
// released; deleting the background drain would fail the first assertion.
func TestServerCloseWaitsForDetachedWorkBeforeStore(t *testing.T) {
	_, upSrv := startUpstream(t, activeUpstreamHandler(activeBody(), nil))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})

	release := make(chan struct{})
	started := make(chan struct{})
	if !s.background().start(func() {
		close(started)
		<-release
	}) {
		t.Fatal("task was refused")
	}
	await(t, started, "the background task")

	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Server.Close returned while a detached task was still running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	await(t, closed, "Server.Close after the task finished")

	if _, _, err := s.active.Acquire(context.Background(), "probe"); !errors.Is(err, activecache.ErrClosed) {
		t.Fatalf("active Acquire after Close = %v, want ErrClosed", err)
	}
	if _, err := store.Variants(context.Background(), "probe"); !errors.Is(err, hashstore.ErrClosed) {
		t.Fatalf("hash store after Close = %v, want ErrClosed", err)
	}
	if got := s.activeCommits.size(); got != 0 {
		t.Fatalf("publication waiter survived Close: %d", got)
	}
}

// stallingWriter accepts bytes quickly until stallAt and then blocks until the
// test releases it. It models a slow client that keeps the handler alive long
// after the store has given up.
type stallingWriter struct {
	header  http.Header
	stallAt int
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once

	mu      sync.Mutex
	written int
}

func (w *stallingWriter) Header() http.Header { return w.header }

func (w *stallingWriter) WriteHeader(int) {}

func (w *stallingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.written += len(p)
	stall := w.written >= w.stallAt
	w.mu.Unlock()
	if stall {
		w.once.Do(func() { close(w.entered) })
		<-w.gate
	}
	return len(p), nil
}

// A Put that already failed must not make Server.Close wait for a stalled
// client: the tee task selects on the closing signal instead of the delivery
// outcome, because the store is done and no variant may be written.
func TestServerCloseIgnoresStalledClientAfterPutFails(t *testing.T) {
	body := bytes.Repeat([]byte("z"), 64<<10)
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"stall"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		// No Content-Length: the store tees an unknown-length body.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	// The store budget is smaller than the first 1 KiB, so Put fails as soon as
	// it has read limit+1 bytes; the client is still streaming.
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors: []Mirror{testMirror()},
		maxSize: 1024,
	})

	writer := &stallingWriter{
		header:  make(http.Header),
		stallAt: 40 << 10,
		gate:    make(chan struct{}),
		entered: make(chan struct{}),
	}
	handlerDone := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://relay.example.com/test/stalled", nil)
		s.handleRequest(writer, req)
		close(handlerDone)
	}()

	await(t, writer.entered, "the client to stall")
	// By now Put has read past its 1 KiB limit and returned; only the handler is
	// still alive, blocked on the client.
	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()
	await(t, closed, "Server.Close with a stalled client")

	select {
	case <-handlerDone:
		t.Fatal("the handler was not blocked on its client")
	default:
	}

	close(writer.gate)
	await(t, handlerDone, "the handler after the client is released")
	if up.count() != 1 {
		t.Fatalf("upstream requests = %d, want 1", up.count())
	}
	if _, err := store.Variants(context.Background(), "probe"); !errors.Is(err, hashstore.ErrClosed) {
		t.Fatalf("hash store after Close = %v, want ErrClosed", err)
	}
}

// Server.Close settles the active manager first, which wakes a publication
// waiter blocked in publication, and only then drains and closes the store.
func TestServerCloseSettlesActivePublicationWaiter(t *testing.T) {
	body := activeBody()
	blocker := newBlockingAdopter()
	// No release: only Manager.Close's context cancellation can end the adopt,
	// which is exactly the shutdown path this test pins.

	_, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control": "max-age=3600",
	}))
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
		activeMutate: func(c *activecache.Config) {
			c.Adopt = blocker
		},
	})
	blocker.attach(store)

	digest := bodyDigestHex(string(body))
	target := "/docker/v2/app/blobs/sha256:" + digest
	rec := cacheRequestNoDrain(s, http.MethodGet, target)
	if rec.Code != http.StatusOK || rec.Body.String() != string(body) {
		t.Fatalf("GET before close = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	await(t, blocker.entered, "publication to start")

	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()
	await(t, closed, "Server.Close")

	if _, _, err := s.active.Acquire(context.Background(), "probe"); !errors.Is(err, activecache.ErrClosed) {
		t.Fatalf("active Acquire after Close = %v, want ErrClosed", err)
	}
	if _, err := store.Variants(context.Background(), "probe"); !errors.Is(err, hashstore.ErrClosed) {
		t.Fatalf("hash store after Close = %v, want ErrClosed", err)
	}
	if got := s.activeCommits.size(); got != 0 {
		t.Fatalf("publication waiter survived Close: %d", got)
	}
}
