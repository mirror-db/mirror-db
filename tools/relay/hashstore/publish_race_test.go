package hashstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// A publish whose body is already in place must survive an LRU eviction of the
// same digest: the evictor may not delete bytes the in-flight commit is about to
// index.
func TestEvictionDoesNotDeleteInFlightPublish(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 100, MaxSize: 100})

	body := strings.Repeat("x", 60)
	x := putBody(t, s, body) // X is indexed and is the only LRU entry

	parked := make(chan struct{})
	releaseCh := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseCh) })
	// Never leave a publisher parked: Close waits for it, so a failing
	// assertion would otherwise hang instead of reporting.
	t.Cleanup(release)
	var calls atomic.Int32
	s.testAfterPublish = func() {
		// Park the first publisher only: that is Adopt(X). The Put below also
		// passes this hook and must not block.
		if calls.Add(1) == 1 {
			close(parked)
			<-releaseCh
		}
	}

	src := writeTempBody(t, s, body)
	adoptErr := make(chan error, 1)
	go func() {
		_, err := s.Adopt(context.Background(), src, int64(len(body)), SHA256, x.SHA256)
		adoptErr <- err
	}()

	<-parked // Adopt has renamed the body into place and is waiting to commit.

	be := newLocalBackend(filepath.Join(dir, dataDirName))
	dataPath, err := be.path(x.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("body missing right after publish: %v", err)
	}

	// Y's commit takes s.mu and evicts X to make room. X is publishing, so the
	// eviction must skip it instead of deleting the just-renamed body.
	y := strings.Repeat("y", 60)
	if _, err := s.Put(context.Background(), strings.NewReader(y), int64(len(y))); err != nil {
		t.Fatalf("put while X is publishing: %v", err)
	}
	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("eviction deleted the in-flight body: %v", err)
	}
	if _, ok, err := s.index.get(x.SHA256); err != nil || !ok {
		t.Fatalf("index record dropped during publish: ok=%v err=%v", ok, err)
	}

	// Reconcile must not treat the skipped record as an orphan either.
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("reconcile removed the in-flight body: %v", err)
	}

	release()
	if err := <-adoptErr; err != nil {
		t.Fatalf("adopt: %v", err)
	}
	s.testAfterPublish = nil

	// The publish's commit re-inserted the record; its body is intact.
	if got := readContent(t, s, SHA256, x.SHA256); got != body {
		t.Fatalf("sha256 body = %q", got)
	}
	if got := readContent(t, s, SHA1, x.SHA1); got != body {
		t.Fatalf("sha1 body = %q", got)
	}
	if st := s.Stats(); st.Objects != 1 || st.Bytes != int64(len(body)) {
		t.Fatalf("stats = %+v", st)
	}
	if n := countFiles(t, filepath.Join(dir, dataDirName)); n != 1 {
		t.Fatalf("body tree holds %d files, want 1", n)
	}
}

// The failed publisher must not delete a body a peer already committed.
func TestSharedRollbackKeepsCommittedPeer(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	body := strings.Repeat("p", 200)
	srcA := writeTempBody(t, s, body)
	srcB := writeTempBody(t, s, body)

	parked := make(chan struct{})
	releaseCh := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseCh) })
	t.Cleanup(release)
	var calls atomic.Int32
	s.testAfterPublish = func() {
		if calls.Add(1) == 1 {
			close(parked)
			<-releaseCh
		}
	}

	aErr := make(chan error, 1)
	go func() {
		_, err := s.Adopt(context.Background(), srcA, int64(len(body)), "", "")
		aErr <- err
	}()
	<-parked // A has uploaded; it has not committed.

	// B uploads and commits the same digest while A is parked.
	if _, err := s.Adopt(context.Background(), srcB, int64(len(body)), "", ""); err != nil {
		t.Fatalf("peer adopt: %v", err)
	}
	c := hashOf(body)
	if _, ok, err := s.index.get(c.SHA256); err != nil || !ok {
		t.Fatalf("peer commit not indexed: ok=%v err=%v", ok, err)
	}

	// A's commit now fails; its rollback must see B's record and keep the body.
	s.testCommitError = func() error { return errors.New("injected index failure") }
	release()
	if err := <-aErr; err == nil {
		t.Fatal("A's commit was supposed to fail")
	}
	s.testAfterPublish = nil
	s.testCommitError = nil

	for _, key := range []string{
		s3BodyKey(c.SHA256),
		s3SHA1AliasKey(c.SHA1),
		s3SHA256AliasKey(c.SHA256),
	} {
		if _, ok := fake.get(key); !ok {
			t.Fatalf("%s was deleted by a failed publisher", key)
		}
	}
	if _, ok, err := s.index.get(c.SHA256); err != nil || !ok {
		t.Fatalf("index record lost: ok=%v err=%v", ok, err)
	}
	if got := readContent(t, s, SHA256, c.SHA256); got != body {
		t.Fatalf("body = %q", got)
	}
	if st := s.Stats(); st.Objects != 1 || st.Bytes != int64(len(body)) {
		t.Fatalf("stats = %+v", st)
	}
}

// The other interleaving: the peer has uploaded but has not committed yet. A
// failed publisher must yield to the in-flight publish rather than delete keys
// that peer is about to index.
func TestSharedRollbackYieldsToInFlightPublisher(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	body := strings.Repeat("q", 200)
	srcA := writeTempBody(t, s, body)
	srcB := writeTempBody(t, s, body)

	aParked := make(chan struct{})
	bParked := make(chan struct{})
	releaseACh := make(chan struct{})
	releaseBCh := make(chan struct{})
	releaseA := sync.OnceFunc(func() { close(releaseACh) })
	releaseB := sync.OnceFunc(func() { close(releaseBCh) })
	t.Cleanup(releaseB)
	t.Cleanup(releaseA)
	var calls atomic.Int32
	s.testAfterPublish = func() {
		switch calls.Add(1) {
		case 1:
			close(aParked)
			<-releaseACh
		case 2:
			close(bParked)
			<-releaseBCh
		}
	}

	aErr := make(chan error, 1)
	go func() {
		_, err := s.Adopt(context.Background(), srcA, int64(len(body)), "", "")
		aErr <- err
	}()
	<-aParked

	bErr := make(chan error, 1)
	go func() {
		_, err := s.Adopt(context.Background(), srcB, int64(len(body)), "", "")
		bErr <- err
	}()
	<-bParked // B has uploaded; neither publisher has committed.

	// A fails while B is still between upload and commit: no index record yet,
	// only the publish guard says the body is owned.
	s.testCommitError = func() error { return errors.New("injected index failure") }
	releaseA()
	if err := <-aErr; err == nil {
		t.Fatal("A's commit was supposed to fail")
	}

	c := hashOf(body)
	for _, key := range []string{
		s3BodyKey(c.SHA256),
		s3SHA1AliasKey(c.SHA1),
		s3SHA256AliasKey(c.SHA256),
	} {
		if _, ok := fake.get(key); !ok {
			t.Fatalf("%s was deleted while a peer still owned it", key)
		}
	}

	// B now commits; the body it uploaded is still there.
	s.testCommitError = nil
	releaseB()
	if err := <-bErr; err != nil {
		t.Fatalf("peer adopt: %v", err)
	}
	s.testAfterPublish = nil

	if _, ok, err := s.index.get(c.SHA256); err != nil || !ok {
		t.Fatalf("index record missing after peer commit: ok=%v err=%v", ok, err)
	}
	if got := readContent(t, s, SHA1, c.SHA1); got != body {
		t.Fatalf("body = %q", got)
	}
	if st := s.Stats(); st.Objects != 1 || st.Bytes != int64(len(body)) {
		t.Fatalf("stats = %+v", st)
	}
}

// A lone failed publisher still cleans its bucket objects: the guard must not
// turn rollback cleanup off.
func TestSharedRollbackStillCleansUpAlone(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	const body = "failed publish cleanup"
	src := writeTempBody(t, s, body)

	s.testCommitError = func() error { return errors.New("injected index failure") }
	if _, err := s.Adopt(context.Background(), src, int64(len(body)), "", ""); err == nil {
		t.Fatal("expected the commit to fail")
	}
	s.testCommitError = nil

	if n := fake.lenObjects(); n != 0 {
		t.Fatalf("bucket leaked %d objects", n)
	}
	if st := s.Stats(); st.Objects != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if _, ok, err := s.index.get(hashOf(body).SHA256); err != nil || ok {
		t.Fatalf("index record survived a failed commit: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source was removed: %v", err)
	}
}
