package hashstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// fakeS3 is the object store the backend talks to. It records every key, so the
// tests assert the body/pointer layout rather than trusting the backend's own
// view of it.
type fakeS3 struct {
	mu            sync.Mutex
	objects       map[string][]byte
	failDelete    bool
	failDeleteKey string
	failPutPrefix string
	getErr        error
	headErr       error
	listErr       error
	pageSize      int
	requested     []string
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPutPrefix != "" && strings.HasPrefix(*in.Key, f.failPutPrefix) {
		return nil, fmt.Errorf("put rejected: %s", *in.Key)
	}
	f.objects[*in.Key] = data
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requested = append(f.requested, *in.Key)
	if f.getErr != nil {
		return nil, f.getErr
	}
	data, ok := f.objects[*in.Key]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: *in.Key}
	}
	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(data)),
		ContentLength: aws.Int64(int64(len(data))),
	}, nil
}

func (f *fakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.headErr != nil {
		return nil, f.headErr
	}
	data, ok := f.objects[*in.Key]
	if !ok {
		// HEAD has no body and S3 reports the absence as NotFound, not NoSuchKey.
		return nil, &smithy.GenericAPIError{Code: "NotFound", Message: *in.Key}
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(data)))}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if aws.ToString(in.Key) == "" {
		// Real S3 rejects a delete with no key; an empty alias must never be
		// requested in the first place.
		return nil, errors.New("empty object key")
	}
	if f.failDelete || (f.failDeleteKey != "" && *in.Key == f.failDeleteKey) {
		return nil, errors.New("delete rejected")
	}
	delete(f.objects, *in.Key)
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}

	prefix := aws.ToString(in.Prefix)
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	// S3 continuation tokens resume after a key, not at an offset: deleting
	// objects while paging must not shift the remaining pages.
	start := 0
	if token := aws.ToString(in.ContinuationToken); token != "" {
		start = sort.SearchStrings(keys, token)
		if start < len(keys) && keys[start] == token {
			start++
		}
	}
	pageSize := f.pageSize
	if pageSize <= 0 {
		pageSize = 1000
	}
	end := min(start+pageSize, len(keys))

	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(end < len(keys))}
	for _, key := range keys[start:end] {
		out.Contents = append(out.Contents, types.Object{Key: aws.String(key)})
	}
	if end < len(keys) && end > start {
		out.NextContinuationToken = aws.String(keys[end-1])
	}
	return out, nil
}

func (f *fakeS3) get(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.objects[key]
	return string(v), ok
}

func (f *fakeS3) setFailDelete(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failDelete = v
}

func (f *fakeS3) setListErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listErr = err
}

func (f *fakeS3) setPageSize(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pageSize = n
}

func (f *fakeS3) setFailDeleteKey(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failDeleteKey = key
}

func (f *fakeS3) setErrors(getErr, headErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = getErr
	f.headErr = headErr
}

func (f *fakeS3) deleteRaw(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
}

func (f *fakeS3) putRaw(key string, value []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = value
}

func (f *fakeS3) requestedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requested...)
}

func s3BodyKey(digest string) string {
	return "relay/blobs/sha256/" + digest[0:2] + "/" + digest[2:4] + "/" + digest
}

func s3SHA1AliasKey(digest string) string {
	return "relay/aliases/sha1/" + digest[0:2] + "/" + digest[2:4] + "/" + digest
}

func s3SHA256AliasKey(digest string) string {
	return "relay/aliases/sha256/" + digest[0:2] + "/" + digest[2:4] + "/" + digest
}

func openShared(t *testing.T, dir string, fake *fakeS3, capacity, maxSize int64) *Store {
	t.Helper()
	cfg := Config{
		Dir:      dir,
		Capacity: capacity,
		MaxSize:  maxSize,
		S3:       &S3Config{Bucket: "bucket", Prefix: "relay", Region: "us-east-1"},
	}
	s, err := open(cfg, newS3Backend(fake, *cfg.S3))
	if err != nil {
		t.Fatalf("open shared: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	s.wg.Wait()
	return s
}

func TestSharedBodyAndPointersLayout(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeS3()
	s := openShared(t, dir, fake, 1<<20, 0)
	c := putBody(t, s, "shared body")

	bodyKey := s3BodyKey(c.SHA256)
	if _, ok := fake.get(bodyKey); !ok {
		t.Fatalf("body object %s missing", bodyKey)
	}
	if got, ok := fake.get(s3SHA1AliasKey(c.SHA1)); !ok || got != c.SHA256 {
		t.Fatalf("sha1 pointer = %q ok=%v, want %q", got, ok, c.SHA256)
	}
	if got, ok := fake.get(s3SHA256AliasKey(c.SHA256)); !ok || got != c.SHA1 {
		t.Fatalf("sha256 pointer = %q ok=%v, want %q", got, ok, c.SHA1)
	}

	// Local disk holds only the tmp dir and the index.
	if _, err := os.Stat(filepath.Join(dir, dataDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local data dir exists with an S3 backend: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, indexFileName)); err != nil {
		t.Fatalf("index missing: %v", err)
	}

	if got := readContent(t, s, SHA1, c.SHA1); got != "shared body" {
		t.Fatalf("sha1 body = %q", got)
	}
	if got := readContent(t, s, SHA256, c.SHA256); got != "shared body" {
		t.Fatalf("sha256 body = %q", got)
	}
}

// Another relay's object is adopted on a successful read, and an eviction in one
// instance removes the shared object for everyone (a MISS, never an error).
func TestSharedAdoptionAndCrossInstanceEviction(t *testing.T) {
	fake := newFakeS3()
	a := openShared(t, t.TempDir(), fake, 200, 200)
	c := putBody(t, a, strings.Repeat("a", 60))

	b := openShared(t, t.TempDir(), fake, 100, 100)
	if st := b.Stats(); st.Objects != 0 {
		t.Fatalf("fresh instance stats = %+v", st)
	}
	if got := readContent(t, b, SHA1, c.SHA1); got != strings.Repeat("a", 60) {
		t.Fatalf("adopted body = %q", got)
	}
	if st := b.Stats(); st.Objects != 1 || st.Bytes != 60 {
		t.Fatalf("adoption did not enter the index: %+v", st)
	}

	// Fill B: the adopted object is the LRU tail, so it is deleted from the
	// shared bucket even though A still has it indexed.
	newBody := strings.Repeat("b", 60)
	putBody(t, b, newBody)
	if _, ok := fake.get(s3BodyKey(c.SHA256)); ok {
		t.Fatal("evicted shared body still present")
	}
	if _, ok := fake.get(s3SHA1AliasKey(c.SHA1)); ok {
		t.Fatal("evicted shared pointer still present")
	}

	if _, _, err := a.Get(context.Background(), SHA256, c.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A saw %v, want ErrNotFound for the deleted shared object", err)
	}
	if st := a.Stats(); st.Objects != 0 {
		t.Fatalf("A kept a stale entry: %+v", st)
	}
	if got := readContent(t, b, SHA256, hashOf(newBody).SHA256); got != newBody {
		t.Fatalf("B body = %q", got)
	}
}

func TestSharedMissingIsMiss(t *testing.T) {
	s := openShared(t, t.TempDir(), newFakeS3(), 1<<20, 0)
	missing := strings.Repeat("0", 64)
	if _, _, err := s.Get(context.Background(), SHA256, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get: got %v, want ErrNotFound", err)
	}
	if _, _, err := s.Get(context.Background(), SHA1, strings.Repeat("0", 40)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get by sha1: got %v, want ErrNotFound", err)
	}
	if _, ok, err := s.Stat(context.Background(), SHA256, missing); err != nil || ok {
		t.Fatalf("stat = (%v,%v), want (false,nil)", ok, err)
	}
}

// A failed object delete is queued durably and retried by Reconcile.
func TestFailedEvictionIsQueuedAndRetried(t *testing.T) {
	fake := newFakeS3()
	fake.setFailDelete(true)
	s := openShared(t, t.TempDir(), fake, 100, 100)

	a := putBody(t, s, strings.Repeat("a", 60))
	putBody(t, s, strings.Repeat("b", 60)) // evicts A, whose delete fails

	pending, err := s.index.pendingDeletes()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].SHA256 != a.SHA256 {
		t.Fatalf("pending = %+v, want A only", pending)
	}
	if _, ok := fake.get(s3BodyKey(a.SHA256)); !ok {
		t.Fatal("body vanished despite the failed delete")
	}

	fake.setFailDelete(false)
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pending, _ := s.index.pendingDeletes(); len(pending) != 0 {
		t.Fatalf("pending after retry = %+v", pending)
	}
	if _, ok := fake.get(s3BodyKey(a.SHA256)); ok {
		t.Fatal("retry did not delete the body")
	}
}

// A failed pointer write must not leave an unreachable body in the bucket.
func TestSharedPutFailureDoesNotLeakBody(t *testing.T) {
	fake := newFakeS3()
	fake.failPutPrefix = "relay/aliases/sha1/"
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)

	const body = "rollback"
	if _, err := s.Put(context.Background(), strings.NewReader(body), int64(len(body))); err == nil {
		t.Fatal("expected the pointer write to fail")
	}
	c := hashOf(body)
	if _, ok := fake.get(s3BodyKey(c.SHA256)); ok {
		t.Fatal("body object leaked after a failed publish")
	}
	if st := s.Stats(); st.Objects != 0 {
		t.Fatalf("stats = %+v, want nothing indexed", st)
	}
}

func TestSharedDeleteRemovesPointers(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	c := putBody(t, s, "bye")

	if err := s.Delete(context.Background(), SHA256, c.SHA256); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, key := range []string{
		s3BodyKey(c.SHA256),
		s3SHA1AliasKey(c.SHA1),
		s3SHA256AliasKey(c.SHA256),
	} {
		if _, ok := fake.get(key); ok {
			t.Errorf("%s survived delete", key)
		}
	}
}

// A permission or transport error is not a miss: the index entry must survive.
func TestSharedTransientErrorKeepsIndex(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	c := putBody(t, s, "denied once")

	fake.setErrors(&smithy.GenericAPIError{Code: "AccessDenied"}, &smithy.GenericAPIError{Code: "AccessDenied"})
	if _, _, err := s.Get(context.Background(), SHA256, c.SHA256); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("get: got %v, want a non-miss error", err)
	}
	if _, _, err := s.Stat(context.Background(), SHA256, c.SHA256); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("stat: got %v, want a non-miss error", err)
	}
	if st := s.Stats(); st.Objects != 1 {
		t.Fatalf("index entry was dropped on an AccessDenied: %+v", st)
	}
	if _, ok, _ := s.index.get(c.SHA256); !ok {
		t.Fatal("durable index entry was dropped")
	}

	fake.setErrors(nil, nil)
	if got := readContent(t, s, SHA256, c.SHA256); got != "denied once" {
		t.Fatalf("body after recovery = %q", got)
	}
}

// A digest pointer is untrusted input from a shared bucket: an invalid value is
// a miss and must never be spliced into an object key.
func TestSharedAliasPointerCannotEscape(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)

	sha1 := strings.Repeat("ab", 20)
	fake.putRaw(s3SHA1AliasKey(sha1), []byte("../../secret"))

	if _, _, err := s.Get(context.Background(), SHA1, sha1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get: got %v, want ErrNotFound", err)
	}
	for _, key := range fake.requestedKeys() {
		if key != s3SHA1AliasKey(sha1) {
			t.Fatalf("read unexpected key %q from an invalid pointer", key)
		}
	}

	// The same rule applies to the SHA-256 pointer.
	sha256 := strings.Repeat("cd", 32)
	fake.putRaw(s3SHA256AliasKey(sha256), []byte("../../../etc/passwd"))
	if _, _, err := s.Get(context.Background(), SHA256, sha256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get by sha256: got %v, want ErrNotFound", err)
	}
}

// A queued delete must be cancelled by a re-PUT of the same body, or the next
// reconcile deletes content the index still serves.
func TestRePutClearsQueuedDelete(t *testing.T) {
	fake := newFakeS3()
	fake.setFailDelete(true)
	s := openShared(t, t.TempDir(), fake, 100, 100)

	body := strings.Repeat("a", 60)
	a := putBody(t, s, body)
	putBody(t, s, strings.Repeat("b", 60)) // evicts A; the delete is queued

	if pending, _ := s.index.pendingDeletes(); len(pending) != 1 {
		t.Fatalf("pending = %+v, want A", pending)
	}

	fake.setFailDelete(false)
	putBody(t, s, body) // re-publish A; must cancel the queued delete
	if pending, _ := s.index.pendingDeletes(); len(pending) != 0 {
		t.Fatalf("pending after re-PUT = %+v, want none", pending)
	}

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := readContent(t, s, SHA256, a.SHA256); got != body {
		t.Fatalf("body after reconcile = %q", got)
	}
	if _, ok, _ := s.index.get(a.SHA256); !ok {
		t.Fatal("index entry lost after reconcile")
	}
}

// blockingPutBackend holds a PUT inside the backend until released, so Close can
// be observed while an operation is in flight.
type blockingPutBackend struct {
	backend
	started chan struct{}
	release chan struct{}
}

func (b *blockingPutBackend) put(ctx context.Context, c Content, tmpPath string) error {
	close(b.started)
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.backend.put(ctx, c, tmpPath)
}

// Close must wait for the in-flight PUT, and that PUT must finish its index
// commit: a shared body published but never indexed would be unreachable.
func TestCloseWaitsForInFlightPut(t *testing.T) {
	fake := newFakeS3()
	dir := t.TempDir()
	cfg := Config{
		Dir:      dir,
		Capacity: 1 << 20,
		S3:       &S3Config{Bucket: "bucket", Prefix: "relay", Region: "us-east-1"},
	}
	b := &blockingPutBackend{
		backend: newS3Backend(fake, *cfg.S3),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	releaseOnce := sync.OnceFunc(func() { close(b.release) })
	defer releaseOnce()

	s, err := open(cfg, b)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.wg.Wait()

	const body = "blocked body"
	putDone := make(chan error, 1)
	go func() {
		_, err := s.Put(context.Background(), strings.NewReader(body), int64(len(body)))
		putDone <- err
	}()
	<-b.started

	closed := make(chan struct{})
	go func() {
		_ = s.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while a PUT was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	releaseOnce()
	if err := <-putDone; err != nil {
		t.Fatalf("put: %v", err)
	}
	<-closed
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	// The PUT committed before Close returned: body and index agree on disk.
	reopened, err := open(cfg, newS3Backend(fake, *cfg.S3))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got := readContent(t, reopened, SHA256, hashOf(body).SHA256); got != body {
		t.Fatalf("body after reopen = %q", got)
	}
}

// A configured prefix with redundant or parent segments must produce the same
// place for writes and for prune's listing.
func TestSharedPrefixCleanedForWritesAndListing(t *testing.T) {
	fake := newFakeS3()
	cfg := Config{
		Dir:      t.TempDir(),
		Capacity: 1 << 20,
		S3:       &S3Config{Bucket: "bucket", Prefix: "relay//x/", Region: "us-east-1"},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.S3.Prefix != "relay/x" {
		t.Fatalf("prefix = %q, want relay/x", cfg.S3.Prefix)
	}

	s, err := open(cfg, newS3Backend(fake, *cfg.S3))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.wg.Wait()

	c := putBody(t, s, "prefixed")
	sharded := func(base, digest string) string {
		return base + "/" + digest[0:2] + "/" + digest[2:4] + "/" + digest
	}
	keptKey := sharded("relay/x/blobs/sha256", c.SHA256)
	if _, ok := fake.get(keptKey); !ok {
		t.Fatalf("body written outside the cleaned prefix (want %s)", keptKey)
	}

	// An unindexed object under the same cleaned prefix must be listed by prune.
	other := hashOf("stale under prefix")
	staleKey := sharded("relay/x/blobs/sha256", other.SHA256)
	fake.putRaw(staleKey, []byte("stale under prefix"))

	stats, err := s.Prune(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	// The indexed body's two pointer objects are kept too.
	if want := (PruneStats{Scanned: 4, Kept: 3, Deleted: 1}); stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if _, ok := fake.get(staleKey); ok {
		t.Fatal("object under the cleaned prefix was not listed")
	}
	if _, ok := fake.get(keptKey); !ok {
		t.Fatal("indexed body under the cleaned prefix was deleted")
	}
}

// A queued delete without a SHA-1 alias must complete: an empty alias cannot
// become an empty object key.
func TestPruneCompletesPendingDeleteWithoutSHA1(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)

	other := hashOf("legacy pending")
	fake.putRaw(s3BodyKey(other.SHA256), []byte("legacy pending"))
	fake.putRaw(s3SHA256AliasKey(other.SHA256), []byte(other.SHA1))

	// A pending entry as an older/unknown writer could leave behind: no SHA-1.
	s.mu.Lock()
	err := s.index.removeAndQueue(objectMeta{SHA256: other.SHA256})
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("queue pending: %v", err)
	}

	stats, err := s.Prune(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if stats.Pending != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want the pending delete to complete", stats)
	}
	if _, ok := fake.get(s3BodyKey(other.SHA256)); ok {
		t.Error("body survived the pending delete")
	}
	if _, ok := fake.get(s3SHA256AliasKey(other.SHA256)); ok {
		t.Error("sha256 alias survived the pending delete")
	}
}
