package activecache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	tiered "github.com/geektr-cloud/go-tiered-cache"
	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

type entryState int

const (
	statePending entryState = iota
	stateInitializing
	stateReady
	statePublished
	stateFailed
	stateClosed
)

// Entry is one immutable object being assembled. Acquire returns it with one
// reference; call Release when the body is no longer needed. The creator must
// call Init (or Fail) before other Acquire callers can proceed.
type Entry struct {
	key     string
	algo    hashstore.Algorithm
	digest  string
	m       *Manager
	created time.Time

	mu           sync.Mutex
	state        entryState
	refs         int
	ready        chan struct{}
	readyClosed  bool
	terminalErr  error
	metadata     any
	declaredSize int64
	pc           *tiered.PassiveCache
	cache        *scratchCache
	store        *entryStore
	content      hashstore.Content
	source       io.Closer

	// settled is closed once the entry can no longer become published: it was
	// published, failed during publication, or was closed. WaitPublished blocks
	// on it so a caller that finished delivering a body can learn the outcome
	// without polling.
	settled    chan struct{}
	settleOnce sync.Once

	// retireScheduled is guarded by mu and keeps maybeRetire from spawning
	// duplicate retirement goroutines.
	retireScheduled bool
	closeOnce       sync.Once

	// accounted and unknown are guarded by the manager lock.
	accounted int64
	unknown   bool
}

func newEntry(m *Manager, key string) *Entry {
	algo, digest := parseDigestKey(key)
	return &Entry{
		key:     key,
		algo:    algo,
		digest:  digest,
		m:       m,
		created: time.Now(),
		ready:   make(chan struct{}),
		settled: make(chan struct{}),
	}
}

// parseDigestKey extracts an expected algorithm and digest from a "sha256:<hex>"
// or "sha1:<hex>" key. Any other key is opaque: it deduplicates requests but
// Adopt publishes the real digest without an expectation.
func parseDigestKey(key string) (hashstore.Algorithm, string) {
	idx := strings.IndexByte(key, ':')
	if idx < 0 {
		return "", ""
	}
	algo, err := hashstore.ParseAlgorithm(key[:idx])
	if err != nil {
		return "", ""
	}
	digest, err := hashstore.NormalizeDigest(algo, key[idx+1:])
	if err != nil {
		return "", ""
	}
	return algo, digest
}

// Key returns the deduplication key this entry was acquired with.
func (e *Entry) Key() string { return e.key }

// Wait blocks until the creator initialized the entry or failed it. A creator
// does not need it; Acquire already waited for non-creators.
func (e *Entry) Wait(ctx context.Context) error { return e.wait(ctx) }

func (e *Entry) wait(ctx context.Context) error {
	select {
	case <-e.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch e.state {
	case stateFailed:
		if e.terminalErr != nil {
			return e.terminalErr
		}
		return ErrClosed
	case stateClosed:
		return ErrClosed
	}
	return nil
}

// Init attaches an already-open upstream source. It is InitFactory with a
// provider that exists before admission, so the caller's source is closed on
// every failure path, including an admission refusal.
func (e *Entry) Init(source tiered.UpstreamProvider, metadata any, size int64) error {
	if source == nil {
		err := errors.New("activecache: nil upstream source")
		e.terminate(err)
		return err
	}
	built := false
	err := e.InitFactory(metadata, size, func() (tiered.UpstreamProvider, error) {
		built = true
		return source, nil
	})
	if err != nil && !built {
		if closer, ok := source.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	return err
}

// InitFactory admits a creator entry and starts its tiered cache before calling
// build, which is where the caller constructs the adapter that takes ownership
// of an already-fetched response body. Admission failures (capacity, the disk
// watermark, the scratch file) and a tiered.New failure therefore happen while
// the caller still owns its response: none of them consume or close the body,
// and the caller can deliver the fetched response through the ordinary path
// instead of asking upstream twice. build runs only after those steps succeed.
// An error from build itself is the caller's cleanup responsibility; build must
// not hand out a body it closed. On success the entry owns the source.
func (e *Entry) InitFactory(metadata any, size int64, build func() (tiered.UpstreamProvider, error)) error {
	fail := func(err error) error {
		e.terminate(err)
		return err
	}

	if build == nil {
		return fail(errors.New("activecache: nil upstream builder"))
	}
	if size < -1 {
		return fail(fmt.Errorf("%w: %d", ErrInvalidSize, size))
	}

	e.mu.Lock()
	if e.state != statePending {
		e.mu.Unlock()
		return fail(ErrAlreadyInitialized)
	}
	e.state = stateInitializing
	e.mu.Unlock()

	if err := e.m.admit(e, size); err != nil {
		return fail(err)
	}

	cache, err := e.m.newScratch(e)
	if err != nil {
		return fail(err)
	}
	e.mu.Lock()
	e.cache = cache
	e.mu.Unlock()

	// The source is filled in only after build runs, so tiered.New cannot pull a
	// body the caller has not handed over yet.
	lazy := &lazySource{}
	st := newEntryStore(e, e.algo, e.digest, e.m.adopt, e.m.handoff)
	pc, err := tiered.New(tiered.Config{Source: lazy, Cache: cache, Store: st})
	if err != nil {
		return fail(fmt.Errorf("activecache: start tiered cache: %w", err))
	}

	source, err := build()
	if err != nil {
		// The builder owns whatever it opened; stop the cache we just started and
		// leave the entry terminal. The scratch file is reclaimed at retirement.
		_ = pc.Close()
		return fail(err)
	}
	if source == nil {
		_ = pc.Close()
		return fail(errors.New("activecache: nil upstream source"))
	}
	lazy.set(source)

	e.mu.Lock()
	if e.state != stateInitializing {
		// Manager.Close won the race and already retired the entry without a
		// PassiveCache to close. Clean up the resources this call just created.
		e.mu.Unlock()
		_ = pc.Close()
		_ = cache.Reclaim()
		if closer, ok := source.(io.Closer); ok {
			_ = closer.Close()
		}
		return ErrClosed
	}
	e.pc = pc
	e.store = st
	e.metadata = metadata
	e.declaredSize = size
	// A provider that owns resources (a pending HTTP response body) is closed
	// with the entry. Init succeeds here, so the entry is now responsible for it.
	if closer, ok := source.(io.Closer); ok {
		e.source = closer
	}
	e.state = stateReady
	e.closeReadyLocked()
	e.mu.Unlock()
	return nil
}

// lazySource is the tiered.UpstreamProvider handed to tiered.New before the
// HTTP adapter exists. It forwards once InitFactory has attached the real
// source, and reports ErrNotReady if the core asks too early.
type lazySource struct {
	mu     sync.Mutex
	source tiered.UpstreamProvider
}

func (l *lazySource) set(source tiered.UpstreamProvider) {
	l.mu.Lock()
	l.source = source
	l.mu.Unlock()
}

func (l *lazySource) ReadSection(ctx context.Context, offset, size int64, extents chan<- tiered.Extent) (io.ReadCloser, error) {
	l.mu.Lock()
	source := l.source
	l.mu.Unlock()
	if source == nil {
		return nil, ErrNotReady
	}
	return source.ReadSection(ctx, offset, size, extents)
}

// Fail aborts a creator entry before Init. Waiters receive err and callers are
// expected to use the ordinary upstream path. A nil err becomes ErrAborted.
func (e *Entry) Fail(err error) {
	if err == nil {
		err = ErrAborted
	}
	e.terminate(err)
}

func (e *Entry) terminate(err error) {
	e.mu.Lock()
	if e.state == stateFailed || e.state == stateClosed || e.state == statePublished {
		e.mu.Unlock()
		return
	}
	reclaim := e.pc == nil
	e.state = stateFailed
	e.terminalErr = err
	e.closeReadyLocked()
	cache := e.cache
	e.mu.Unlock()

	e.markSettled()
	e.m.removeKey(e)
	if reclaim {
		if cache != nil {
			_ = cache.Reclaim()
		}
		// admit may have succeeded before the scratch file was created. The
		// reservation must be released whether or not there was a cache to
		// reclaim; an initialized entry keeps its accounting until retire so a
		// lingering reader's local bytes stay bounded.
		e.m.unaccount(e)
	}
	e.m.maybeRetire(e)
}

// markSettled closes the settlement channel exactly once. It must not be called
// while holding e.mu if a waiter could call back into the entry.
func (e *Entry) markSettled() {
	e.settleOnce.Do(func() { close(e.settled) })
}

func (e *Entry) closeReadyLocked() {
	if e.readyClosed {
		return
	}
	e.readyClosed = true
	close(e.ready)
}

// Release drops one reference. The last reference retires a published or failed
// entry; an unpublished one waits for the idle reaper.
func (e *Entry) Release() {
	e.mu.Lock()
	if e.refs > 0 {
		e.refs--
	}
	e.mu.Unlock()
	e.m.maybeRetire(e)
}

// Read returns a sequential reader for the complete object. It is only valid
// after the entry is ready.
func (e *Entry) Read(ctx context.Context) (io.ReadCloser, error) {
	e.mu.Lock()
	pc, state, err := e.pc, e.state, e.terminalErr
	e.mu.Unlock()
	if pc == nil {
		if state == stateFailed && err != nil {
			return nil, err
		}
		if state == stateClosed {
			return nil, ErrClosed
		}
		return nil, ErrNotReady
	}
	if state == stateClosed {
		return nil, ErrClosed
	}
	if state == stateFailed && err != nil {
		return nil, err
	}
	return pc.Read(ctx)
}

// ReadSection returns a reader for exactly size bytes at offset. It is only
// valid after the entry is ready.
func (e *Entry) ReadSection(ctx context.Context, offset, size int64) (io.ReadCloser, error) {
	e.mu.Lock()
	pc, state, err := e.pc, e.state, e.terminalErr
	e.mu.Unlock()
	if pc == nil {
		if state == stateFailed && err != nil {
			return nil, err
		}
		if state == stateClosed {
			return nil, ErrClosed
		}
		return nil, ErrNotReady
	}
	if state == stateClosed {
		return nil, ErrClosed
	}
	if state == stateFailed && err != nil {
		return nil, err
	}
	return pc.ReadSection(ctx, offset, size)
}

// StalledTime reports how long the entry has had no reader, upstream or
// handoff activity. An entry without a tiered cache falls back to its age.
func (e *Entry) StalledTime() time.Duration { return e.idleFor() }

func (e *Entry) idleFor() time.Duration {
	e.mu.Lock()
	pc := e.pc
	created := e.created
	e.mu.Unlock()
	if pc != nil {
		return pc.StalledTime()
	}
	return max(time.Since(created), 0)
}

// Metadata returns the value the creator passed to Init. It is nil until the
// entry becomes ready.
func (e *Entry) Metadata() any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.metadata
}

// DeclaredSize returns the size the creator passed to Init, or -1 when unknown.
func (e *Entry) DeclaredSize() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.declaredSize
}

// Published reports whether hashstore.Adopt accepted the complete body.
func (e *Entry) Published() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state == statePublished || e.state == stateClosed && e.content.SHA256 != ""
}

// Content returns the published digests. The boolean is false until the body is
// published.
func (e *Entry) Content() (hashstore.Content, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.content.SHA256 == "" {
		return hashstore.Content{}, false
	}
	return e.content, true
}

// WaitPublished blocks until the entry can no longer become published and
// reports the outcome: the published content, the failure that prevented
// publication, or ErrClosed when the manager shut the entry down first. A
// caller that delivered a complete body uses it to decide whether a URL variant
// may name the result. ctx bounds the wait; the entry keeps publishing.
func (e *Entry) WaitPublished(ctx context.Context) (hashstore.Content, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-e.settled:
	case <-ctx.Done():
		return hashstore.Content{}, ctx.Err()
	}
	if content, ok := e.Content(); ok {
		return content, nil
	}
	if err := e.Err(); err != nil {
		return hashstore.Content{}, err
	}
	return hashstore.Content{}, ErrClosed
}

// Err returns the terminal error, if the entry failed.
func (e *Entry) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != stateFailed && e.state != stateClosed {
		return nil
	}
	// A closed entry that published is a success: readers already got valid
	// bytes. An unpublished one is always a failure, even if no explicit error
	// was recorded when it was retired.
	if e.content.SHA256 != "" {
		return nil
	}
	if e.terminalErr != nil {
		return e.terminalErr
	}
	return ErrClosed
}
