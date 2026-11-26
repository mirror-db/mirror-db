package activecache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/diskspace"
	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

var (
	// ErrClosed means the manager is shutting down.
	ErrClosed = errors.New("activecache: manager closed")
	// ErrCapacity means MaxEntries, the byte capacity, or an already active
	// unknown-size entry refuses a new entry. Callers should use the ordinary
	// upstream path; the manager does not queue or block.
	ErrCapacity = errors.New("activecache: capacity limited")
	// ErrLowDiskSpace means the free-space watermark refuses a new entry. It is
	// the diskspace error, so errors.Is checks work for both.
	ErrLowDiskSpace = diskspace.ErrLowDiskSpace
	// ErrKeyEmpty means Acquire was called without a key.
	ErrKeyEmpty = errors.New("activecache: empty key")
	// ErrNotReady means the entry has no tiered cache yet. A creator must call
	// Init first.
	ErrNotReady = errors.New("activecache: entry is not ready")
	// ErrAlreadyInitialized means Init was called twice on one entry.
	ErrAlreadyInitialized = errors.New("activecache: entry already initialized")
	// ErrInvalidSize means Init was given a size below -1.
	ErrInvalidSize = errors.New("activecache: invalid declared size")
	// ErrAborted marks an entry the creator abandoned before Init.
	ErrAborted = errors.New("activecache: entry aborted")
)

const scratchPrefix = "active-"

// Config describes a Manager. Dir must be the same cache directory the hash
// store was opened with: Adopt only accepts files under <Dir>/.tmp.
type Config struct {
	// Dir is the local cache directory. Scratch files live in <Dir>/.tmp, which
	// is also where hashstore.Adopt expects them.
	Dir string
	// Capacity bounds the bytes held by active entries together. Known sizes are
	// reserved at Init; an entry of unknown size is admitted alone and may grow
	// to completion while admitting nothing else.
	Capacity int64
	// MaxEntries bounds the number of active keys.
	MaxEntries int
	// IdleTimeout is how long an entry with no readers and no activity is kept
	// before the reaper drops it. Published entries leave the active map at once
	// and are only closed when their readers release.
	IdleTimeout time.Duration
	// DiskWaterMark is the minimum free space that must remain on Dir before a
	// new entry is admitted. Zero disables the guard.
	DiskWaterMark int64
	// Store is the destination for published bodies. Required unless an
	// internal test injects an adopter.
	Store *hashstore.Store
	// MaxBodySize bounds one entry's body. Zero derives it from the adopter
	// (hashstore.Store reports its MaxSize through Stats.Capacity) and falls
	// back to Capacity. An entry larger than this can never be adopted, so
	// admitting it would only waste an upstream fetch.
	MaxBodySize int64

	// diskOptions and adopt are test seams. Production callers cannot set them,
	// so the real filesystem is always probed and bodies always go to Store.
	diskOptions []diskspace.Option
	adopt       adopter
	// disableHandoff forces the copy-based tiered.Store path in tests.
	disableHandoff bool
}

type adopter interface {
	Adopt(ctx context.Context, path string, size int64, expectAlgorithm hashstore.Algorithm, expectDigest string) (hashstore.Content, error)
}

// Manager owns the active entries of one process. It is safe for concurrent use.
type Manager struct {
	tmpDir      string
	capacity    int64
	maxBody     int64
	maxEntries  int
	idleTimeout time.Duration
	reaperEvery time.Duration
	space       *diskspace.Checker
	adopt       adopter
	handoff     bool

	ctx    context.Context
	cancel context.CancelFunc

	mu            sync.Mutex
	byKey         map[string]*Entry
	live          map[*Entry]struct{}
	used          int64
	unknownActive int
	closed        bool

	reapDone   chan struct{}
	reapExited chan struct{}

	closeOnce sync.Once
	retireWG  sync.WaitGroup
}

// New validates cfg, creates the scratch directory and starts the reaper.
func New(cfg Config) (*Manager, error) {
	cfg.Dir = strings.TrimSpace(cfg.Dir)
	if cfg.Dir == "" {
		return nil, errors.New("activecache: cache dir is empty")
	}
	if cfg.Capacity <= 0 {
		return nil, fmt.Errorf("activecache: capacity must be positive, got %d", cfg.Capacity)
	}
	if cfg.MaxEntries <= 0 {
		return nil, fmt.Errorf("activecache: max entries must be positive, got %d", cfg.MaxEntries)
	}
	if cfg.IdleTimeout <= 0 {
		return nil, fmt.Errorf("activecache: idle timeout must be positive, got %s", cfg.IdleTimeout)
	}
	if cfg.DiskWaterMark < 0 {
		return nil, fmt.Errorf("activecache: disk water mark must not be negative, got %d", cfg.DiskWaterMark)
	}
	if cfg.adopt == nil {
		if cfg.Store == nil {
			return nil, errors.New("activecache: nil hash store")
		}
		cfg.adopt = cfg.Store
	}

	maxBody := cfg.Capacity
	if cfg.MaxBodySize > 0 && cfg.MaxBodySize < maxBody {
		maxBody = cfg.MaxBodySize
	}
	if stats, ok := cfg.adopt.(interface{ Stats() hashstore.Stats }); ok {
		if limit := stats.Stats().Capacity; limit > 0 && limit < maxBody {
			maxBody = limit
		}
	}

	tmpDir := filepath.Join(cfg.Dir, ".tmp")
	for _, dir := range []string{cfg.Dir, tmpDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("activecache: create %s: %w", dir, err)
		}
	}

	every := cfg.IdleTimeout / 4
	if every < 10*time.Millisecond {
		every = 10 * time.Millisecond
	}
	if every > time.Second {
		every = time.Second
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		tmpDir:      tmpDir,
		capacity:    cfg.Capacity,
		maxBody:     maxBody,
		maxEntries:  cfg.MaxEntries,
		idleTimeout: cfg.IdleTimeout,
		reaperEvery: every,
		space:       diskspace.New(cfg.Dir, cfg.DiskWaterMark, cfg.diskOptions...),
		adopt:       cfg.adopt,
		handoff:     !cfg.disableHandoff,
		ctx:         ctx,
		cancel:      cancel,
		byKey:       make(map[string]*Entry),
		live:        make(map[*Entry]struct{}),
		reapDone:    make(chan struct{}),
		reapExited:  make(chan struct{}),
	}
	go m.reapLoop()
	return m, nil
}

// Acquire takes a reference to the entry for key. When created is true the
// caller is the creator and must eventually call Init or Fail and Release.
// Otherwise Acquire waits until that creator finishes, and returns its error
// when initialization failed so the caller can fall back to the upstream.
func (m *Manager) Acquire(ctx context.Context, key string) (entry *Entry, created bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, false, ErrKeyEmpty
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, false, ErrClosed
	}
	if e := m.byKey[key]; e != nil {
		e.mu.Lock()
		e.refs++
		e.mu.Unlock()
		m.mu.Unlock()
		if err := e.wait(ctx); err != nil {
			e.Release()
			return nil, false, err
		}
		return e, false, nil
	}
	m.mu.Unlock()

	// Probe the filesystem without holding the manager lock. The check is
	// repeated here, after the existing-entry fast path, so a reader reusing a
	// live key is never refused because the disk is currently low.
	if err := m.checkWater(); err != nil {
		return nil, false, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, false, ErrClosed
	}
	if e := m.byKey[key]; e != nil {
		e.mu.Lock()
		e.refs++
		e.mu.Unlock()
		m.mu.Unlock()
		if err := e.wait(ctx); err != nil {
			e.Release()
			return nil, false, err
		}
		return e, false, nil
	}
	if entries := len(m.byKey); entries >= m.maxEntries {
		m.mu.Unlock()
		return nil, false, fmt.Errorf("%w: %d active entries", ErrCapacity, entries)
	}
	if used := m.used; used >= m.capacity {
		m.mu.Unlock()
		return nil, false, fmt.Errorf("%w: %d of %d bytes active", ErrCapacity, used, m.capacity)
	}
	if m.unknownActive > 0 {
		m.mu.Unlock()
		return nil, false, fmt.Errorf("%w: an unknown-size entry is active", ErrCapacity)
	}
	e := newEntry(m, key)
	e.refs = 1
	m.byKey[key] = e
	m.live[e] = struct{}{}
	m.mu.Unlock()
	return e, true, nil
}

// Close stops the reaper and closes every live entry. It must be called before
// the hash store is closed. It is idempotent.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		close(m.reapDone)
		entries := make([]*Entry, 0, len(m.live))
		for e := range m.live {
			entries = append(entries, e)
		}
		m.mu.Unlock()

		m.cancel()
		<-m.reapExited
		m.retireWG.Wait()
		for _, e := range entries {
			m.retire(e)
		}
	})
	return nil
}

// checkWater probes the disk watermark. A low verdict is not cached: every new
// admission probes again, and an entry already accepted keeps writing.
func (m *Manager) checkWater() error {
	if !m.space.Enabled() {
		return nil
	}
	return m.space.Check()
}

// admit reserves capacity for a known size or marks the single unknown-size
// slot. It performs no I/O while holding the lock.
func (m *Manager) admit(e *Entry, size int64) error {
	if err := m.checkWater(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if size >= 0 {
		if m.unknownActive > 0 {
			return fmt.Errorf("%w: an unknown-size entry is active", ErrCapacity)
		}
		if size > m.maxBody {
			return fmt.Errorf("%w: body of %d bytes exceeds the %d-byte adopt limit", ErrCapacity, size, m.maxBody)
		}
		if size > m.capacity || m.used > m.capacity-size {
			return fmt.Errorf("%w: need %d bytes, %d of %d active", ErrCapacity, size, m.used, m.capacity)
		}
		e.accounted = size
		m.used += size
		return nil
	}
	if m.unknownActive > 0 {
		return fmt.Errorf("%w: an unknown-size entry is active", ErrCapacity)
	}
	if m.used >= m.capacity {
		return fmt.Errorf("%w: %d of %d bytes active", ErrCapacity, m.used, m.capacity)
	}
	e.unknown = true
	m.unknownActive++
	return nil
}

// reserveWrite admits bytes about to be written to the scratch file. A known
// size was fully reserved at Init. An unknown one grows here under a hard cap:
// without a declared size the manager cannot reserve up front, but it must never
// grow past what Adopt could accept or the segment's byte budget. The caller
// releases the unwritten remainder after the write.
func (m *Manager) reserveWrite(e *Entry, n int64) error {
	if n <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if !e.unknown {
		return nil
	}
	if e.accounted+n > m.maxBody {
		return fmt.Errorf("%w: unknown-size body exceeds %d bytes", ErrCapacity, m.maxBody)
	}
	// The segment budget is shared with every other admitted entry. An unknown
	// body has no reservation, so its writes may only consume what is left.
	if m.used > m.capacity-n {
		return fmt.Errorf("%w: unknown-size body exceeds the %d-byte segment budget", ErrCapacity, m.capacity)
	}
	e.accounted += n
	m.used += n
	return nil
}

// releaseWrite returns bytes reserved by reserveWrite that were not written.
func (m *Manager) releaseWrite(e *Entry, n int64) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	if e.unknown {
		if n > e.accounted {
			n = e.accounted
		}
		e.accounted -= n
		m.used -= n
		if m.used < 0 {
			m.used = 0
		}
	}
	m.mu.Unlock()
}

// accountWritten records space actually written. A low watermark must not abort
// an admitted entry: dropping bytes would leave a hole the core believes is
// cached. The probe only gates new admissions.
func (m *Manager) accountWritten(n int64) {
	if n <= 0 {
		return
	}
	_ = m.space.Account(n)
}

// unaccountLocked releases an entry's reservation. The caller holds m.mu.
func (m *Manager) unaccountLocked(e *Entry) {
	if e.accounted > 0 {
		m.used -= e.accounted
		e.accounted = 0
	}
	if e.unknown {
		if m.unknownActive > 0 {
			m.unknownActive--
		}
		e.unknown = false
	}
}

func (m *Manager) unaccount(e *Entry) {
	m.mu.Lock()
	m.unaccountLocked(e)
	m.mu.Unlock()
}

func (m *Manager) removeKey(e *Entry) {
	m.mu.Lock()
	if m.byKey[e.key] == e {
		delete(m.byKey, e.key)
	}
	m.mu.Unlock()
}

// published runs when hashstore.Adopt accepted the body. It removes the key
// immediately so new requests find the hash store, and lets existing readers
// finish on the entry's local file descriptor.
func (m *Manager) published(e *Entry, c hashstore.Content) {
	m.removeKey(e)

	e.mu.Lock()
	if e.state != stateFailed && e.state != stateClosed {
		e.state = statePublished
		e.content = c
	}
	e.mu.Unlock()
	e.markSettled()

	// A local backend renamed the scratch file into the content store, but a
	// reader may still hold the file descriptor. A shared backend leaves the
	// local file readable until the same readers finish. Either way the bytes
	// stay accounted here until retirement; releasing the reservation at publish
	// would let the segment admit new work against disk it still occupies.

	m.maybeRetire(e)
}

// storeFailed records a publication failure. Readers already streaming from the
// scratch cache may continue; the key is removed so a later request rebuilds a
// fresh entry instead of inheriting a broken one.
func (m *Manager) storeFailed(e *Entry, err error) {
	e.mu.Lock()
	if e.state == stateReady || e.state == stateInitializing {
		e.state = stateFailed
		e.terminalErr = err
		e.closeReadyLocked()
	}
	e.mu.Unlock()
	e.markSettled()
	m.removeKey(e)
	m.maybeRetire(e)
}

func (m *Manager) maybeRetire(e *Entry) {
	e.mu.Lock()
	schedule := e.refs == 0 && (e.state == stateFailed || e.state == statePublished) && !e.retireScheduled
	if schedule {
		e.retireScheduled = true
	}
	e.mu.Unlock()
	if schedule {
		// Never call PassiveCache.Close from a tiered callback: the callback runs
		// on a goroutine that Close waits for. A tracked goroutine breaks that
		// cycle and lets Manager.Close wait for it.
		m.scheduleRetire(e)
	}
}

// scheduleRetire runs retirement on its own goroutine so a tiered callback never
// closes the cache that is waiting for it. Once the manager is closing, the
// scheduled work is dropped: Close has already snapshotted every live entry and
// retires it itself, outside the tiered callback's goroutine.
func (m *Manager) scheduleRetire(e *Entry) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.retireWG.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.retireWG.Done()
		m.retire(e)
	}()
}

// retire closes the entry's adapters exactly once, releases its accounting and
// removes it from the live set. It is safe to call concurrently; closeOnce makes
// second callers wait for the first.
func (m *Manager) retire(e *Entry) {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		if !e.readyClosed {
			if e.state != stateFailed {
				e.state = stateFailed
				e.terminalErr = ErrClosed
			}
			e.closeReadyLocked()
		}
		pc := e.pc
		st := e.store
		cache := e.cache
		source := e.source
		e.mu.Unlock()

		if pc != nil {
			_ = pc.Close()
		}
		if source != nil {
			_ = source.Close()
		}
		if st != nil {
			_ = st.close()
		}
		if cache != nil {
			_ = cache.Reclaim()
		}

		m.mu.Lock()
		if m.byKey[e.key] == e {
			delete(m.byKey, e.key)
		}
		delete(m.live, e)
		m.unaccountLocked(e)
		m.mu.Unlock()

		e.mu.Lock()
		e.state = stateClosed
		e.mu.Unlock()
		e.markSettled()
	})
}

func (m *Manager) reapLoop() {
	defer close(m.reapExited)
	ticker := time.NewTicker(m.reaperEvery)
	defer ticker.Stop()
	for {
		select {
		case <-m.reapDone:
			return
		case <-ticker.C:
			m.reap()
		}
	}
}

// reap drops idle entries with no references. Published entries are already out
// of the active map; failed ones are removed when the last reference leaves.
func (m *Manager) reap() {
	m.mu.Lock()
	candidates := make([]*Entry, 0)
	for e := range m.live {
		e.mu.Lock()
		idle := e.refs == 0 && e.state != statePublished && e.state != stateFailed && e.state != stateClosed
		e.mu.Unlock()
		if idle {
			candidates = append(candidates, e)
		}
	}
	m.mu.Unlock()

	for _, e := range candidates {
		if e.idleFor() < m.idleTimeout {
			continue
		}
		m.mu.Lock()
		e.mu.Lock()
		due := e.refs == 0 && e.state != statePublished && e.state != stateFailed && e.state != stateClosed
		e.mu.Unlock()
		if due && m.byKey[e.key] == e {
			delete(m.byKey, e.key)
		} else if !due {
			m.mu.Unlock()
			continue
		}
		m.mu.Unlock()
		m.scheduleRetire(e)
	}
}

func (m *Manager) newScratch(e *Entry) (*scratchCache, error) {
	file, err := os.CreateTemp(m.tmpDir, scratchPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("activecache: create scratch: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("activecache: close scratch: %w", err)
	}
	cache, err := newScratchCache(path, e)
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return cache, nil
}
