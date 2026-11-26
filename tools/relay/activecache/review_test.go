package activecache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	tiered "github.com/geektr-cloud/go-tiered-cache"
	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// trackedSource records whether the entry closed it. Close is idempotent, like
// every provider the manager accepts.
type trackedSource struct {
	testSource
	closed atomic.Bool
	once   sync.Once
}

func (s *trackedSource) Close() error {
	s.once.Do(func() { s.closed.Store(true) })
	return nil
}

// An unknown-size body has no reservation, so the only hard boundary is the
// write path: it must refuse bytes beyond the admit limit instead of filling
// the disk forever.
func TestUnknownSizeWriteIsBounded(t *testing.T) {
	store, dir := openTestStore(t, 16<<20)
	m := newTestManager(t, store, dir, func(cfg *Config) { cfg.Capacity = 4096 })

	data := bytes.Repeat([]byte("x"), 64<<10)
	entry, created, err := m.Acquire(context.Background(), "unknown-bounded")
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if err := entry.Init(&testSource{data: data}, nil, -1); err != nil {
		t.Fatalf("init: %v", err)
	}
	reader, err := entry.Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, readErr := io.Copy(io.Discard, reader); readErr == nil {
		_ = reader.Close()
		t.Fatal("oversized unknown body was accepted")
	}
	_ = reader.Close()
	entry.Release()

	// The failed entry must release its accounting once it retires.
	waitFor(t, "bounded entry retire", func() bool {
		next, created, err := m.Acquire(context.Background(), "after-bounded")
		if err != nil || !created {
			return false
		}
		if err := next.Init(&testSource{data: make([]byte, 8)}, nil, 8); err != nil {
			next.Release()
			return false
		}
		next.Fail(nil)
		next.Release()
		return true
	})
}

// An unknown-size body shares the segment budget with admitted entries; it may
// not grow into space another entry already reserved.
func TestUnknownSizeSharesSegmentBudget(t *testing.T) {
	store, dir := openTestStore(t, 16<<20)
	m := newTestManager(t, store, dir, func(cfg *Config) { cfg.Capacity = 4096 })

	known, created, err := m.Acquire(context.Background(), "known")
	if err != nil || !created {
		t.Fatalf("acquire known: created=%v err=%v", created, err)
	}
	if err := known.Init(&testSource{data: make([]byte, 3072)}, nil, 3072); err != nil {
		t.Fatalf("init known: %v", err)
	}

	unknown, created, err := m.Acquire(context.Background(), "unknown-shared")
	if err != nil || !created {
		t.Fatalf("acquire unknown: created=%v err=%v", created, err)
	}
	if err := unknown.Init(&testSource{data: bytes.Repeat([]byte("u"), 2048)}, nil, -1); err != nil {
		t.Fatalf("init unknown: %v", err)
	}
	reader, err := unknown.Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, readErr := io.Copy(io.Discard, reader); readErr == nil {
		_ = reader.Close()
		t.Fatal("unknown body grew past the segment budget")
	}
	_ = reader.Close()
	unknown.Release()
	known.Fail(nil)
	known.Release()
}

// admit reserves capacity before the scratch file exists. If creating that file
// fails, the reservation must not leak.
func TestInitScratchFailureReleasesReservation(t *testing.T) {
	store, dir := openTestStore(t, 16<<20)
	m := newTestManager(t, store, dir, func(cfg *Config) { cfg.Capacity = 8 })

	tmp := filepath.Join(dir, ".tmp")
	backup := filepath.Join(dir, ".tmp.bak")
	if err := os.Rename(tmp, backup); err != nil {
		t.Fatalf("move scratch dir: %v", err)
	}
	if err := os.WriteFile(tmp, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("block scratch dir: %v", err)
	}

	entry, created, err := m.Acquire(context.Background(), "scratch-fails")
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if err := entry.Init(&testSource{data: make([]byte, 8)}, nil, 8); err == nil {
		t.Fatalf("Init succeeded without a scratch directory")
	}
	entry.Release()

	if err := os.Remove(tmp); err != nil {
		t.Fatalf("remove blocker: %v", err)
	}
	if err := os.Rename(backup, tmp); err != nil {
		t.Fatalf("restore scratch dir: %v", err)
	}

	next, created, err := m.Acquire(context.Background(), "scratch-works")
	if err != nil || !created {
		t.Fatalf("acquire after failure: created=%v err=%v", created, err)
	}
	if err := next.Init(&testSource{data: make([]byte, 8)}, nil, 8); err != nil {
		t.Fatalf("reservation was not released: %v", err)
	}
	next.Fail(nil)
	next.Release()
}

func TestInitAndCloseReleaseSource(t *testing.T) {
	t.Run("init failure", func(t *testing.T) {
		store, dir := openTestStore(t, 16<<20)
		m := newTestManager(t, store, dir, nil)

		tmp := filepath.Join(dir, ".tmp")
		backup := filepath.Join(dir, ".tmp.bak")
		if err := os.Rename(tmp, backup); err != nil {
			t.Fatalf("move scratch dir: %v", err)
		}
		if err := os.WriteFile(tmp, []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("block scratch dir: %v", err)
		}
		defer func() {
			_ = os.Remove(tmp)
			_ = os.Rename(backup, tmp)
		}()

		source := &trackedSource{testSource: testSource{data: make([]byte, 8)}}
		entry, created, err := m.Acquire(context.Background(), "init-source")
		if err != nil || !created {
			t.Fatalf("acquire: created=%v err=%v", created, err)
		}
		if err := entry.Init(source, nil, 8); err == nil {
			t.Fatalf("Init succeeded without a scratch directory")
		}
		if !source.closed.Load() {
			t.Fatal("Init failure left the source open")
		}
		entry.Release()
	})

	t.Run("manager close", func(t *testing.T) {
		store, dir := openTestStore(t, 16<<20)
		m := newTestManager(t, store, dir, nil)
		source := &trackedSource{testSource: testSource{data: bytes.Repeat([]byte("a"), 1024)}}
		entry, created, err := m.Acquire(context.Background(), digestKey(source.data))
		if err != nil || !created {
			t.Fatalf("acquire: created=%v err=%v", created, err)
		}
		if err := entry.Init(source, nil, int64(len(source.data))); err != nil {
			t.Fatalf("init: %v", err)
		}
		if err := m.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if !source.closed.Load() {
			t.Fatal("Manager.Close left the source open")
		}
		// A second close is harmless, matching the adapter contract.
		_ = source.Close()
		entry.Release()
	})
}

func TestEntryErrAfterClose(t *testing.T) {
	t.Run("unpublished is a failure", func(t *testing.T) {
		store, dir := openTestStore(t, 16<<20)
		m := newTestManager(t, store, dir, nil)
		entry, created, err := m.Acquire(context.Background(), "unpublished")
		if err != nil || !created {
			t.Fatalf("acquire: created=%v err=%v", created, err)
		}
		if err := entry.Init(&testSource{data: make([]byte, 4096)}, nil, 4096); err != nil {
			t.Fatalf("init: %v", err)
		}
		if err := m.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if !errors.Is(entry.Err(), ErrClosed) {
			t.Fatalf("Err after Close = %v, want ErrClosed", entry.Err())
		}
		entry.Release()
	})

	t.Run("published is a success", func(t *testing.T) {
		store, dir := openTestStore(t, 16<<20)
		m := newTestManager(t, store, dir, nil)
		data := bytes.Repeat([]byte("published"), 256)
		entry, created, err := m.Acquire(context.Background(), digestKey(data))
		if err != nil || !created {
			t.Fatalf("acquire: created=%v err=%v", created, err)
		}
		if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
			t.Fatalf("init: %v", err)
		}
		readAll(t, mustRead(t, entry))
		waitPublished(t, entry)
		if err := m.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := entry.Err(); err != nil {
			t.Fatalf("Err after publish+Close = %v, want nil", err)
		}
		entry.Release()
	})
}

// The manager must not admit a body that the adopter can only reject.
func TestManagerMaxBodyFromStore(t *testing.T) {
	dir := t.TempDir()
	store, err := hashstore.Open(hashstore.Config{Dir: dir, Capacity: 1 << 20, MaxSize: 8})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m := newTestManager(t, store, dir, func(cfg *Config) { cfg.Capacity = 1 << 20 })

	big, created, err := m.Acquire(context.Background(), "big")
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if err := big.Init(&testSource{data: make([]byte, 16)}, nil, 16); !errors.Is(err, ErrCapacity) {
		t.Fatalf("Init oversized body = %v, want ErrCapacity", err)
	}
	big.Release()

	small, created, err := m.Acquire(context.Background(), "small")
	if err != nil || !created {
		t.Fatalf("acquire small: created=%v err=%v", created, err)
	}
	if err := small.Init(&testSource{data: make([]byte, 8)}, nil, 8); err != nil {
		t.Fatalf("Init at the limit: %v", err)
	}
	small.Fail(nil)
	small.Release()
}

// Accounting survives publication: a reader still holds the local file, so the
// space is not free until retirement.
func TestPublishedBodyKeepsAccountingUntilRetire(t *testing.T) {
	store, dir := openTestStore(t, 16<<20)
	m := newTestManager(t, store, dir, func(cfg *Config) { cfg.Capacity = 4096 })
	data := bytes.Repeat([]byte("r"), 4096)

	entry, created, err := m.Acquire(context.Background(), digestKey(data))
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	readAll(t, mustRead(t, entry))
	waitPublished(t, entry)

	if _, _, err := m.Acquire(context.Background(), "while-held"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("Acquire while a published reader holds space = %v, want ErrCapacity", err)
	}

	entry.Release()
	waitFor(t, "published entry retire", func() bool {
		next, created, err := m.Acquire(context.Background(), "after-retire")
		if err != nil || !created {
			return false
		}
		next.Fail(nil)
		next.Release()
		return true
	})
}

var _ tiered.UpstreamProvider = (*trackedSource)(nil)
