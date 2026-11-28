package activecache

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// recordingTrustedAdopter is an adopter with the trusted capability. It verifies
// the declared digests against the bytes on disk (test-side only) and records
// which entry point published, so a test can prove the assembly digest was used
// instead of a reread.
type recordingTrustedAdopter struct {
	fakeAdopter

	mu           sync.Mutex
	trustedCalls int
	trusted      hashstore.Content
}

func (a *recordingTrustedAdopter) AdoptTrusted(ctx context.Context, path string, c hashstore.Content, algo hashstore.Algorithm, digest string) (hashstore.Content, error) {
	if err := ctx.Err(); err != nil {
		return hashstore.Content{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return hashstore.Content{}, err
	}
	sum1 := sha1.Sum(body)
	sum256 := sha256.Sum256(body)
	want := hashstore.Content{
		SHA1:   hex.EncodeToString(sum1[:]),
		SHA256: hex.EncodeToString(sum256[:]),
		Size:   int64(len(body)),
	}
	if c != want {
		return hashstore.Content{}, fmt.Errorf("trusted content %+v does not describe the body %+v", c, want)
	}
	if algo != "" {
		got := c.SHA256
		if algo == hashstore.SHA1 {
			got = c.SHA1
		}
		if got != digest {
			return hashstore.Content{}, fmt.Errorf("%w: %s", hashstore.ErrDigestMismatch, got)
		}
	}
	a.mu.Lock()
	a.trustedCalls++
	a.trusted = c
	a.mu.Unlock()
	return c, nil
}

func (a *recordingTrustedAdopter) trustedCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.trustedCalls
}

func (a *recordingTrustedAdopter) trustedContent() hashstore.Content {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.trusted
}

func newTrustedManager(t *testing.T, dir string, adopter *recordingTrustedAdopter, mutate func(*Config)) *Manager {
	t.Helper()
	cfg := Config{
		Dir:         dir,
		Capacity:    16 << 20,
		MaxEntries:  32,
		IdleTimeout: time.Minute,
		Adopt:       adopter,
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

func trustedBody(t *testing.T) []byte {
	t.Helper()
	data := make([]byte, 32*1024)
	for i := range data {
		data[i] = byte(i*11 + 5)
	}
	return data
}

// A full assembly publishes through AdoptTrusted with the digests the sinks
// computed, and the hash store is never asked to reread the file.
func TestTrustedAdopterPublishesPrecomputedDigest(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingTrustedAdopter{}
	m := newTrustedManager(t, dir, rec, nil)
	data := trustedBody(t)

	entry, created, err := m.Acquire(context.Background(), digestKey(data))
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := readAll(t, mustRead(t, entry)); !bytes.Equal(got, data) {
		t.Fatalf("body mismatch")
	}
	content := waitPublished(t, entry)
	entry.Release()

	if got := rec.trustedCount(); got != 1 {
		t.Fatalf("AdoptTrusted calls = %d, want 1", got)
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("untrusted Adopt calls = %d, want 0", got)
	}
	if content != rec.trustedContent() {
		t.Fatalf("published content = %+v, recorded = %+v", content, rec.trustedContent())
	}
	if content.SHA1 != sha1Hex(data) || content.SHA256 != sha256Hex(data) || content.Size != int64(len(data)) {
		t.Fatalf("published content = %+v", content)
	}
	waitFor(t, "scratch cleanup", func() bool { return len(scratchFiles(t, dir)) == 0 })
}

// An adopter without the trusted capability keeps the verification path.
func TestAdoptOnlyAdopterFallsBack(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeAdopter{}
	cfg := Config{
		Dir:         dir,
		Capacity:    16 << 20,
		MaxEntries:  4,
		IdleTimeout: time.Minute,
		Adopt:       fake,
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	data := trustedBody(t)

	entry, created, err := m.Acquire(context.Background(), digestKey(data))
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := readAll(t, mustRead(t, entry)); !bytes.Equal(got, data) {
		t.Fatalf("body mismatch")
	}
	content := waitPublished(t, entry)
	entry.Release()
	if got := fake.count(); got != 1 {
		t.Fatalf("Adopt calls = %d, want 1", got)
	}
	if content.SHA256 != sha256Hex(data) {
		t.Fatalf("content = %+v", content)
	}
}

// The copy fallback still consumes the PrefixSinks, so it publishes through
// AdoptTrusted too.
func TestTrustedCopyFallbackUsesPrecomputedDigest(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingTrustedAdopter{}
	m := newTrustedManager(t, dir, rec, func(cfg *Config) { cfg.disableHandoff = true })
	data := trustedBody(t)

	entry, created, err := m.Acquire(context.Background(), digestKey(data))
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := readAll(t, mustRead(t, entry)); !bytes.Equal(got, data) {
		t.Fatalf("body mismatch")
	}
	content := waitPublished(t, entry)
	entry.Release()
	if got := rec.trustedCount(); got != 1 {
		t.Fatalf("AdoptTrusted calls = %d, want 1", got)
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("untrusted Adopt calls = %d, want 0", got)
	}
	if content.SHA256 != sha256Hex(data) {
		t.Fatalf("content = %+v", content)
	}
}

// An unknown-size object assembled from out-of-order ranges still completes its
// prefix before publication, so the trusted digest is the whole body's.
func TestTrustedUnknownSizeOutOfOrderRanges(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingTrustedAdopter{}
	m := newTrustedManager(t, dir, rec, nil)
	data := trustedBody(t)

	entry, created, err := m.Acquire(context.Background(), "unknown-order")
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if err := entry.Init(&testSource{data: data}, nil, -1); err != nil {
		t.Fatalf("init: %v", err)
	}

	const section = 4096
	// Back to front, so the prefix is filled last.
	for offset := int64(len(data) - section); offset >= 0; offset -= section {
		size := int64(section)
		if offset+size > int64(len(data)) {
			size = int64(len(data)) - offset
		}
		part, err := entry.ReadSection(context.Background(), offset, size)
		if err != nil {
			t.Fatalf("read section at %d: %v", offset, err)
		}
		got := readAll(t, part)
		if !bytes.Equal(got, data[offset:offset+size]) {
			t.Fatalf("section at %d mismatch", offset)
		}
	}
	content := waitPublished(t, entry)
	entry.Release()
	if got := rec.trustedCount(); got != 1 {
		t.Fatalf("AdoptTrusted calls = %d, want 1", got)
	}
	if content.SHA256 != sha256Hex(data) || content.Size != int64(len(data)) {
		t.Fatalf("content = %+v", content)
	}
}

// A partially read object never publishes, so no digest is committed for bytes
// the cache never held.
func TestTrustedPartialReadDoesNotPublish(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingTrustedAdopter{}
	m := newTrustedManager(t, dir, rec, nil)
	data := trustedBody(t)

	entry, created, err := m.Acquire(context.Background(), digestKey(data))
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	part, err := entry.ReadSection(context.Background(), 0, 512)
	if err != nil {
		t.Fatalf("read section: %v", err)
	}
	readAll(t, part)
	time.Sleep(50 * time.Millisecond)

	if _, ok := entry.Content(); ok {
		t.Fatal("a partial object was published")
	}
	if got := rec.trustedCount() + rec.count(); got != 0 {
		t.Fatalf("a partial object reached the adopter %d times", got)
	}
	entry.Release()
}

// Manager.Close retires an unfinished entry without publishing it.
func TestTrustedManagerCloseDoesNotPublish(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingTrustedAdopter{}
	m := newTrustedManager(t, dir, rec, nil)
	data := trustedBody(t)

	entry, created, err := m.Acquire(context.Background(), digestKey(data))
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := rec.trustedCount() + rec.count(); got != 0 {
		t.Fatalf("Close published %d times", got)
	}
	entry.Release()
}

// The real store's AdoptTrusted still enforces the expectation: a wrong key
// publishes nothing, and the mismatch surfaces through the entry.
func TestTrustedExpectationMismatchPublishesNothing(t *testing.T) {
	store, dir := openTestStore(t, 16<<20)
	m := newTestManager(t, store, dir, nil)
	data := trustedBody(t)
	wrongKey := digestKey([]byte("some other body"))

	entry, created, err := m.Acquire(context.Background(), wrongKey)
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := readAll(t, mustRead(t, entry)); !bytes.Equal(got, data) {
		t.Fatalf("body mismatch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if _, err := entry.WaitPublished(ctx); !errors.Is(err, hashstore.ErrDigestMismatch) {
		t.Fatalf("WaitPublished = %v, want ErrDigestMismatch", err)
	}
	if _, ok, err := store.Stat(context.Background(), hashstore.SHA256, sha256Hex(data)); err != nil || ok {
		t.Fatalf("mismatched body reached the store: ok=%v err=%v", ok, err)
	}
	entry.Release()
}

// A sink that did not see the whole object must not publish its digest: the
// entry falls back to Adopt, which rereads and verifies the file.
func TestTrustedByteCountMismatchFallsBackToAdopt(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingTrustedAdopter{}
	m := newTrustedManager(t, dir, rec, nil)

	data := trustedBody(t)
	src, err := os.CreateTemp(m.tmpDir, scratchPrefix+"short-*")
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	path := src.Name()
	if _, err := src.Write(data); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}

	// Both sinks saw only half the body: their digests describe a prefix, not
	// the object, so publication must not use them.
	sha1Sink := newDigestSink(sha1.New())
	sha256Sink := newDigestSink(sha256.New())
	if _, err := sha1Sink.Write(data[:len(data)/2]); err != nil {
		t.Fatal(err)
	}
	if _, err := sha256Sink.Write(data[:len(data)/2]); err != nil {
		t.Fatal(err)
	}

	e := newEntry(m, "")
	st := newEntryStore(e, "", "", m.adopt, false, sha1Sink, sha256Sink)
	t.Cleanup(func() { _ = st.close() })
	if err := st.publish(context.Background(), path, int64(len(data))); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := rec.trustedCount(); got != 0 {
		t.Fatalf("AdoptTrusted calls = %d, want 0 for a partial prefix", got)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("Adopt fallback calls = %d, want 1", got)
	}
	content, ok := e.Content()
	if !ok || content.SHA256 != sha256Hex(data) {
		t.Fatalf("published content = %+v ok=%v", content, ok)
	}
}
