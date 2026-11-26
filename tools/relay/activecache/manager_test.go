package activecache

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tiered "github.com/geektr-cloud/go-tiered-cache"
	"github.com/mirror-db/mirror-db/tools/relay/diskspace"
	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
	"go.uber.org/goleak"
)

const waitTimeout = 5 * time.Second

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func openTestStore(t *testing.T, capacity int64) (*hashstore.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := hashstore.Open(hashstore.Config{Dir: dir, Capacity: capacity})
	if err != nil {
		t.Fatalf("open hash store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, dir
}

func newTestManager(t *testing.T, store *hashstore.Store, dir string, mutate func(*Config)) *Manager {
	t.Helper()
	cfg := Config{
		Dir:         dir,
		Capacity:    16 << 20,
		MaxEntries:  32,
		IdleTimeout: time.Minute,
		Store:       store,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func digestKey(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// testSource is a controllable range upstream. It always reports the complete
// extent before serving a section, so the tiered core learns the object size
// without an extra request.
type testSource struct {
	data  []byte
	gate  <-chan struct{}
	opens atomic.Int32
}

func (s *testSource) ReadSection(ctx context.Context, offset, size int64, extents chan<- tiered.Extent) (io.ReadCloser, error) {
	s.opens.Add(1)
	if offset < 0 || size < 0 {
		return nil, tiered.ErrInvalidRange
	}
	select {
	case extents <- tiered.Extent{End: int64(len(s.data))}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.gate != nil {
		select {
		case <-s.gate:
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
	return io.NopCloser(io.NewSectionReader(bytes.NewReader(s.data), offset, end-offset)), nil
}

func readAll(t *testing.T, r io.ReadCloser) []byte {
	t.Helper()
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return data
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitPublished(t *testing.T, e *Entry) hashstore.Content {
	t.Helper()
	var content hashstore.Content
	waitFor(t, "publish", func() bool {
		c, ok := e.Content()
		if ok {
			content = c
		}
		return ok
	})
	return content
}

func scratchFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".tmp", scratchPrefix+"*"))
	if err != nil {
		t.Fatalf("glob scratch: %v", err)
	}
	return matches
}

func TestAcquireCoalescesConcurrentReaders(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	data := bytes.Repeat([]byte("abcdefghij"), 32*1024)
	key := digestKey(data)

	m := newTestManager(t, store, dir, nil)
	ctx := context.Background()

	creator, created, err := m.Acquire(ctx, key)
	if err != nil || !created {
		t.Fatalf("first acquire: created=%v err=%v", created, err)
	}

	type result struct {
		entry   *Entry
		created bool
		data    []byte
		err     error
	}
	const readers = 4
	results := make(chan result, readers)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			ready.Done()
			<-start
			e, c, err := m.Acquire(ctx, key)
			if err != nil {
				results <- result{entry: e, created: c, err: err}
				return
			}
			r, err := e.Read(ctx)
			if err != nil {
				e.Release()
				results <- result{entry: e, created: c, err: err}
				return
			}
			body, err := io.ReadAll(r)
			_ = r.Close()
			e.Release()
			results <- result{entry: e, created: c, data: body, err: err}
		}()
	}
	ready.Wait()
	close(start)
	// Give the waiters a chance to block in Acquire before Init publishes the
	// entry; either way they must reuse the same Entry.
	time.Sleep(20 * time.Millisecond)

	source := &testSource{data: data}
	if err := creator.Init(source, "meta", int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	creatorBody := readAll(t, mustRead(t, creator))
	creator.Release()

	for i := 0; i < readers; i++ {
		res := <-results
		if res.err != nil {
			t.Fatalf("reader: %v", res.err)
		}
		if res.created {
			t.Fatalf("a concurrent reader created a second entry")
		}
		if res.entry != creator {
			t.Fatalf("reader got a different entry")
		}
		if !bytes.Equal(res.data, data) {
			t.Fatalf("reader body mismatch")
		}
	}
	if !bytes.Equal(creatorBody, data) {
		t.Fatalf("creator body mismatch")
	}

	content := waitPublished(t, creator)
	if content.SHA256 != sha256Hex(data) || content.SHA1 != sha1Hex(data) || content.Size != int64(len(data)) {
		t.Fatalf("published content = %+v", content)
	}

	r, size, err := store.Get(ctx, hashstore.SHA256, content.SHA256)
	if err != nil {
		t.Fatalf("hash store get: %v", err)
	}
	defer r.Close()
	if size != int64(len(data)) {
		t.Fatalf("hash store size = %d", size)
	}
	stored, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatalf("hash store body mismatch: %v", err)
	}

	waitFor(t, "scratch cleanup", func() bool { return len(scratchFiles(t, dir)) == 0 })
}

func TestOldReaderContinuesAfterPublish(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	data := bytes.Repeat([]byte("0123456789"), 40*1024)
	key := digestKey(data)

	m := newTestManager(t, store, dir, nil)
	ctx := context.Background()
	entry, created, err := m.Acquire(ctx, key)
	if err != nil || !created {
		t.Fatalf("acquire: %v", err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}

	old, err := entry.Read(ctx)
	if err != nil {
		t.Fatalf("old reader: %v", err)
	}
	head := make([]byte, 1024)
	if _, err := io.ReadFull(old, head); err != nil {
		t.Fatalf("read head: %v", err)
	}

	second, err := entry.Read(ctx)
	if err != nil {
		t.Fatalf("second reader: %v", err)
	}
	if got := readAll(t, second); !bytes.Equal(got, data) {
		t.Fatalf("second reader body mismatch")
	}
	waitPublished(t, entry)

	rest, err := io.ReadAll(old)
	if err != nil {
		t.Fatalf("continue old reader: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close old reader: %v", err)
	}
	if !bytes.Equal(append(head, rest...), data) {
		t.Fatalf("old reader lost the tail after publish")
	}
	entry.Release()
	waitFor(t, "scratch cleanup", func() bool { return len(scratchFiles(t, dir)) == 0 })
}

func TestCopyFinalizePath(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	data := bytes.Repeat([]byte("copy-path-"), 16*1024)

	m := newTestManager(t, store, dir, func(cfg *Config) { cfg.disableHandoff = true })
	ctx := context.Background()
	entry, created, err := m.Acquire(ctx, digestKey(data))
	if err != nil || !created {
		t.Fatalf("acquire: %v", err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := readAll(t, mustRead(t, entry)); !bytes.Equal(got, data) {
		t.Fatalf("body mismatch")
	}
	content := waitPublished(t, entry)
	if content.SHA256 != sha256Hex(data) {
		t.Fatalf("published sha256 = %s", content.SHA256)
	}
	if _, ok, err := store.Stat(ctx, hashstore.SHA256, content.SHA256); err != nil || !ok {
		t.Fatalf("hash store stat: ok=%v err=%v", ok, err)
	}
	entry.Release()
	waitFor(t, "scratch cleanup", func() bool { return len(scratchFiles(t, dir)) == 0 })
}

func TestDigestMismatchPublishesNothing(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	data := bytes.Repeat([]byte("mismatch"), 8*1024)
	realKey := digestKey(data)
	wrongKey := "sha256:" + sha256Hex([]byte("other"))

	m := newTestManager(t, store, dir, nil)
	ctx := context.Background()
	entry, created, err := m.Acquire(ctx, wrongKey)
	if err != nil || !created {
		t.Fatalf("acquire: %v", err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := readAll(t, mustRead(t, entry)); !bytes.Equal(got, data) {
		t.Fatalf("body mismatch")
	}

	waitFor(t, "publish failure", func() bool {
		return errors.Is(entry.Err(), hashstore.ErrDigestMismatch)
	})
	if _, ok := entry.Content(); ok {
		t.Fatalf("mismatched body reported as published")
	}
	if _, ok, err := store.Stat(ctx, hashstore.SHA256, sha256Hex(data)); err != nil || ok {
		t.Fatalf("expected no stored body, got ok=%v err=%v", ok, err)
	}
	// The failed key is gone; a fresh entry for the correct digest is allowed.
	entry2, created2, err2 := m.Acquire(ctx, realKey)
	if err2 != nil || !created2 {
		t.Fatalf("re-acquire after failure: created=%v err=%v", created2, err2)
	}
	entry2.Fail(nil)
	entry2.Release()
	entry.Release()
}

func TestDifferentKeysAreIndependent(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	firstData := bytes.Repeat([]byte("first"), 4*1024)
	secondData := bytes.Repeat([]byte("second"), 6*1024)
	m := newTestManager(t, store, dir, nil)
	ctx := context.Background()

	first, created, err := m.Acquire(ctx, digestKey(firstData))
	if err != nil || !created {
		t.Fatalf("acquire first: %v", err)
	}
	second, created, err := m.Acquire(ctx, digestKey(secondData))
	if err != nil || !created {
		t.Fatalf("acquire second: %v", err)
	}
	if first == second {
		t.Fatalf("different keys shared one entry")
	}
	if err := first.Init(&testSource{data: firstData}, nil, int64(len(firstData))); err != nil {
		t.Fatalf("init first: %v", err)
	}
	if err := second.Init(&testSource{data: secondData}, nil, int64(len(secondData))); err != nil {
		t.Fatalf("init second: %v", err)
	}
	if got := readAll(t, mustRead(t, first)); !bytes.Equal(got, firstData) {
		t.Fatalf("first body mismatch")
	}
	if got := readAll(t, mustRead(t, second)); !bytes.Equal(got, secondData) {
		t.Fatalf("second body mismatch")
	}
	if c := waitPublished(t, first); c.SHA256 != sha256Hex(firstData) {
		t.Fatalf("first content = %+v", c)
	}
	if c := waitPublished(t, second); c.SHA256 != sha256Hex(secondData) {
		t.Fatalf("second content = %+v", c)
	}
	first.Release()
	second.Release()
}

func TestReadSectionsPublishCompleteObject(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	data := bytes.Repeat([]byte("ranges"), 8*1024)
	m := newTestManager(t, store, dir, nil)
	ctx := context.Background()

	entry, created, err := m.Acquire(ctx, digestKey(data))
	if err != nil || !created {
		t.Fatalf("acquire: %v", err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	const section = 4096
	var got []byte
	for offset := int64(0); offset < int64(len(data)); offset += section {
		size := int64(section)
		if offset+size > int64(len(data)) {
			size = int64(len(data)) - offset
		}
		part, err := entry.ReadSection(ctx, offset, size)
		if err != nil {
			t.Fatalf("read section at %d: %v", offset, err)
		}
		got = append(got, readAll(t, part)...)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("section bodies mismatch")
	}
	content := waitPublished(t, entry)
	if content.SHA256 != sha256Hex(data) {
		t.Fatalf("published content = %+v", content)
	}
	entry.Release()
	waitFor(t, "scratch cleanup", func() bool { return len(scratchFiles(t, dir)) == 0 })
}

func mustRead(t *testing.T, e *Entry) io.ReadCloser {
	t.Helper()
	r, err := e.Read(context.Background())
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	return r
}

func TestMaxEntriesRejectsNewKeys(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	m := newTestManager(t, store, dir, func(cfg *Config) { cfg.MaxEntries = 1 })
	ctx := context.Background()

	first, created, err := m.Acquire(ctx, "one")
	if err != nil || !created {
		t.Fatalf("acquire first: %v", err)
	}
	if _, _, err := m.Acquire(ctx, "two"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("expected ErrCapacity, got %v", err)
	}
	first.Fail(nil)
	first.Release()
	waitFor(t, "key release", func() bool {
		e, created, err := m.Acquire(ctx, "two")
		if err != nil {
			return false
		}
		if !created {
			e.Release()
			return false
		}
		e.Fail(nil)
		e.Release()
		return true
	})
}

func TestCapacityReservationAndWaiterWakes(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	m := newTestManager(t, store, dir, func(cfg *Config) { cfg.Capacity = 1024 })
	ctx := context.Background()

	entry, created, err := m.Acquire(ctx, "big")
	if err != nil || !created {
		t.Fatalf("acquire: %v", err)
	}

	waiter := make(chan error, 1)
	go func() {
		_, _, err := m.Acquire(ctx, "big")
		waiter <- err
	}()
	time.Sleep(20 * time.Millisecond)

	if err := entry.Init(&testSource{data: make([]byte, 64)}, nil, 4096); !errors.Is(err, ErrCapacity) {
		t.Fatalf("expected ErrCapacity, got %v", err)
	}
	select {
	case err := <-waiter:
		if !errors.Is(err, ErrCapacity) {
			t.Fatalf("waiter error = %v", err)
		}
	case <-time.After(waitTimeout):
		t.Fatalf("waiter was not woken by Init failure")
	}
	entry.Release()

	// The failed reservation must be released for the next entry.
	next, created, err := m.Acquire(ctx, "small")
	if err != nil || !created {
		t.Fatalf("acquire after failure: created=%v err=%v", created, err)
	}
	if err := next.Init(&testSource{data: make([]byte, 8)}, nil, 512); err != nil {
		t.Fatalf("init small: %v", err)
	}
	next.Fail(nil)
	next.Release()
}

func TestUnknownSizeBlocksNewEntries(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	data := bytes.Repeat([]byte("unknown"), 8*1024)
	m := newTestManager(t, store, dir, nil)
	ctx := context.Background()

	entry, created, err := m.Acquire(ctx, digestKey(data))
	if err != nil || !created {
		t.Fatalf("acquire: %v", err)
	}
	if err := entry.Init(&testSource{data: data}, nil, -1); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, _, err := m.Acquire(ctx, "another"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("expected unknown-size gate, got %v", err)
	}
	if got := readAll(t, mustRead(t, entry)); !bytes.Equal(got, data) {
		t.Fatalf("body mismatch")
	}
	waitPublished(t, entry)
	entry.Release()

	waitFor(t, "unknown entry retire", func() bool {
		e, created, err := m.Acquire(ctx, "another")
		if err != nil {
			return false
		}
		if !created {
			e.Release()
			return false
		}
		e.Fail(nil)
		e.Release()
		return true
	})
}

func TestLowDiskRejectsNewEntriesButFinishesAdmitted(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	data := bytes.Repeat([]byte("water"), 16*1024)

	var free atomic.Int64
	free.Store(1 << 30)
	probe := func(string) (int64, error) { return free.Load(), nil }

	m := newTestManager(t, store, dir, func(cfg *Config) {
		cfg.DiskWaterMark = 1024
		cfg.diskOptions = []diskspace.Option{
			diskspace.WithProbe(probe),
			diskspace.WithRecheck(1, time.Nanosecond),
		}
	})
	ctx := context.Background()

	rejected, created, err := m.Acquire(ctx, "rejected")
	if err != nil || !created || rejected == nil {
		t.Fatalf("acquire: %v", err)
	}
	rejected.Fail(nil)
	rejected.Release()

	entry, created, err := m.Acquire(ctx, digestKey(data))
	if err != nil || !created {
		t.Fatalf("acquire admitted entry: %v", err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}

	// The admitted entry must finish with complete bytes even when the
	// watermark trips mid-write.
	free.Store(0)
	if got := readAll(t, mustRead(t, entry)); !bytes.Equal(got, data) {
		t.Fatalf("admitted entry lost bytes after low disk")
	}
	waitPublished(t, entry)

	if _, _, err := m.Acquire(ctx, "after-low-disk"); !errors.Is(err, ErrLowDiskSpace) {
		t.Fatalf("expected ErrLowDiskSpace, got %v", err)
	}
	entry.Release()
}

func TestLowDiskRejectsAtAdmission(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	probe := func(string) (int64, error) { return 1, nil }
	m := newTestManager(t, store, dir, func(cfg *Config) {
		cfg.DiskWaterMark = 1024
		cfg.diskOptions = []diskspace.Option{diskspace.WithProbe(probe)}
	})

	if _, _, err := m.Acquire(context.Background(), "k"); !errors.Is(err, ErrLowDiskSpace) {
		t.Fatalf("expected ErrLowDiskSpace, got %v", err)
	}
}

func TestPartialRangeIsReapedWhenIdle(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	data := bytes.Repeat([]byte("partial"), 32*1024)
	m := newTestManager(t, store, dir, func(cfg *Config) {
		cfg.IdleTimeout = 60 * time.Millisecond
	})
	ctx := context.Background()

	entry, created, err := m.Acquire(ctx, digestKey(data))
	if err != nil || !created {
		t.Fatalf("acquire: %v", err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	r, err := entry.ReadSection(ctx, 0, 128)
	if err != nil {
		t.Fatalf("read section: %v", err)
	}
	if got := readAll(t, r); !bytes.Equal(got, data[:128]) {
		t.Fatalf("section body mismatch")
	}
	entry.Release()

	waitFor(t, "idle reap", func() bool {
		e, created, err := m.Acquire(ctx, digestKey(data))
		if err != nil {
			return false
		}
		if !created {
			e.Release()
			return false
		}
		e.Fail(nil)
		e.Release()
		return true
	})
	waitFor(t, "scratch cleanup", func() bool { return len(scratchFiles(t, dir)) == 0 })
}

func TestAcquireWaitsForLateInitAndFailWakes(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	data := []byte("late-init")
	m := newTestManager(t, store, dir, nil)
	ctx := context.Background()

	creator, created, err := m.Acquire(ctx, digestKey(data))
	if err != nil || !created {
		t.Fatalf("acquire: %v", err)
	}
	waiter := make(chan error, 1)
	go func() {
		_, _, err := m.Acquire(ctx, digestKey(data))
		waiter <- err
	}()
	time.Sleep(20 * time.Millisecond)
	creator.Fail(errors.New("upstream unavailable"))
	creator.Release()

	select {
	case err := <-waiter:
		if err == nil || err.Error() != "upstream unavailable" {
			t.Fatalf("waiter error = %v", err)
		}
	case <-time.After(waitTimeout):
		t.Fatalf("waiter was not woken")
	}
}

// TestInitFactoryAdmissionFailureLeavesSourceUntouched proves the preflight
// order: a capacity refusal happens before build, so the caller can deliver the
// response it already fetched through the ordinary path.
func TestInitFactoryAdmissionFailureLeavesSourceUntouched(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	m := newTestManager(t, store, dir, func(c *Config) { c.Capacity = 64 })
	entry, created, err := m.Acquire(context.Background(), digestKey([]byte("preflight")))
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	defer entry.Release()

	built := false
	err = entry.InitFactory(nil, 1024, func() (tiered.UpstreamProvider, error) {
		built = true
		return &testSource{data: make([]byte, 1024)}, nil
	})
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("InitFactory error = %v, want ErrCapacity", err)
	}
	if built {
		t.Fatal("builder ran before admission succeeded")
	}
	if entry.Err() == nil {
		t.Fatal("refused entry is not terminal")
	}
	if len(scratchFiles(t, dir)) != 0 {
		t.Fatalf("refused entry left scratch: %v", scratchFiles(t, dir))
	}
}

// TestEntryStoreCopyFallbackHonoursWatermark covers the one local write that
// does not go through scratchCache: the copy fallback's own temp file.
func TestEntryStoreCopyFallbackHonoursWatermark(t *testing.T) {
	store, dir := openTestStore(t, 1<<20)
	m := newTestManager(t, store, dir, func(c *Config) {
		c.DiskWaterMark = 1 << 20
		c.diskOptions = []diskspace.Option{
			diskspace.WithProbe(func(string) (int64, error) { return 0, nil }),
		}
	})
	e := newEntry(m, "")
	st := newEntryStore(e, hashstore.SHA256, sha256Hex([]byte("body")), m.adopt, false)
	defer func() { _ = st.close() }()

	if _, err := st.Write([]byte("body")); !errors.Is(err, ErrLowDiskSpace) {
		t.Fatalf("copy write error = %v, want ErrLowDiskSpace", err)
	}
	matches, err := filepath.Glob(filepath.Join(m.tmpDir, scratchPrefix+"store-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("low-disk copy created temp files: %v", matches)
	}
}

// TestCopyFallbackLowDiskSettlesEntry proves a watermark refusal on the copy
// fallback path fails the entry immediately instead of leaving a delivered body
// unpublished until the idle timeout.
func TestCopyFallbackLowDiskSettlesEntry(t *testing.T) {
	store, dir := openTestStore(t, 16<<20)
	var probes atomic.Int32
	m := newTestManager(t, store, dir, func(c *Config) {
		c.DiskWaterMark = 1
		c.disableHandoff = true
		c.diskOptions = []diskspace.Option{
			diskspace.WithProbe(func(string) (int64, error) {
				// Acquire and admit both pass; the copy write sees a full disk.
				if probes.Add(1) <= 2 {
					return 1 << 30, nil
				}
				return 0, nil
			}),
		}
	})
	data := bytes.Repeat([]byte("lowdisk"), 1024)
	entry, created, err := m.Acquire(context.Background(), digestKey(data))
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	defer entry.Release()
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}

	reader, err := entry.Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_, _ = io.ReadAll(reader)
	_ = reader.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := entry.WaitPublished(ctx); !errors.Is(err, ErrLowDiskSpace) {
		t.Fatalf("WaitPublished = %v, want ErrLowDiskSpace", err)
	}
}
