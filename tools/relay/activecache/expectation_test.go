package activecache

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

const urlKey = "mirror://test/blob/thing"

// TestExpectationOnURLKeyPublishes checks the response-side shape: a URL-keyed
// entry with a separately declared digest expectation still publishes the real
// body under its real digest, and the key stays the URL.
func TestExpectationOnURLKeyPublishes(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	data := bytes.Repeat([]byte("expect"), 8*1024)

	m := newTestManager(t, store, dir, nil)
	ctx := context.Background()
	entry, created, err := m.Acquire(ctx, urlKey)
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if got := entry.Key(); got != urlKey {
		t.Fatalf("key = %q, want %q", got, urlKey)
	}
	if err := entry.SetExpectation(hashstore.SHA256, sha256Hex(data)); err != nil {
		t.Fatalf("set expectation: %v", err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := readAll(t, mustRead(t, entry)); !bytes.Equal(got, data) {
		t.Fatalf("body mismatch")
	}

	content := waitPublished(t, entry)
	if content.SHA256 != sha256Hex(data) {
		t.Fatalf("published sha256 = %q", content.SHA256)
	}
	if _, ok, err := store.Stat(ctx, hashstore.SHA256, content.SHA256); err != nil || !ok {
		t.Fatalf("hash store stat: ok=%v err=%v", ok, err)
	}
	entry.Release()
	waitFor(t, "scratch cleanup", func() bool { return len(scratchFiles(t, dir)) == 0 })
}

// TestExpectationMismatchPublishesNothing is the safety contract: a client body
// that is already streaming is unaffected, but nothing is published, so no
// variant can ever name the wrong bytes.
func TestExpectationMismatchPublishesNothing(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	data := bytes.Repeat([]byte("real"), 8*1024)
	wrong := sha256Hex([]byte("other"))

	m := newTestManager(t, store, dir, nil)
	ctx := context.Background()
	entry, created, err := m.Acquire(ctx, urlKey)
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	if err := entry.SetExpectation(hashstore.SHA256, wrong); err != nil {
		t.Fatalf("set expectation: %v", err)
	}
	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := readAll(t, mustRead(t, entry)); !bytes.Equal(got, data) {
		t.Fatalf("client body was damaged by a mismatch")
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
	entry.Release()
}

func TestExpectationRejectsLateAndInvalidValues(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	data := []byte("late")

	m := newTestManager(t, store, dir, nil)
	ctx := context.Background()

	t.Run("invalid digest", func(t *testing.T) {
		entry, _, err := m.Acquire(ctx, "mirror://test/invalid")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer entry.Release()
		if err := entry.SetExpectation(hashstore.SHA256, "abc"); !errors.Is(err, hashstore.ErrInvalidDigest) {
			t.Fatalf("SetExpectation(short) = %v, want ErrInvalidDigest", err)
		}
		if err := entry.SetExpectation("sha512", sha256Hex(data)); err == nil {
			t.Fatal("SetExpectation(unknown algorithm) accepted")
		}
		// 40 hex is a valid SHA-1 length, so an unvalidated algorithm name would
		// wrongly accept it. Only SHA-1 and SHA-256 exist.
		if err := entry.SetExpectation("sha512", sha1Hex(data)); !errors.Is(err, hashstore.ErrInvalidDigest) {
			t.Fatalf("SetExpectation(sha512, 40 hex) = %v, want ErrInvalidDigest", err)
		}
		if err := entry.SetExpectation("SHA256", sha256Hex(data)); err == nil {
			t.Fatal("SetExpectation(uppercase algorithm) accepted")
		}
		entry.Fail(nil)
	})

	t.Run("after init", func(t *testing.T) {
		entry, _, err := m.Acquire(ctx, "mirror://test/late")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer entry.Release()
		if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
			t.Fatalf("init: %v", err)
		}
		if err := entry.SetExpectation(hashstore.SHA256, sha256Hex(data)); !errors.Is(err, ErrAlreadyInitialized) {
			t.Fatalf("SetExpectation after Init = %v, want ErrAlreadyInitialized", err)
		}
		readAll(t, mustRead(t, entry))
		waitPublished(t, entry)
	})
}

// TestExpectationNeverOverridesDigestKey protects policy precedence: a digest
// derived from the key is the pre-request fact, so a later statement can only
// restate it.
func TestExpectationNeverOverridesDigestKey(t *testing.T) {
	store, dir := openTestStore(t, 64<<20)
	data := []byte("keyed")
	key := "sha256:" + sha256Hex(data)

	m := newTestManager(t, store, dir, nil)
	ctx := context.Background()
	entry, created, err := m.Acquire(ctx, key)
	if err != nil || !created {
		t.Fatalf("acquire: created=%v err=%v", created, err)
	}
	defer entry.Release()

	if err := entry.SetExpectation(hashstore.SHA256, sha256Hex(data)); err != nil {
		t.Fatalf("restating the key digest: %v", err)
	}
	if err := entry.SetExpectation(hashstore.SHA256, sha256Hex([]byte("other"))); !errors.Is(err, ErrExpectationConflict) {
		t.Fatalf("overriding the key digest = %v, want ErrExpectationConflict", err)
	}
	// A sha1 statement cannot replace the sha256 the key already fixes.
	if err := entry.SetExpectation(hashstore.SHA1, sha1Hex(data)); !errors.Is(err, ErrExpectationConflict) {
		t.Fatalf("cross-algorithm override = %v, want ErrExpectationConflict", err)
	}

	if err := entry.Init(&testSource{data: data}, nil, int64(len(data))); err != nil {
		t.Fatalf("init: %v", err)
	}
	readAll(t, mustRead(t, entry))
	content := waitPublished(t, entry)
	if content.SHA256 != sha256Hex(data) {
		t.Fatalf("published sha256 = %q", content.SHA256)
	}
}
