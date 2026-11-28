package activecache

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// fakeAdopter simulates a shared object store: Adopt computes the digests and
// leaves the source path alone, so the only way a later read can work is the
// local descriptor opened before publication.
type fakeAdopter struct {
	mu       sync.Mutex
	calls    int
	leftover string
}

func (f *fakeAdopter) Adopt(ctx context.Context, path string, size int64, expectAlgorithm hashstore.Algorithm, expectDigest string) (hashstore.Content, error) {
	if err := ctx.Err(); err != nil {
		return hashstore.Content{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return hashstore.Content{}, err
	}
	if int64(len(body)) != size {
		return hashstore.Content{}, fmt.Errorf("size %d, declared %d", len(body), size)
	}
	sha1sum := sha1.Sum(body)
	sha256sum := sha256.Sum256(body)
	c := hashstore.Content{
		SHA1:   hex.EncodeToString(sha1sum[:]),
		SHA256: hex.EncodeToString(sha256sum[:]),
		Size:   size,
	}
	if expectAlgorithm != "" {
		got := c.SHA256
		if expectAlgorithm == hashstore.SHA1 {
			got = c.SHA1
		}
		if got != expectDigest {
			return hashstore.Content{}, fmt.Errorf("%w: %s", hashstore.ErrDigestMismatch, got)
		}
	}
	f.mu.Lock()
	f.calls++
	f.leftover = path
	f.mu.Unlock()
	return c, nil
}

func (f *fakeAdopter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestSharedStoreReadsUseLocalDescriptor(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeAdopter{}
	cfg := Config{
		Dir:         dir,
		Capacity:    8 << 20,
		MaxEntries:  4,
		IdleTimeout: time.Minute,
		Adopt:       fake,
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	data := bytes.Repeat([]byte("shared-store"), 8*1024)
	ctx := context.Background()
	entry, created, err := m.Acquire(ctx, digestKey(data))
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
	head := make([]byte, 512)
	if _, err := io.ReadFull(old, head); err != nil {
		t.Fatalf("read head: %v", err)
	}

	second := mustRead(t, entry)
	if got := readAll(t, second); !bytes.Equal(got, data) {
		t.Fatalf("second reader mismatch")
	}
	waitPublished(t, entry)
	if got := fake.count(); got != 1 {
		t.Fatalf("adopt called %d times", got)
	}

	rest, err := io.ReadAll(old)
	if err != nil {
		t.Fatalf("continue old reader: %v", err)
	}
	_ = old.Close()
	if !bytes.Equal(append(head, rest...), data) {
		t.Fatalf("old reader could not continue from the local descriptor")
	}
	if got := fake.count(); got != 1 {
		t.Fatalf("reads went back to the shared store: adopt called %d times", got)
	}
	entry.Release()
}

func TestHandoffReclaimsScratchName(t *testing.T) {
	store, dir := openTestStore(t, 32<<20)
	data := bytes.Repeat([]byte("handoff"), 8*1024)
	m := newTestManager(t, store, dir, nil)
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
	entry.Release()

	waitFor(t, "scratch reclaim", func() bool { return len(scratchFiles(t, dir)) == 0 })
	if _, ok, err := store.Stat(ctx, hashstore.SHA256, content.SHA256); err != nil || !ok {
		t.Fatalf("published body missing: ok=%v err=%v", ok, err)
	}
}
