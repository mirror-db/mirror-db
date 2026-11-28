package hashstore

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openLocal(t *testing.T, cfg Config) *Store {
	t.Helper()
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	// The startup reconcile runs in the background; tests start after it, so a
	// later assertion cannot race with the repair pass.
	s.wg.Wait()
	return s
}

func hashOf(body string) Content {
	h1 := sha1.Sum([]byte(body))
	h256 := sha256.Sum256([]byte(body))
	return Content{
		SHA1:   hex.EncodeToString(h1[:]),
		SHA256: hex.EncodeToString(h256[:]),
		Size:   int64(len(body)),
	}
}

func putBody(t *testing.T, s *Store, body string) Content {
	t.Helper()
	c, err := s.Put(context.Background(), strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("put %d-byte body: %v", len(body), err)
	}
	return c
}

func readContent(t *testing.T, s *Store, algo Algorithm, digest string) string {
	t.Helper()
	rc, size, err := s.Get(context.Background(), algo, digest)
	if err != nil {
		t.Fatalf("get %s/%s: %v", algo, digest, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s/%s: %v", algo, digest, err)
	}
	if int64(len(data)) != size {
		t.Fatalf("%s/%s: got %d bytes, size reported %d", algo, digest, len(data), size)
	}
	return string(data)
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("walk %s: %v", root, err)
	}
	return n
}

// One body, two addresses, one unit of capacity.
func TestPutServesBothDigestsOnce(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
	const body = "hello content store"

	c := putBody(t, s, body)
	want := hashOf(body)
	if c != want {
		t.Fatalf("digests = %+v, want %+v", c, want)
	}
	if got := readContent(t, s, SHA1, c.SHA1); got != body {
		t.Fatalf("sha1 body = %q", got)
	}
	if got := readContent(t, s, SHA256, c.SHA256); got != body {
		t.Fatalf("sha256 body = %q", got)
	}

	st := s.Stats()
	if st.Objects != 1 || st.Bytes != int64(len(body)) {
		t.Fatalf("stats = %+v, want one object of %d bytes", st, len(body))
	}
	// The same body cannot exist twice: a second write must not duplicate the
	// file or the accounting, and must not delete the body it replaces.
	putBody(t, s, body)
	if st := s.Stats(); st.Objects != 1 || st.Bytes != int64(len(body)) {
		t.Fatalf("after duplicate put stats = %+v", st)
	}
	if got := readContent(t, s, SHA256, c.SHA256); got != body {
		t.Fatalf("body after duplicate put = %q", got)
	}
	if n := countFiles(t, filepath.Join(dir, dataDirName)); n != 1 {
		t.Fatalf("data dir holds %d files, want 1", n)
	}
}

// PutExpected is the ordinary tee's content-addressed write: the digest check
// happens before publication, so a body that contradicts the expectation leaves
// no object, no accounting and no temp file behind.
func TestPutExpectedRefusesMismatchedContent(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
	const body = "expected content"
	want := hashOf(body)
	other := hashOf("some other content")

	for _, tc := range []struct {
		name   string
		algo   Algorithm
		digest string
	}{
		{"sha256", SHA256, other.SHA256},
		{"sha1", SHA1, other.SHA1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.PutExpected(context.Background(), strings.NewReader(body), int64(len(body)), tc.algo, tc.digest)
			if !errors.Is(err, ErrDigestMismatch) {
				t.Fatalf("mismatch error = %v, want ErrDigestMismatch", err)
			}
		})
	}
	if st := s.Stats(); st.Objects != 0 || st.Bytes != 0 {
		t.Fatalf("mismatched puts changed stats: %+v", st)
	}
	if _, _, err := s.Get(context.Background(), SHA256, want.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mismatched body was published: %v", err)
	}
	if n := countFiles(t, filepath.Join(dir, dataDirName)); n != 0 {
		t.Fatalf("data dir holds %d files, want 0", n)
	}
	if n := countFiles(t, filepath.Join(dir, tmpDirName)); n != 0 {
		t.Fatalf("temp dir holds %d files after a mismatch, want 0", n)
	}
}

// A matching expectation publishes exactly like Put, for either algorithm.
func TestPutExpectedPublishesMatchingContent(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
	const body = "expected content"
	want := hashOf(body)

	for _, tc := range []struct {
		name   string
		algo   Algorithm
		digest string
	}{
		{"sha256", SHA256, want.SHA256},
		{"sha1", SHA1, want.SHA1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := s.PutExpected(context.Background(), strings.NewReader(body), int64(len(body)), tc.algo, tc.digest)
			if err != nil {
				t.Fatalf("put: %v", err)
			}
			if c != want {
				t.Fatalf("digests = %+v, want %+v", c, want)
			}
			if got := readContent(t, s, SHA256, want.SHA256); got != body {
				t.Fatalf("stored body = %q", got)
			}
		})
	}
}

// A mismatched expectation never disturbs a body that already exists at its
// real address.
func TestPutExpectedMismatchLeavesExistingBodyUntouched(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
	const stored = "already stored"
	const incoming = "different bytes"
	existing := putBody(t, s, stored)
	before := s.Stats()

	_, err := s.PutExpected(context.Background(), strings.NewReader(incoming), int64(len(incoming)), SHA256, existing.SHA256)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("mismatch error = %v, want ErrDigestMismatch", err)
	}
	if got := readContent(t, s, SHA256, existing.SHA256); got != stored {
		t.Fatalf("existing body = %q", got)
	}
	if after := s.Stats(); after != before {
		t.Fatalf("stats changed: %+v -> %+v", before, after)
	}
}

// Strict LRU in logical bytes: reading A protects it, so C displaces B.
func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 100, MaxSize: 100})
	a := putBody(t, s, strings.Repeat("a", 40))
	b := putBody(t, s, strings.Repeat("b", 40))

	readContent(t, s, SHA256, a.SHA256)

	c := putBody(t, s, strings.Repeat("c", 40))

	if _, _, err := s.Get(context.Background(), SHA256, b.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B should have been evicted, got %v", err)
	}
	if got := readContent(t, s, SHA1, a.SHA1); got != strings.Repeat("a", 40) {
		t.Fatalf("A body = %q", got)
	}
	if got := readContent(t, s, SHA256, c.SHA256); got != strings.Repeat("c", 40) {
		t.Fatalf("C body = %q", got)
	}
	if st := s.Stats(); st.Bytes > st.Capacity || st.Objects != 2 {
		t.Fatalf("stats = %+v", st)
	}

	// Eviction removes the body, not just the accounting.
	be := newLocalBackend(filepath.Join(dir, dataDirName))
	evicted, err := be.path(b.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(evicted); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("evicted body still on disk: %v", err)
	}
}

// Oversized bodies are skipped and never evict a resident object.
func TestOversizedBodyIsNotStored(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 100, MaxSize: 50})
	a := putBody(t, s, strings.Repeat("a", 40))

	big := strings.Repeat("x", 60)
	if _, err := s.Put(context.Background(), strings.NewReader(big), int64(len(big))); !errors.Is(err, ErrOversized) {
		t.Fatalf("known size: got %v, want ErrOversized", err)
	}
	if _, err := s.Put(context.Background(), strings.NewReader(big), -1); !errors.Is(err, ErrOversized) {
		t.Fatalf("unknown size: got %v, want ErrOversized", err)
	}

	if st := s.Stats(); st.Objects != 1 || st.Bytes != 40 {
		t.Fatalf("oversized put changed accounting: %+v", st)
	}
	if got := readContent(t, s, SHA256, a.SHA256); got != strings.Repeat("a", 40) {
		t.Fatalf("resident body lost: %q", got)
	}
	if n := countFiles(t, s.tmpDir); n != 0 {
		t.Fatalf("%d temp files left behind", n)
	}
}

func TestEmptyBodyIsStored(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 100})
	c := putBody(t, s, "")
	if got := readContent(t, s, SHA256, c.SHA256); got != "" {
		t.Fatalf("body = %q", got)
	}
	if st := s.Stats(); st.Objects != 1 || st.Bytes != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// A restart must restore accounting, both aliases and the LRU order.
func TestRestartRestoresIndexAndOrder(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir, Capacity: 100, MaxSize: 100}

	s := openLocal(t, cfg)
	a := putBody(t, s, strings.Repeat("a", 40))
	b := putBody(t, s, strings.Repeat("b", 40))
	readContent(t, s, SHA1, a.SHA1) // A becomes MRU; B is now the tail
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := openLocal(t, cfg)
	if st := reopened.Stats(); st.Objects != 2 || st.Bytes != 80 {
		t.Fatalf("restored stats = %+v", st)
	}
	// The restored order must still say A is MRU and B is the tail: pushing
	// past capacity evicts B, not A.
	c := putBody(t, reopened, strings.Repeat("c", 40))
	if got := readContent(t, reopened, SHA256, c.SHA256); got != strings.Repeat("c", 40) {
		t.Fatalf("C body = %q", got)
	}
	if got := readContent(t, reopened, SHA256, a.SHA256); got != strings.Repeat("a", 40) {
		t.Fatalf("A body after restart = %q", got)
	}
	if _, _, err := reopened.Get(context.Background(), SHA256, b.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B should have been evicted first, got %v", err)
	}
}

// Reading a body whose file vanished is a MISS, and the stale index entry goes.
func TestMissingBodyIsMissAndDropped(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1000})
	c := putBody(t, s, "vanishing")

	be := newLocalBackend(filepath.Join(dir, dataDirName))
	p, err := be.path(c.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Get(context.Background(), SHA256, c.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get: got %v, want ErrNotFound", err)
	}
	if st := s.Stats(); st.Objects != 0 {
		t.Fatalf("stats = %+v, want the entry dropped", st)
	}
	if _, ok, _ := s.index.get(c.SHA256); ok {
		t.Fatal("index entry survived the missing body")
	}
}

// Reconcile drops records whose file is gone and removes orphan files (a crash
// between publishing a body and writing its index entry).
func TestReconcileRepairsLocalState(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 100, MaxSize: 100})
	c := putBody(t, s, strings.Repeat("a", 40))

	be := newLocalBackend(filepath.Join(dir, dataDirName))
	gone, err := be.path(c.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	orphanDigest := strings.Repeat("ab", 32)
	orphanPath, err := be.path(orphanDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(orphanPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if st := s.Stats(); st.Objects != 0 || st.Bytes != 0 {
		t.Fatalf("stats after reconcile = %+v", st)
	}
	if _, err := os.Stat(orphanPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("orphan file still present: %v", err)
	}
}

func TestDeleteByEitherDigest(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1000})
	c := putBody(t, s, "delete me")

	if err := s.Delete(context.Background(), SHA1, c.SHA1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, addr := range []struct {
		algo   Algorithm
		digest string
	}{{SHA1, c.SHA1}, {SHA256, c.SHA256}} {
		if _, _, err := s.Get(context.Background(), addr.algo, addr.digest); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: got %v, want ErrNotFound", addr.algo, err)
		}
	}
	if err := s.Delete(context.Background(), SHA1, c.SHA1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: got %v", err)
	}
}

func TestInvalidDigestRejected(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 100})
	for _, tc := range []struct {
		algo   Algorithm
		digest string
	}{
		{SHA1, "abc"},
		{SHA1, strings.Repeat("z", 40)},
		{SHA256, strings.Repeat("a", 40)},
		{Algorithm("md5"), strings.Repeat("a", 32)},
	} {
		if _, _, err := s.Get(context.Background(), tc.algo, tc.digest); !errors.Is(err, ErrInvalidDigest) {
			t.Errorf("%s/%s: got %v, want ErrInvalidDigest", tc.algo, tc.digest, err)
		}
	}
}

// Concurrent writers and readers must stay consistent under -race, and every
// successful PUT must be readable until it is evicted.
func TestConcurrentPutGetDelete(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 16, MaxSize: 1 << 12})
	stable := putBody(t, s, strings.Repeat("s", 100))

	ctx := context.Background()

	// A reconcile pass running while writes are in flight must not delete a body
	// that is already published but not yet indexed.
	stopReconcile := make(chan struct{})
	reconcileDone := make(chan struct{})
	go func() {
		defer close(reconcileDone)
		for {
			select {
			case <-stopReconcile:
				return
			default:
				if err := s.Reconcile(ctx); err != nil {
					t.Errorf("reconcile: %v", err)
					return
				}
			}
		}
	}()

	var workers sync.WaitGroup
	for w := 0; w < 8; w++ {
		workers.Add(1)
		go func(w int) {
			defer workers.Done()
			for i := 0; i < 20; i++ {
				body := fmt.Sprintf("worker-%d-body-%d", w, i)
				c, err := s.Put(ctx, strings.NewReader(body), -1)
				if err != nil {
					t.Errorf("put: %v", err)
					return
				}
				rc, _, err := s.Get(ctx, SHA256, c.SHA256)
				if err != nil {
					t.Errorf("get just-written body: %v", err)
					continue
				}
				_, _ = io.Copy(io.Discard, rc)
				_ = rc.Close()
				_, _, _ = s.Stat(ctx, SHA1, stable.SHA1)
				if i%5 == 0 {
					_ = s.Delete(ctx, SHA256, c.SHA256)
				}
			}
		}(w)
	}
	workers.Wait()
	close(stopReconcile)
	<-reconcileDone

	if st := s.Stats(); st.Bytes > st.Capacity {
		t.Fatalf("over capacity after concurrent writes: %+v", st)
	}
	if got := readContent(t, s, SHA1, stable.SHA1); got != strings.Repeat("s", 100) {
		t.Fatalf("stable body = %q", got)
	}
}

// CACHE_MAX_SIZE, not CACHE_CAPACITY, is the LRU budget. With the default 10%
// budget a 1000-byte capacity stores 100 bytes, so three 40-byte bodies evict
// the oldest one.
func TestDefaultBudgetIsTenPercentOfCapacity(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1000})
	if st := s.Stats(); st.Capacity != 100 {
		t.Fatalf("LRU budget = %d, want 100 (10%% of 1000)", st.Capacity)
	}

	a := putBody(t, s, strings.Repeat("a", 40))
	b := putBody(t, s, strings.Repeat("b", 40))
	c := putBody(t, s, strings.Repeat("c", 40))

	if _, _, err := s.Get(context.Background(), SHA256, a.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest body should be evicted, got %v", err)
	}
	if got := readContent(t, s, SHA256, b.SHA256); got != strings.Repeat("b", 40) {
		t.Fatalf("B body = %q", got)
	}
	if got := readContent(t, s, SHA256, c.SHA256); got != strings.Repeat("c", 40) {
		t.Fatalf("C body = %q", got)
	}
	if st := s.Stats(); st.Objects != 2 || st.Bytes != 80 || st.Capacity != 100 {
		t.Fatalf("stats = %+v", st)
	}
}

// An unreadable body is not a cache miss: the index entry must survive so a
// transient error cannot silently empty the cache.
func TestLocalUnreadableBodyKeepsIndex(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1000})
	c := putBody(t, s, "keep me")

	be := newLocalBackend(filepath.Join(dir, dataDirName))
	p, err := be.path(c.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o750); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Get(context.Background(), SHA256, c.SHA256); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("get: got %v, want a non-miss error", err)
	}
	if _, _, err := s.Stat(context.Background(), SHA256, c.SHA256); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("stat: got %v, want a non-miss error", err)
	}
	if st := s.Stats(); st.Objects != 1 {
		t.Fatalf("index entry was dropped on a transient error: %+v", st)
	}
	if _, ok, _ := s.index.get(c.SHA256); !ok {
		t.Fatal("durable index entry was dropped")
	}
}

// The publish guard is a refcount: a concurrent PUT of the same digest must not
// clear the protection early.
func TestPublishingRefCount(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1000})

	s.beginPublish("digest")
	s.beginPublish("digest")
	s.mu.Lock()
	count := s.publishing["digest"]
	s.mu.Unlock()
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	s.endPublish("digest")
	s.mu.Lock()
	count = s.publishing["digest"]
	s.mu.Unlock()
	if count != 1 {
		t.Fatalf("count after one end = %d, want 1", count)
	}

	s.endPublish("digest")
	s.mu.Lock()
	_, ok := s.publishing["digest"]
	s.mu.Unlock()
	if ok {
		t.Fatal("publish guard cleared too late")
	}
}

// MaxInt64 is a legal configuration; the oversized probe must not wrap to 0 and
// read every body as empty.
func TestMaxInt64BudgetDoesNotOverflow(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: math.MaxInt64, MaxSize: math.MaxInt64})
	const body = "not empty"
	c := putBody(t, s, body)

	if got := readContent(t, s, SHA256, c.SHA256); got != body {
		t.Fatalf("body = %q", got)
	}
	if st := s.Stats(); st.Bytes != int64(len(body)) || st.Capacity != math.MaxInt64 {
		t.Fatalf("stats = %+v", st)
	}
}

// A second opener must not touch .tmp before it holds the bbolt lock. It used to
// wipe the running relay's in-flight downloads and then fail to open the index.
func TestOpenLockPrecedesTempCleanup(t *testing.T) {
	prev := indexLockTimeout
	indexLockTimeout = 100 * time.Millisecond
	t.Cleanup(func() { indexLockTimeout = prev })

	dir := t.TempDir()
	cfg := Config{Dir: dir, Capacity: 1 << 20}

	first, err := Open(cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	first.wg.Wait()

	inFlight := filepath.Join(dir, tmpDirName, "in-flight")
	if err := os.WriteFile(inFlight, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	openers := map[string]func(Config) (*Store, error){
		"OpenOffline": OpenOffline,
		"Open":        Open,
	}
	for name, open := range openers {
		if s, err := open(cfg); err == nil {
			_ = s.Close()
			t.Errorf("%s: opened a directory that is still locked", name)
		}
		if _, err := os.Stat(inFlight); err != nil {
			t.Errorf("%s: removed .tmp before taking the lock: %v", name, err)
		}
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	second, err := OpenOffline(cfg)
	if err != nil {
		t.Fatalf("open after unlock: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if _, err := os.Stat(inFlight); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf(".tmp file survived a successful open: %v", err)
	}
}
