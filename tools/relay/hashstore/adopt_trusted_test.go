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

	"github.com/mirror-db/mirror-db/tools/relay/diskspace"
)

// adoptTrustedBody writes body into the temp dir and publishes it with the
// digests the caller computed, like a trusted caller that hashed while writing.
func adoptTrustedBody(t *testing.T, s *Store, body string) Content {
	t.Helper()
	path := writeTempBody(t, s, body)
	c, err := s.AdoptTrusted(context.Background(), path, hashOf(body), "", "")
	if err != nil {
		t.Fatalf("adopt trusted %d-byte body: %v", len(body), err)
	}
	return c
}

func TestAdoptTrustedLocalPublishesBothDigests(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})

	const body = "trusted adopted body"
	path := writeTempBody(t, s, body)
	want := hashOf(body)
	// A trusted caller may present either hex case; the store normalises it.
	declared := Content{
		SHA1:   strings.ToUpper(want.SHA1),
		SHA256: strings.ToUpper(want.SHA256),
		Size:   int64(len(body)),
	}

	c, err := s.AdoptTrusted(context.Background(), path, declared, "", "")
	if err != nil {
		t.Fatalf("adopt trusted: %v", err)
	}
	if c != want {
		t.Fatalf("content = %+v, want %+v", c, want)
	}
	if got := readContent(t, s, SHA1, c.SHA1); got != body {
		t.Fatalf("sha1 body = %q", got)
	}
	if got := readContent(t, s, SHA256, c.SHA256); got != body {
		t.Fatalf("sha256 body = %q", got)
	}
	if st := s.Stats(); st.Objects != 1 || st.Bytes != int64(len(body)) {
		t.Fatalf("stats = %+v", st)
	}
	// A local backend renames the source into the body tree.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source survived a local adopt: %v", err)
	}
	if n := countFiles(t, filepath.Join(dir, dataDirName)); n != 1 {
		t.Fatalf("body tree holds %d files, want 1", n)
	}
}

func TestAdoptTrustedRejectsInvalidContent(t *testing.T) {
	want := hashOf("valid digest body")
	cases := []struct {
		name    string
		content Content
	}{
		{"empty sha256", Content{SHA1: want.SHA1, Size: want.Size}},
		{"empty sha1", Content{SHA256: want.SHA256, Size: want.Size}},
		{"sha256 short", Content{SHA1: want.SHA1, SHA256: "abcd", Size: want.Size}},
		{"sha256 long", Content{SHA1: want.SHA1, SHA256: strings.Repeat("a", 66), Size: want.Size}},
		{"sha1 long", Content{SHA1: strings.Repeat("a", 64), SHA256: want.SHA256, Size: want.Size}},
		{"sha256 not hex", Content{SHA1: want.SHA1, SHA256: strings.Repeat("z", 64), Size: want.Size}},
		{"sha1 not hex", Content{SHA1: strings.Repeat("g", 40), SHA256: want.SHA256, Size: want.Size}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
			path := writeTempBody(t, s, "valid digest body")

			if _, err := s.AdoptTrusted(context.Background(), path, tc.content, "", ""); !errors.Is(err, ErrInvalidDigest) {
				t.Fatalf("got %v, want ErrInvalidDigest", err)
			}
			assertNoAdoptSideEffects(t, s, dir, path)
		})
	}
}

func TestAdoptTrustedRejectsInvalidSource(t *testing.T) {
	const body = "trusted source body"
	want := hashOf(body)

	t.Run("negative size", func(t *testing.T) {
		dir := t.TempDir()
		s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
		path := writeTempBody(t, s, body)
		content := want
		content.Size = -1
		if _, err := s.AdoptTrusted(context.Background(), path, content, "", ""); !errors.Is(err, ErrInvalidAdopt) {
			t.Fatalf("got %v, want ErrInvalidAdopt", err)
		}
		assertNoAdoptSideEffects(t, s, dir, path)
	})

	t.Run("oversized", func(t *testing.T) {
		dir := t.TempDir()
		s := openLocal(t, Config{Dir: dir, Capacity: 8, MaxSize: 8})
		body := strings.Repeat("x", 9)
		path := writeTempBody(t, s, body)
		if _, err := s.AdoptTrusted(context.Background(), path, hashOf(body), "", ""); !errors.Is(err, ErrOversized) {
			t.Fatalf("got %v, want ErrOversized", err)
		}
		assertNoAdoptSideEffects(t, s, dir, path)
	})

	t.Run("size claim disagrees with file", func(t *testing.T) {
		dir := t.TempDir()
		s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
		path := writeTempBody(t, s, body)
		content := want
		content.Size = int64(len(body)) + 1
		if _, err := s.AdoptTrusted(context.Background(), path, content, "", ""); !errors.Is(err, ErrInvalidAdopt) {
			t.Fatalf("got %v, want ErrInvalidAdopt", err)
		}
		assertNoAdoptSideEffects(t, s, dir, path)
	})

	t.Run("outside temp dir", func(t *testing.T) {
		dir := t.TempDir()
		s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
		path := filepath.Join(dir, "elsewhere")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AdoptTrusted(context.Background(), path, want, "", ""); !errors.Is(err, ErrInvalidAdopt) {
			t.Fatalf("got %v, want ErrInvalidAdopt", err)
		}
		if records, err := s.index.records(); err != nil || len(records) != 0 {
			t.Fatalf("records = %d, err = %v", len(records), err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
		target := filepath.Join(dir, "outside-tmp")
		if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(s.tmpDir, "link")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AdoptTrusted(context.Background(), path, want, "", ""); !errors.Is(err, ErrInvalidAdopt) {
			t.Fatalf("got %v, want ErrInvalidAdopt", err)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != body {
			t.Fatalf("symlink target changed: %q %v", got, err)
		}
	})
}

func TestAdoptTrustedHonoursCancelledContext(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
	body := "cancelled trusted adopt"
	path := writeTempBody(t, s, body)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.AdoptTrusted(ctx, path, hashOf(body), "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	assertNoAdoptSideEffects(t, s, dir, path)
}

func TestAdoptTrustedRejectsBelowWaterMark(t *testing.T) {
	dir := t.TempDir()
	probe := &spaceProbe{free: 1 << 40}
	s := openLocal(t, watermarkConfig(dir, 1<<10, diskspace.WithProbe(probe.probe)))

	const body = "trusted adopt under low water"
	path := writeTempBody(t, s, body)
	probe.setFree(0)
	if _, err := s.AdoptTrusted(context.Background(), path, hashOf(body), "", ""); !errors.Is(err, diskspace.ErrLowDiskSpace) {
		t.Fatalf("got %v, want ErrLowDiskSpace", err)
	}
	assertNoAdoptSideEffects(t, s, dir, path)
}

func TestAdoptTrustedExpectedDigest(t *testing.T) {
	cases := []struct {
		name   string
		algo   Algorithm
		digest func(Content) string
	}{
		{"sha256", SHA256, func(c Content) string { return c.SHA256 }},
		{"sha1", SHA1, func(c Content) string { return c.SHA1 }},
		{"sha256 uppercase", SHA256, func(c Content) string { return strings.ToUpper(c.SHA256) }},
		{"none", "", func(Content) string { return "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
			body := "trusted-expected-" + strings.ReplaceAll(tc.name, " ", "-")
			want := hashOf(body)
			path := writeTempBody(t, s, body)

			c, err := s.AdoptTrusted(context.Background(), path, want, tc.algo, tc.digest(want))
			if err != nil {
				t.Fatalf("adopt trusted: %v", err)
			}
			if c != want {
				t.Fatalf("content = %+v, want %+v", c, want)
			}
			if got := readContent(t, s, SHA1, want.SHA1); got != body {
				t.Fatalf("sha1 body = %q", got)
			}
		})
	}
}

func TestAdoptTrustedRejectsBadExpectation(t *testing.T) {
	other := hashOf("different body")
	cases := []struct {
		name   string
		algo   Algorithm
		digest string
		want   error
	}{
		{"sha256 mismatch", SHA256, other.SHA256, ErrDigestMismatch},
		{"sha1 mismatch", SHA1, other.SHA1, ErrDigestMismatch},
		{"sha256 invalid hex", SHA256, strings.Repeat("z", 64), ErrInvalidDigest},
		{"sha256 too short", SHA256, "abcd", ErrInvalidDigest},
		{"sha1 wrong length", SHA1, strings.Repeat("a", 64), ErrInvalidDigest},
		{"unknown algorithm", Algorithm("md5"), strings.Repeat("a", 32), ErrInvalidDigest},
		{"digest without algorithm", "", other.SHA256, ErrInvalidDigest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
			body := "trusted-rejected-" + strings.ReplaceAll(tc.name, " ", "-")
			path := writeTempBody(t, s, body)

			_, err := s.AdoptTrusted(context.Background(), path, hashOf(body), tc.algo, tc.digest)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			assertNoAdoptSideEffects(t, s, dir, path)
		})
	}
}

// A trusted mismatch must leave an existing body under that digest untouched.
func TestAdoptTrustedMismatchDoesNotTouchExistingBody(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
	good := putBody(t, s, "good body")

	path := writeTempBody(t, s, "evil body")
	_, err := s.AdoptTrusted(context.Background(), path, hashOf("evil body"), SHA256, good.SHA256)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("got %v, want ErrDigestMismatch", err)
	}
	if got := readContent(t, s, SHA256, good.SHA256); got != "good body" {
		t.Fatalf("existing body = %q", got)
	}
	if st := s.Stats(); st.Objects != 1 || st.Bytes != int64(len("good body")) {
		t.Fatalf("stats = %+v", st)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("source was removed on a rejected adopt: %v", err)
	}
}

func TestAdoptTrustedIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
	const body = "one trusted body, two sources"
	first := writeTempBody(t, s, body)
	second := writeTempBody(t, s, body)

	a, err := s.AdoptTrusted(context.Background(), first, hashOf(body), "", "")
	if err != nil {
		t.Fatalf("first adopt: %v", err)
	}
	b, err := s.AdoptTrusted(context.Background(), second, hashOf(body), SHA1, hashOf(body).SHA1)
	if err != nil {
		t.Fatalf("second adopt: %v", err)
	}
	if a != b {
		t.Fatalf("digests differ: %+v vs %+v", a, b)
	}
	if st := s.Stats(); st.Objects != 1 || st.Bytes != int64(len(body)) {
		t.Fatalf("stats = %+v", st)
	}
	if n := countFiles(t, filepath.Join(dir, dataDirName)); n != 1 {
		t.Fatalf("body tree holds %d files, want 1", n)
	}
}

// The trusted path uses the same publish window as Put and Adopt: an eviction
// and a reconcile that run between the backend put and the index commit must not
// delete the body the commit is about to name.
func TestAdoptTrustedPublishWindowSurvivesEvictionAndReconcile(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 100, MaxSize: 100})

	body := strings.Repeat("t", 60)
	x := hashOf(body)

	parked := make(chan struct{})
	releaseCh := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseCh) })
	t.Cleanup(release)
	var calls atomic.Int32
	s.testAfterPublish = func() {
		// Park only the AdoptTrusted; the Put below also passes this hook.
		if calls.Add(1) == 1 {
			close(parked)
			<-releaseCh
		}
	}

	src := writeTempBody(t, s, body)
	adoptErr := make(chan error, 1)
	go func() {
		_, err := s.AdoptTrusted(context.Background(), src, x, "", "")
		adoptErr <- err
	}()
	<-parked // the body is in place; the commit has not run.

	be := newLocalBackend(filepath.Join(dir, dataDirName))
	dataPath, err := be.path(x.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("body missing right after publish: %v", err)
	}

	y := strings.Repeat("y", 60)
	if _, err := s.Put(context.Background(), strings.NewReader(y), int64(len(y))); err != nil {
		t.Fatalf("put while X is publishing: %v", err)
	}
	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("eviction deleted the in-flight body: %v", err)
	}
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

	if got := readContent(t, s, SHA256, x.SHA256); got != body {
		t.Fatalf("sha256 body = %q", got)
	}
	if got := readContent(t, s, SHA1, x.SHA1); got != body {
		t.Fatalf("sha1 body = %q", got)
	}
}

func TestSharedAdoptTrustedPublishesBodyAndPointers(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	const body = "shared trusted body"

	path := writeTempBody(t, s, body)
	c, err := s.AdoptTrusted(context.Background(), path, hashOf(body), SHA256, hashOf(body).SHA256)
	if err != nil {
		t.Fatalf("adopt trusted: %v", err)
	}
	if got, ok := fake.get(s3BodyKey(c.SHA256)); !ok || got != body {
		t.Fatalf("body object = %q ok=%v", got, ok)
	}
	if got, ok := fake.get(s3SHA1AliasKey(c.SHA1)); !ok || got != c.SHA256 {
		t.Fatalf("sha1 pointer = %q ok=%v, want %q", got, ok, c.SHA256)
	}
	if got, ok := fake.get(s3SHA256AliasKey(c.SHA256)); !ok || got != c.SHA1 {
		t.Fatalf("sha256 pointer = %q ok=%v, want %q", got, ok, c.SHA1)
	}
	// A shared adopt uploads; the source stays for the caller to remove.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("shared adopt removed the source: %v", err)
	}
	if got := readContent(t, s, SHA1, c.SHA1); got != body {
		t.Fatalf("sha1 body = %q", got)
	}
	if got := readContent(t, s, SHA256, c.SHA256); got != body {
		t.Fatalf("sha256 body = %q", got)
	}
	if st := s.Stats(); st.Objects != 1 || st.Bytes != int64(len(body)) {
		t.Fatalf("stats = %+v", st)
	}

	other := openShared(t, t.TempDir(), fake, 1<<20, 0)
	if got := readContent(t, other, SHA1, c.SHA1); got != body {
		t.Fatalf("trusted body on a second instance = %q", got)
	}
}

func TestSharedAdoptTrustedMismatchLeavesBucketEmpty(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	const body = "trusted no expectation match"
	path := writeTempBody(t, s, body)

	_, err := s.AdoptTrusted(context.Background(), path, hashOf(body), SHA256, hashOf("other").SHA256)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("got %v, want ErrDigestMismatch", err)
	}
	if n := fake.lenObjects(); n != 0 {
		t.Fatalf("bucket received %d objects", n)
	}
	if st := s.Stats(); st.Objects != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("source was removed: %v", err)
	}
}

func TestSharedAdoptTrustedPublishFailureRollsBack(t *testing.T) {
	fake := newFakeS3()
	fake.failPutPrefix = "relay/aliases/sha1/"
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	const body = "trusted rollback adopt"
	path := writeTempBody(t, s, body)

	if _, err := s.AdoptTrusted(context.Background(), path, hashOf(body), "", ""); err == nil {
		t.Fatal("expected the pointer write to fail")
	}
	if n := fake.lenObjects(); n != 0 {
		t.Fatalf("bucket leaked %d objects", n)
	}
	if st := s.Stats(); st.Objects != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("source was removed on a failed publish: %v", err)
	}
}

// The seam distinguishes the two contracts structurally: Adopt re-reads the file
// to compute digests, AdoptTrusted does not.
func TestAdoptTrustedDoesNotRereadForDigests(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	var reads atomic.Int32
	s.testDigestRead = func() { reads.Add(1) }

	const body = "trusted digest read"
	path := writeTempBody(t, s, body)
	if _, err := s.AdoptTrusted(context.Background(), path, hashOf(body), "", ""); err != nil {
		t.Fatalf("adopt trusted: %v", err)
	}
	if got := reads.Load(); got != 0 {
		t.Fatalf("AdoptTrusted performed %d digest reads, want 0", got)
	}

	const other = "untrusted digest read"
	path = writeTempBody(t, s, other)
	if _, err := s.Adopt(context.Background(), path, int64(len(other)), "", ""); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("Adopt performed %d digest reads, want exactly 1", got)
	}
}
