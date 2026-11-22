package hashstore

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
	bolt "go.etcd.io/bbolt"
)

// pruneLocal opens the store offline, so nothing repairs the directory before the
// scan and the counts stay meaningful.
func pruneLocal(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := OpenOffline(Config{Dir: dir, Capacity: 1 << 20})
	if err != nil {
		t.Fatalf("open offline: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

func writeBodyFile(t *testing.T, dir, sha256, body string) string {
	t.Helper()
	be := newLocalBackend(filepath.Join(dir, dataDirName))
	p, err := be.path(sha256)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPruneLocalRemovesOrphansAndStale(t *testing.T) {
	dir := t.TempDir()
	seed, err := Open(Config{Dir: dir, Capacity: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = seed.Close() })
	a := putBody(t, seed, "indexed body")
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	// Written after the only store closed: an orphan until prune looks.
	orphan := hashOf("orphan").SHA256
	orphanPath := writeBodyFile(t, dir, orphan, "orphan")

	s := pruneLocal(t, dir)
	stats, err := s.Prune(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if want := (PruneStats{Scanned: 2, Kept: 1, Deleted: 1}); stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if _, err := os.Stat(orphanPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("orphan still on disk: %v", err)
	}
	if got := readContent(t, s, SHA256, a.SHA256); got != "indexed body" {
		t.Fatalf("indexed body = %q", got)
	}

	// A body that vanished leaves a stale index entry; prune drops it.
	be := newLocalBackend(filepath.Join(dir, dataDirName))
	indexedPath, err := be.path(a.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(indexedPath); err != nil {
		t.Fatal(err)
	}
	stats, err = s.Prune(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if want := (PruneStats{Stale: 1}); stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if got := s.Stats(); got.Objects != 0 || got.Bytes != 0 {
		t.Fatalf("stale entry survived: %+v", got)
	}
}

func TestPruneSharedRemovesOnlyUnindexed(t *testing.T) {
	fake := newFakeS3()
	fake.setPageSize(2) // force several LIST pages
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	a := putBody(t, s, "kept body")

	// A body another relay wrote: complete, but not in this index.
	other := hashOf("other relay body")
	fake.putRaw(s3BodyKey(other.SHA256), []byte("other relay body"))
	fake.putRaw(s3SHA1AliasKey(other.SHA1), []byte(other.SHA256))
	fake.putRaw(s3SHA256AliasKey(other.SHA256), []byte(other.SHA1))

	// A foreign key under our prefix, and another prefix entirely.
	fake.putRaw("relay/notes.txt", []byte("hand written"))
	otherPrefix := "relay-other/blobs/sha256/aa/bb/" + strings.Repeat("aa", 32)
	fake.putRaw(otherPrefix, []byte("another relay's prefix"))

	stats, err := s.Prune(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if want := (PruneStats{Scanned: 6, Kept: 3, Deleted: 3, Skipped: 1}); stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if got := readContent(t, s, SHA256, a.SHA256); got != "kept body" {
		t.Fatalf("indexed body = %q", got)
	}
	if st := s.Stats(); st.Objects != 1 {
		t.Fatalf("prune changed the local index: %+v", st)
	}

	for _, key := range []string{
		s3BodyKey(other.SHA256),
		s3SHA1AliasKey(other.SHA1),
		s3SHA256AliasKey(other.SHA256),
	} {
		if _, ok := fake.get(key); ok {
			t.Errorf("unindexed object %s survived", key)
		}
	}
	if _, ok := fake.get("relay/notes.txt"); !ok {
		t.Error("unknown key under the prefix was deleted")
	}
	if _, ok := fake.get(otherPrefix); !ok {
		t.Error("key from another prefix was deleted")
	}
}

func TestPruneSharedEmptyBucket(t *testing.T) {
	s := openShared(t, t.TempDir(), newFakeS3(), 1<<20, 0)
	stats, err := s.Prune(context.Background())
	if err != nil || stats != (PruneStats{}) {
		t.Fatalf("prune = (%+v,%v), want zero stats and no error", stats, err)
	}
}

func TestPruneSharedDeleteFailureContinues(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	putBody(t, s, "kept body")

	other := hashOf("half deleted")
	fake.putRaw(s3BodyKey(other.SHA256), []byte("half deleted"))
	fake.putRaw(s3SHA1AliasKey(other.SHA1), []byte(other.SHA256))
	fake.putRaw(s3SHA256AliasKey(other.SHA256), []byte(other.SHA1))
	fake.setFailDeleteKey(s3BodyKey(other.SHA256))

	stats, err := s.Prune(context.Background())
	if err == nil {
		t.Fatal("expected an error when a delete fails")
	}
	if want := (PruneStats{Scanned: 6, Kept: 3, Deleted: 2, Failed: 1}); stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	// The failing body remains; its aliases were still removed, and the indexed
	// body was untouched.
	if _, ok := fake.get(s3BodyKey(other.SHA256)); !ok {
		t.Error("failed delete removed the body anyway")
	}
	if _, ok := fake.get(s3SHA1AliasKey(other.SHA1)); ok {
		t.Error("pass stopped after the first failure")
	}
	if _, ok := fake.get(s3BodyKey(hashOf("kept body").SHA256)); !ok {
		t.Error("indexed body was deleted")
	}
}

func TestPruneSharedListFailure(t *testing.T) {
	fake := newFakeS3()
	fake.setListErr(errors.New("listing denied"))
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)

	stats, err := s.Prune(context.Background())
	if err == nil {
		t.Fatal("expected a listing error")
	}
	if stats.Failed != 1 || stats.Scanned != 0 || stats.Deleted != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestPruneCompletesPendingDeletes(t *testing.T) {
	fake := newFakeS3()
	fake.setFailDelete(true)
	s := openShared(t, t.TempDir(), fake, 100, 100)

	a := putBody(t, s, strings.Repeat("a", 60))
	putBody(t, s, strings.Repeat("b", 60)) // evicts A; the delete is queued
	fake.setFailDelete(false)

	stats, err := s.Prune(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if stats.Pending != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if _, ok := fake.get(s3BodyKey(a.SHA256)); ok {
		t.Fatal("pending delete was not completed")
	}
}

func TestPruneHonoursCancelledContext(t *testing.T) {
	dir := t.TempDir()
	orphanPath := writeBodyFile(t, dir, hashOf("orphan").SHA256, "orphan")
	s := pruneLocal(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats, err := s.Prune(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("prune: got %v, want context.Canceled", err)
	}
	if stats != (PruneStats{}) {
		t.Fatalf("stats = %+v, want zero", stats)
	}
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("cancelled prune still deleted: %v", err)
	}
}

func TestPruneAfterCloseIsClosed(t *testing.T) {
	s, err := OpenOffline(Config{Dir: t.TempDir(), Capacity: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("prune after close: got %v, want ErrClosed", err)
	}
}

func putAliasRow(t *testing.T, s *Store, sha1, primary string) {
	t.Helper()
	key, err := hex.DecodeString(sha1)
	if err != nil {
		t.Fatal(err)
	}
	value, err := hex.DecodeString(primary)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.index.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(aliasesBucket).Put(key, value)
	}); err != nil {
		t.Fatal(err)
	}
}

// A body that another writer removed must lose its local record and its remote
// pointers in the same pass: the index check runs before LIST.
func TestPruneDropsStaleIndexAndRemoteAliases(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	c := putBody(t, s, "vanished elsewhere")

	fake.deleteRaw(s3BodyKey(c.SHA256))

	stats, err := s.Prune(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if want := (PruneStats{Scanned: 2, Deleted: 2, Stale: 1}); stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if _, ok, _ := s.index.get(c.SHA256); ok {
		t.Error("object record survived a missing body")
	}
	if _, ok, _ := s.index.lookupSHA1(c.SHA1); ok {
		t.Error("bbolt alias row survived a missing body")
	}
	for _, key := range []string{s3SHA1AliasKey(c.SHA1), s3SHA256AliasKey(c.SHA256)} {
		if _, ok := fake.get(key); ok {
			t.Errorf("remote pointer %s survived", key)
		}
	}
}

// A stat error means "unknown", never "gone": the index and the shared objects
// stay, and the pass reports a failure.
func TestPruneKeepsIndexOnStatError(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	c := putBody(t, s, "denied stat")

	fake.setErrors(nil, &smithy.GenericAPIError{Code: "AccessDenied"})
	stats, err := s.Prune(context.Background())
	if err == nil {
		t.Fatal("expected an error when HEAD is denied")
	}
	if want := (PruneStats{Scanned: 3, Kept: 3, Failed: 1}); stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if _, ok, _ := s.index.get(c.SHA256); !ok {
		t.Error("index dropped on a denied stat")
	}
	if _, ok := fake.get(s3BodyKey(c.SHA256)); !ok {
		t.Error("remote body deleted on a denied stat")
	}
}

// Historical damage: an alias row whose primary record does not exist. The pass
// repairs the index and then the LIST sees the pointer as unindexed.
func TestPruneDropsDanglingAliasRow(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)

	sha1 := strings.Repeat("ab", 20)
	primary := strings.Repeat("cd", 32)
	putAliasRow(t, s, sha1, primary)
	fake.putRaw(s3SHA1AliasKey(sha1), []byte(primary))

	stats, err := s.Prune(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if want := (PruneStats{Scanned: 1, Deleted: 1, Stale: 1}); stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if _, ok, _ := s.index.lookupSHA1(sha1); ok {
		t.Error("dangling alias row survived")
	}
	if _, ok := fake.get(s3SHA1AliasKey(sha1)); ok {
		t.Error("remote dangling alias survived")
	}
}
