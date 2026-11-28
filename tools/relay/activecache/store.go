package activecache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	tiered "github.com/geektr-cloud/go-tiered-cache"
	fsadapter "github.com/geektr-cloud/go-tiered-cache/adapters/fs"
	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// scratchCache is the random-access scratch file of one entry. It accounts every
// write with the manager and treats a missing file during Reclaim as success:
// Adopt renames the file into the content store before the tiered core reclaims
// it.
type scratchCache struct {
	*fsadapter.Cache
	entry *Entry
}

var _ tiered.Cache = (*scratchCache)(nil)

func newScratchCache(path string, e *Entry) (*scratchCache, error) {
	cache, err := fsadapter.NewCache(path)
	if err != nil {
		return nil, fmt.Errorf("activecache: open scratch: %w", err)
	}
	return &scratchCache{Cache: cache, entry: e}, nil
}

func (c *scratchCache) WriteAt(p []byte, offset int64) (int, error) {
	want := int64(len(p))
	if err := c.entry.m.reserveWrite(c.entry, want); err != nil {
		// Exceeding the hard boundary is terminal for this object, not a
		// transient upstream failure. Marking the entry failed wakes waiters and
		// lets the last Release retire it; the tiered core terminates too.
		c.entry.m.storeFailed(c.entry, err)
		return 0, err
	}
	n, err := c.Cache.WriteAt(p, offset)
	if n > 0 {
		c.entry.m.accountWritten(int64(n))
	}
	if int64(n) < want {
		c.entry.m.releaseWrite(c.entry, want-int64(n))
	}
	if err != nil {
		// A scratch write that cannot complete leaves a hole the core believes
		// is cached. Fail the entry now so a later request rebuilds instead of
		// reading a partial object.
		c.entry.m.storeFailed(c.entry, err)
	}
	return n, err
}

func (c *scratchCache) Reclaim() error {
	err := c.Cache.Reclaim()
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// entryStore publishes a completed scratch file through hashstore.Adopt. It
// implements tiered.CacheHandoff so the normal path adopts the scratch file
// directly instead of copying it; the copy-based Write/Finalize path remains for
// tests and for a Store that cannot hand off.
//
// When the adopter also accepts precomputed digests and both PrefixSinks saw the
// whole object, publication uses AdoptTrusted and skips the store's verification
// read. Every other case keeps Adopt, which recomputes both digests.
//
// The tiered core never closes adapters. close is called by Manager.retire after
// the PassiveCache has stopped.
type entryStore struct {
	entry      *Entry
	adopt      adopter
	algo       hashstore.Algorithm
	digest     string
	handoff    bool
	sha1Sink   *digestSink
	sha256Sink *digestSink

	mu        sync.RWMutex
	temp      *os.File
	tempPath  string
	written   int64
	readFile  *os.File
	published bool
	adopting  bool
	closed    bool
}

var (
	_ tiered.Store        = (*entryStore)(nil)
	_ tiered.CacheHandoff = (*entryStore)(nil)
)

func newEntryStore(e *Entry, algo hashstore.Algorithm, digest string, adopt adopter, handoff bool, sha1Sink, sha256Sink *digestSink) *entryStore {
	return &entryStore{
		entry:      e,
		adopt:      adopt,
		algo:       algo,
		digest:     digest,
		handoff:    handoff,
		sha1Sink:   sha1Sink,
		sha256Sink: sha256Sink,
	}
}

// Write appends bytes on the copy-based handoff path.
func (s *entryStore) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, fmt.Errorf("activecache: store write: %w", ErrClosed)
	}
	if s.published || s.adopting {
		return 0, fmt.Errorf("activecache: store write after finalize: %w", tiered.ErrClosed)
	}
	if s.temp == nil {
		// The copy fallback is another local temp file. It creates no scratch
		// cache of its own, so it must consult the same watermark before the file
		// exists; otherwise the one path that exists for a broken handoff could
		// fill a disk the guard is meant to protect.
		if err := s.entry.m.checkWater(); err != nil {
			s.entry.m.storeFailed(s.entry, err)
			return 0, err
		}
		file, err := os.CreateTemp(s.entry.m.tmpDir, scratchPrefix+"store-*")
		if err != nil {
			err = fmt.Errorf("activecache: create store temp: %w", err)
			s.entry.m.storeFailed(s.entry, err)
			return 0, err
		}
		s.temp = file
		s.tempPath = file.Name()
	}
	n, err := s.temp.Write(p)
	s.written += int64(n)
	if err != nil {
		err = fmt.Errorf("activecache: write store temp: %w", err)
		s.entry.m.storeFailed(s.entry, err)
		return n, err
	}
	if err := s.entry.m.space.Account(int64(n)); err != nil {
		s.entry.m.storeFailed(s.entry, err)
		return n, err
	}
	return n, nil
}

// ReadAt serves published bytes from the local descriptor opened before Adopt.
// The descriptor stays valid after Adopt renames or Reclaim deletes the scratch
// name, and no read ever touches a shared object store.
func (s *entryStore) ReadAt(p []byte, offset int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.readFile == nil {
		return 0, fmt.Errorf("activecache: store read: %w", ErrClosed)
	}
	if offset < 0 {
		return 0, fmt.Errorf("activecache: store read at %d: %w", offset, tiered.ErrInvalidRange)
	}
	return s.readFile.ReadAt(p, offset)
}

// Finalize publishes the store's own temporary copy. This is the fallback the
// tiered core uses when FinalizeFromCache reports ErrHandoffUnsupported.
func (s *entryStore) Finalize() error {
	s.mu.Lock()
	if s.published {
		s.mu.Unlock()
		return nil
	}
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("activecache: finalize store: %w", ErrClosed)
	}
	if s.adopting {
		s.mu.Unlock()
		return fmt.Errorf("activecache: finalize store: already publishing")
	}
	if s.temp == nil {
		s.mu.Unlock()
		return fmt.Errorf("activecache: finalize store: no temp file")
	}
	s.adopting = true
	path := s.tempPath
	size := s.written
	s.mu.Unlock()

	err := s.publish(s.entry.m.ctx, path, size)

	s.mu.Lock()
	s.adopting = false
	s.mu.Unlock()
	return err
}

// FinalizeFromCache adopts the complete scratch file in place. It opens the
// local descriptor first so readers keep working after Adopt renames the file
// (local backend) or after the core reclaims the scratch name (shared backend).
func (s *entryStore) FinalizeFromCache(ctx context.Context, cache tiered.Cache, end int64) error {
	if !s.handoff {
		return fmt.Errorf("activecache: direct handoff disabled: %w", tiered.ErrHandoffUnsupported)
	}
	scratch, ok := cache.(*scratchCache)
	if !ok || scratch == nil {
		return fmt.Errorf("activecache: handoff from %T: %w", cache, tiered.ErrHandoffUnsupported)
	}
	if end < 0 {
		return fmt.Errorf("activecache: handoff end %d: %w", end, tiered.ErrInvalidRange)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("activecache: handoff store: %w", ErrClosed)
	}
	if s.published {
		s.mu.Unlock()
		return nil
	}
	if s.adopting {
		s.mu.Unlock()
		return fmt.Errorf("activecache: handoff store: already publishing")
	}
	s.adopting = true
	s.mu.Unlock()

	err := s.publish(ctx, scratch.Path(), end)

	s.mu.Lock()
	s.adopting = false
	s.mu.Unlock()
	return err
}

func (s *entryStore) publish(ctx context.Context, path string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		err = fmt.Errorf("activecache: open published source: %w", err)
		s.entry.m.storeFailed(s.entry, err)
		return err
	}

	content, err := s.adoptContent(ctx, path, size)
	if err != nil {
		_ = file.Close()
		err = fmt.Errorf("activecache: publish content: %w", err)
		s.entry.m.storeFailed(s.entry, err)
		return err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("activecache: publish after close: %w", ErrClosed)
	}
	s.readFile = file
	s.published = true
	s.mu.Unlock()

	// Adopt consumes the source with the local backend (rename); a shared
	// backend leaves it for the caller. Both are safe to remove: the read
	// descriptor is already open.
	_ = os.Remove(path)

	s.entry.m.published(s.entry, content)
	return nil
}

// adoptContent publishes the completed body. A store that accepts precomputed
// digests is used only when both sinks saw exactly the published extent, so a
// partial prefix can never be committed as the object's digest. Otherwise the
// file goes through Adopt, which recomputes both digests itself.
func (s *entryStore) adoptContent(ctx context.Context, path string, size int64) (hashstore.Content, error) {
	if trusted, ok := s.adopt.(trustedAdopter); ok {
		if content, ok := assembledContent(s.sha1Sink, s.sha256Sink, size); ok {
			return trusted.AdoptTrusted(ctx, path, content, s.algo, s.digest)
		}
	}
	return s.adopt.Adopt(ctx, path, size, s.algo, s.digest)
}

func (s *entryStore) close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	temp := s.temp
	tempPath := s.tempPath
	read := s.readFile
	s.temp = nil
	s.readFile = nil
	s.mu.Unlock()

	var errs []error
	if read != nil {
		if err := read.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if temp != nil {
		if err := temp.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if tempPath != "" {
		if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var _ io.ReaderAt = (*entryStore)(nil)
