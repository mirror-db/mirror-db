package hashstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/diskspace"
)

// spaceProbe is a controllable filesystem free-space figure for the watermark
// tests; no real disk size is involved.
type spaceProbe struct {
	mu   sync.Mutex
	free int64
}

func (p *spaceProbe) probe(string) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.free, nil
}

func (p *spaceProbe) setFree(free int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.free = free
}

func watermarkConfig(dir string, watermark int64, opts ...diskspace.Option) Config {
	return Config{
		Dir:           dir,
		Capacity:      1 << 20,
		DiskWaterMark: watermark,
		diskOptions:   opts,
	}
}

// assertNothingStored checks every place a rejected Put could have left a
// trace: the LRU, the index (records and aliases), the temp directory and the
// local body tree.
func assertNothingStored(t *testing.T, s *Store, dir string) {
	t.Helper()
	if st := s.Stats(); st.Objects != 0 || st.Bytes != 0 {
		t.Fatalf("store accounting not empty: %+v", st)
	}
	records, err := s.index.records()
	if err != nil {
		t.Fatalf("index records: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("index holds %d records", len(records))
	}
	tmpEntries, err := os.ReadDir(filepath.Join(dir, tmpDirName))
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(tmpEntries) != 0 {
		t.Fatalf("temp dir holds %d files", len(tmpEntries))
	}
	// data/ is only created by a successful backend put.
	if entries, err := os.ReadDir(filepath.Join(dir, dataDirName)); err == nil && len(entries) != 0 {
		t.Fatalf("body tree holds %d entries", len(entries))
	}
}

func TestPutRejectsBelowWaterMarkBeforeCreatingTemp(t *testing.T) {
	dir := t.TempDir()
	probe := &spaceProbe{free: 10}
	s := openLocal(t, watermarkConfig(dir, 1<<10, diskspace.WithProbe(probe.probe)))

	_, err := s.Put(context.Background(), strings.NewReader("hello"), 5)
	if !errors.Is(err, diskspace.ErrLowDiskSpace) {
		t.Fatalf("got %v, want ErrLowDiskSpace", err)
	}
	assertNothingStored(t, s, dir)
}

func TestPutRejectsWhenWaterMarkCrossedMidWrite(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int64
	probe := func(string) (int64, error) {
		// The pre-write check passes; the first mid-write recheck does not.
		if calls.Add(1) == 1 {
			return 1 << 40, nil
		}
		return 0, nil
	}
	s := openLocal(t, watermarkConfig(dir, 1,
		diskspace.WithProbe(probe),
		diskspace.WithRecheck(4, time.Hour),
	))

	body := strings.Repeat("x", 32)
	_, err := s.Put(context.Background(), strings.NewReader(body), int64(len(body)))
	if !errors.Is(err, diskspace.ErrLowDiskSpace) {
		t.Fatalf("got %v, want ErrLowDiskSpace", err)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("probe calls = %d, want a pre-write and a mid-write probe", got)
	}
	// The partial temp file must be gone, and nothing may reach the index or the
	// body tree.
	assertNothingStored(t, s, dir)
}

func TestDisabledWaterMarkNeverProbes(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int64
	probe := func(string) (int64, error) {
		calls.Add(1)
		return 0, errors.New("probe must not run while disabled")
	}
	s := openLocal(t, watermarkConfig(dir, 0, diskspace.WithProbe(probe)))

	content, err := s.Put(context.Background(), strings.NewReader("hello"), 5)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("probe called %d times with the watermark disabled", got)
	}
	rc, size, err := s.Get(context.Background(), SHA256, content.SHA256)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	if size != 5 {
		t.Fatalf("size = %d", size)
	}
}

func TestReadsAndDeletesSurviveLowWaterMark(t *testing.T) {
	dir := t.TempDir()
	probe := &spaceProbe{free: 1 << 40}
	s := openLocal(t, watermarkConfig(dir, 1, diskspace.WithProbe(probe.probe)))

	body := "already stored"
	content, err := s.Put(context.Background(), strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("seed put: %v", err)
	}

	probe.setFree(0)
	if _, err := s.Put(context.Background(), strings.NewReader("new"), 3); !errors.Is(err, diskspace.ErrLowDiskSpace) {
		t.Fatalf("low-water put got %v, want ErrLowDiskSpace", err)
	}

	// Reads are explicitly outside the guard.
	rc, size, err := s.Get(context.Background(), SHA256, content.SHA256)
	if err != nil {
		t.Fatalf("get under low water: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != body || size != int64(len(body)) {
		t.Fatalf("got %q (%d bytes), want %q", got, size, body)
	}

	// So are deletions: prune must be able to free space exactly when it is low.
	if err := s.Delete(context.Background(), SHA256, content.SHA256); err != nil {
		t.Fatalf("delete under low water: %v", err)
	}
	if st := s.Stats(); st.Objects != 0 {
		t.Fatalf("object survived delete: %+v", st)
	}
}

func TestSharedBucketLocalTempIsGated(t *testing.T) {
	fake := newFakeS3()
	dir := t.TempDir()
	probe := &spaceProbe{free: 10}
	cfg := watermarkConfig(dir, 1<<10, diskspace.WithProbe(probe.probe))
	cfg.S3 = &S3Config{Bucket: "bucket", Prefix: "relay", Region: "us-east-1"}

	s, err := open(cfg, newS3Backend(fake, *cfg.S3))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	s.wg.Wait()

	if _, err := s.Put(context.Background(), strings.NewReader("hello"), 5); !errors.Is(err, diskspace.ErrLowDiskSpace) {
		t.Fatalf("got %v, want ErrLowDiskSpace", err)
	}
	if len(fake.objects) != 0 {
		t.Fatalf("shared bucket received %d objects", len(fake.objects))
	}
	assertNothingStored(t, s, dir)
}

func TestDiskWaterMarkMustNotBeNegative(t *testing.T) {
	cfg := Config{Dir: t.TempDir(), Capacity: 1 << 20, DiskWaterMark: -1}
	if err := cfg.Normalize(); err == nil {
		t.Fatal("a negative disk water mark must be rejected")
	}
}
