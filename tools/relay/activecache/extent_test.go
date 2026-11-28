package activecache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	tiered "github.com/geektr-cloud/go-tiered-cache"
)

// extentProbeSource serves a body whose size is discoverable only at real EOF:
// it never reports an authoritative extent. Selected offsets can gate their
// first Read, and one offset can block inside ReadSection, which lets a test
// pin the order of upstream discovery.
type extentProbeSource struct {
	data  []byte
	gates map[int64]<-chan struct{}

	blockAt    int64
	blockEnter chan struct{}
	blockGate  <-chan struct{}
	blockOnce  sync.Once

	mu    sync.Mutex
	opens []int64
}

var _ tiered.UpstreamProvider = (*extentProbeSource)(nil)

func newExtentProbeSource(data []byte) *extentProbeSource {
	return &extentProbeSource{data: data, gates: make(map[int64]<-chan struct{}), blockAt: -1}
}

func (s *extentProbeSource) ReadSection(ctx context.Context, offset, size int64, extents chan<- tiered.Extent) (io.ReadCloser, error) {
	if offset < 0 || size < 0 {
		return nil, tiered.ErrInvalidRange
	}
	s.mu.Lock()
	s.opens = append(s.opens, offset)
	s.mu.Unlock()

	if offset == s.blockAt && s.blockEnter != nil {
		s.blockOnce.Do(func() { close(s.blockEnter) })
		select {
		case <-s.blockGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	end := offset + size
	if end > int64(len(s.data)) || end < offset {
		end = int64(len(s.data))
	}
	var reader io.Reader = bytes.NewReader(nil)
	if offset < end {
		reader = io.NewSectionReader(bytes.NewReader(s.data), offset, end-offset)
	}
	if gate, ok := s.gates[offset]; ok {
		reader = &gatedReader{ctx: ctx, gate: gate, r: reader}
	}
	return io.NopCloser(reader), nil
}

func (s *extentProbeSource) opened(offset int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, seen := range s.opens {
		if seen == offset {
			return true
		}
	}
	return false
}

// gatedReader blocks its first Read until gate is released or the upstream
// context is cancelled.
type gatedReader struct {
	ctx  context.Context
	gate <-chan struct{}
	r    io.Reader
}

func (g *gatedReader) Read(p []byte) (int, error) {
	select {
	case <-g.gate:
	case <-g.ctx.Done():
		return 0, g.ctx.Err()
	}
	return g.r.Read(p)
}

// TestWholeObjectReadSurvivesConcurrentSectionEOF covers an unknown-size entry
// (Init size -1) whose whole-object reader is still in flight when another
// reader opens a section past the real EOF from the middle. That section's
// upstream discovers the extent first; the whole-object reader must still
// deliver every byte and the entry must still publish.
//
// On go-tiered-cache v0.0.1 the failing upstream extends to the whole-object
// reader's synthetic end and fails it, truncating the body before the first
// gap and preventing publication. v0.0.2 clamps the failed interval to the
// discovered extent, so only the section within it is affected.
func TestWholeObjectReadSurvivesConcurrentSectionEOF(t *testing.T) {
	store, dir := openTestStore(t, 16<<20)
	m := newTestManager(t, store, dir, nil)
	ctx := context.Background()

	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i*7 + 3)
	}
	const mid = 2048
	const tailSize = 256
	far := int64(len(data)) + 512 // a section starting past the real EOF
	const overread = 512

	gateZero := make(chan struct{})
	gateMid := make(chan struct{})
	blockGate := make(chan struct{})
	blockEnter := make(chan struct{})
	var zeroOnce, midOnce, blockOnce sync.Once
	releaseZero := func() { zeroOnce.Do(func() { close(gateZero) }) }
	releaseMid := func() { midOnce.Do(func() { close(gateMid) }) }
	releaseBlock := func() { blockOnce.Do(func() { close(blockGate) }) }
	// Cleanups run LIFO, so releasing the gates here unblocks the tiered core
	// before the manager cleanup closes it.
	t.Cleanup(func() {
		releaseZero()
		releaseMid()
		releaseBlock()
	})

	source := newExtentProbeSource(data)
	source.gates[0] = gateZero
	source.gates[mid] = gateMid
	source.blockAt = far
	source.blockEnter = blockEnter
	source.blockGate = blockGate

	entry, created, err := m.Acquire(ctx, "unknown-extent-concurrent-eof")
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if err := entry.Init(source, nil, -1); err != nil {
		t.Fatalf("init: %v", err)
	}

	whole, err := entry.Read(ctx)
	if err != nil {
		t.Fatalf("whole read: %v", err)
	}
	waitFor(t, "whole-object upstream", func() bool { return source.opened(0) })

	// The middle section is longer than the remaining bytes, so its upstream is
	// the one that reaches the real EOF and discovers the extent.
	middle, err := entry.ReadSection(ctx, mid, int64(len(data)-mid)+overread)
	if err != nil {
		t.Fatalf("middle section: %v", err)
	}
	waitFor(t, "middle upstream", func() bool { return source.opened(mid) })

	// A third, never-read section past the extent parks the scheduler inside
	// ReadSection. While it is parked no reconcile runs, so the EOF failure
	// cannot be neutralised by pruning the middle upstream first. That keeps the
	// ordering deterministic instead of a race.
	tail, err := entry.ReadSection(ctx, far, tailSize)
	if err != nil {
		t.Fatalf("tail section: %v", err)
	}
	waitFor(t, "parked reconcile", func() bool {
		select {
		case <-blockEnter:
			return true
		default:
			return false
		}
	})

	// Let the middle upstream run to EOF and discover the extent.
	releaseMid()
	middleBody, middleErr := io.ReadAll(middle)
	_ = middle.Close()
	if !errors.Is(middleErr, io.ErrUnexpectedEOF) {
		t.Fatalf("middle section error = %v, want UnexpectedEOF", middleErr)
	}
	if !bytes.Equal(middleBody, data[mid:]) {
		t.Fatalf("middle section = %d bytes, want %d", len(middleBody), len(data)-mid)
	}

	// Drop the parked section and let the scheduler resume.
	_ = tail.Close()
	releaseBlock()

	// The whole-object reader must now continue from where it was gated.
	releaseZero()
	body, readErr := io.ReadAll(whole)
	_ = whole.Close()
	if readErr != nil {
		t.Fatalf("whole read error = %v, want nil", readErr)
	}
	if !bytes.Equal(body, data) {
		t.Fatalf("whole-object body = %d bytes, want %d", len(body), len(data))
	}
	if err := entry.Err(); err != nil {
		t.Fatalf("entry failed: %v", err)
	}
	content := waitPublished(t, entry)
	if content.SHA256 != sha256Hex(data) || content.Size != int64(len(data)) {
		t.Fatalf("published content = %+v", content)
	}
	entry.Release()
	waitFor(t, "scratch cleanup", func() bool { return len(scratchFiles(t, dir)) == 0 })
}
