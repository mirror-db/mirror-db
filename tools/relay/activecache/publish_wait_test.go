package activecache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// WaitPublished is the handshake the HTTP layer uses after delivering a full
// body: it must report the published content, not a stale nil.
func TestWaitPublishedReportsContent(t *testing.T) {
	store, dir := openTestStore(t, 1<<20)
	m := newTestManager(t, store, dir, nil)

	data := []byte("a complete immutable object")
	entry, created, err := m.Acquire(context.Background(), digestKey(data))
	if err != nil || !created {
		t.Fatalf("Acquire = created %v, err %v", created, err)
	}
	if err := entry.Init(&testSource{data: data}, "meta", int64(len(data))); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rc, err := entry.Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	readAll(t, rc)

	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	content, err := entry.WaitPublished(ctx)
	if err != nil {
		t.Fatalf("WaitPublished: %v", err)
	}
	if content.SHA256 != sha256Hex(data) || content.Size != int64(len(data)) {
		t.Fatalf("content = %+v", content)
	}
	entry.Release()
}

// A digest mismatch must surface as an error, not as a publication.
func TestWaitPublishedReportsAdoptFailure(t *testing.T) {
	store, dir := openTestStore(t, 1<<20)
	m := newTestManager(t, store, dir, nil)

	data := []byte("the body the upstream sent")
	entry, created, err := m.Acquire(context.Background(), digestKey([]byte("a different body")))
	if err != nil || !created {
		t.Fatalf("Acquire = created %v, err %v", created, err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rc, err := entry.Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	readAll(t, rc)

	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if _, err := entry.WaitPublished(ctx); !errors.Is(err, hashstore.ErrDigestMismatch) {
		t.Fatalf("WaitPublished = %v, want ErrDigestMismatch", err)
	}
	entry.Release()
}

// Close settles every waiter instead of leaving it blocked on a body that can
// never be published.
func TestWaitPublishedUnblocksOnClose(t *testing.T) {
	store, dir := openTestStore(t, 1<<20)
	m := newTestManager(t, store, dir, nil)

	data := []byte("never completed")
	entry, created, err := m.Acquire(context.Background(), "opaque-key")
	if err != nil || !created {
		t.Fatalf("Acquire = created %v, err %v", created, err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("Init: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := entry.WaitPublished(ctx)
		done <- err
	}()

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitPublished after Close = %v", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("WaitPublished did not unblock on Close")
	}
}
