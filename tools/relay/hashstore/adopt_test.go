package hashstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mirror-db/mirror-db/tools/relay/diskspace"
)

// writeTempBody writes a completed temp file where a download would leave it.
func writeTempBody(t *testing.T, s *Store, body string) string {
	t.Helper()
	f, err := os.CreateTemp(s.tmpDir, "adopt-*")
	if err != nil {
		t.Fatalf("create temp body: %v", err)
	}
	if _, err := io.WriteString(f, body); err != nil {
		_ = f.Close()
		t.Fatalf("write temp body: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp body: %v", err)
	}
	return f.Name()
}

func adoptBody(t *testing.T, s *Store, body string) Content {
	t.Helper()
	path := writeTempBody(t, s, body)
	c, err := s.Adopt(context.Background(), path, int64(len(body)), "", "")
	if err != nil {
		t.Fatalf("adopt %d-byte body: %v", len(body), err)
	}
	return c
}

// assertNoAdoptSideEffects checks every place a rejected Adopt could have left a
// trace: accounting, the index, the body tree and the temp dir (which must hold
// only the untouched source).
func assertNoAdoptSideEffects(t *testing.T, s *Store, dir, source string) {
	t.Helper()
	if st := s.Stats(); st.Objects != 0 || st.Bytes != 0 {
		t.Fatalf("store accounting not empty: %+v", st)
	}
	records, err := s.index.records()
	if err != nil {
		t.Fatalf("index records: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("index holds %d records", len(records))
	}
	if _, err := os.Lstat(source); err != nil {
		t.Fatalf("source was removed: %v", err)
	}
	entries, err := os.ReadDir(s.tmpDir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Join(s.tmpDir, e.Name()) == source {
			continue
		}
		t.Fatalf("temp dir holds an unexpected entry %q", e.Name())
	}
	if entries, err := os.ReadDir(filepath.Join(dir, dataDirName)); err == nil && len(entries) != 0 {
		t.Fatalf("body tree holds %d entries", len(entries))
	}
}

// A local adoption publishes the body under both digests, consumes the temp
// name by renaming it, and accounts the bytes once.
func TestAdoptLocalPublishesBothDigests(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})

	const body = "adopted body"
	path := writeTempBody(t, s, body)

	c, err := s.Adopt(context.Background(), path, int64(len(body)), "", "")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	want := hashOf(body)
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
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source survived a local adopt: %v", err)
	}
	if n := countFiles(t, filepath.Join(dir, dataDirName)); n != 1 {
		t.Fatalf("body tree holds %d files, want 1", n)
	}
}

func TestAdoptExpectedDigest(t *testing.T) {
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
			body := "expected-" + strings.ReplaceAll(tc.name, " ", "-")
			want := hashOf(body)
			path := writeTempBody(t, s, body)

			c, err := s.Adopt(context.Background(), path, int64(len(body)), tc.algo, tc.digest(want))
			if err != nil {
				t.Fatalf("adopt: %v", err)
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

// A rejected expectation must not publish, index or touch an existing object.
func TestAdoptRejectsBadExpectation(t *testing.T) {
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
			body := "rejected-" + strings.ReplaceAll(tc.name, " ", "-")
			path := writeTempBody(t, s, body)

			_, err := s.Adopt(context.Background(), path, int64(len(body)), tc.algo, tc.digest)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			assertNoAdoptSideEffects(t, s, dir, path)
		})
	}
}

func TestAdoptRejectsInvalidSource(t *testing.T) {
	t.Run("declared size too large", func(t *testing.T) {
		dir := t.TempDir()
		s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
		path := writeTempBody(t, s, "short")
		if _, err := s.Adopt(context.Background(), path, 6, "", ""); !errors.Is(err, ErrInvalidAdopt) {
			t.Fatalf("got %v, want ErrInvalidAdopt", err)
		}
		assertNoAdoptSideEffects(t, s, dir, path)
	})

	t.Run("declared size too small", func(t *testing.T) {
		dir := t.TempDir()
		s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
		path := writeTempBody(t, s, "long body")
		if _, err := s.Adopt(context.Background(), path, 4, "", ""); !errors.Is(err, ErrInvalidAdopt) {
			t.Fatalf("got %v, want ErrInvalidAdopt", err)
		}
		assertNoAdoptSideEffects(t, s, dir, path)
	})

	t.Run("negative size", func(t *testing.T) {
		dir := t.TempDir()
		s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
		path := writeTempBody(t, s, "body")
		if _, err := s.Adopt(context.Background(), path, -1, "", ""); !errors.Is(err, ErrInvalidAdopt) {
			t.Fatalf("got %v, want ErrInvalidAdopt", err)
		}
		assertNoAdoptSideEffects(t, s, dir, path)
	})

	t.Run("oversized", func(t *testing.T) {
		dir := t.TempDir()
		s := openLocal(t, Config{Dir: dir, Capacity: 8, MaxSize: 8})
		path := writeTempBody(t, s, strings.Repeat("x", 9))
		if _, err := s.Adopt(context.Background(), path, 9, "", ""); !errors.Is(err, ErrOversized) {
			t.Fatalf("got %v, want ErrOversized", err)
		}
		assertNoAdoptSideEffects(t, s, dir, path)
	})

	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
		path := filepath.Join(s.tmpDir, "not-a-file")
		if err := os.Mkdir(path, 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Adopt(context.Background(), path, 0, "", ""); !errors.Is(err, ErrInvalidAdopt) {
			t.Fatalf("got %v, want ErrInvalidAdopt", err)
		}
		assertNoAdoptSideEffects(t, s, dir, path)
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
		target := filepath.Join(dir, "outside-tmp")
		if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(s.tmpDir, "link")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Adopt(context.Background(), path, 6, "", ""); !errors.Is(err, ErrInvalidAdopt) {
			t.Fatalf("got %v, want ErrInvalidAdopt", err)
		}
		assertNoAdoptSideEffects(t, s, dir, path)
		if got, err := os.ReadFile(target); err != nil || string(got) != "target" {
			t.Fatalf("symlink target changed: %q %v", got, err)
		}
	})

	t.Run("outside temp dir", func(t *testing.T) {
		dir := t.TempDir()
		s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
		path := filepath.Join(dir, "elsewhere")
		if err := os.WriteFile(path, []byte("body"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Adopt(context.Background(), path, 4, "", ""); !errors.Is(err, ErrInvalidAdopt) {
			t.Fatalf("got %v, want ErrInvalidAdopt", err)
		}
		assertNoAdoptSideEffects(t, s, dir, path)
	})

	t.Run("empty path", func(t *testing.T) {
		dir := t.TempDir()
		s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
		if _, err := s.Adopt(context.Background(), "", 0, "", ""); !errors.Is(err, ErrInvalidAdopt) {
			t.Fatalf("got %v, want ErrInvalidAdopt", err)
		}
	})
}

// A body that is already correct must not be replaced when a different file is
// offered under its digest.
func TestAdoptMismatchDoesNotTouchExistingBody(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
	good := putBody(t, s, "good body")

	path := writeTempBody(t, s, "evil body")
	_, err := s.Adopt(context.Background(), path, int64(len("evil body")), SHA256, good.SHA256)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("got %v, want ErrDigestMismatch", err)
	}
	if got := readContent(t, s, SHA256, good.SHA256); got != "good body" {
		t.Fatalf("existing body = %q", got)
	}
	if got := readContent(t, s, SHA1, good.SHA1); got != "good body" {
		t.Fatalf("existing body by sha1 = %q", got)
	}
	if st := s.Stats(); st.Objects != 1 || st.Bytes != int64(len("good body")) {
		t.Fatalf("stats = %+v", st)
	}

	// Adopting the same bytes over an existing object is idempotent.
	same := writeTempBody(t, s, "good body")
	if _, err := s.Adopt(context.Background(), same, int64(len("good body")), SHA256, good.SHA256); err != nil {
		t.Fatalf("adopt existing content: %v", err)
	}
	if st := s.Stats(); st.Objects != 1 || st.Bytes != int64(len("good body")) {
		t.Fatalf("stats after re-adopt = %+v", st)
	}
	if n := countFiles(t, filepath.Join(dir, dataDirName)); n != 1 {
		t.Fatalf("body tree holds %d files, want 1", n)
	}
}

func TestAdoptSameBodyTwice(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	const body = "one body, two sources"
	first := writeTempBody(t, s, body)
	second := writeTempBody(t, s, body)

	a, err := s.Adopt(context.Background(), first, int64(len(body)), "", "")
	if err != nil {
		t.Fatalf("first adopt: %v", err)
	}
	b, err := s.Adopt(context.Background(), second, int64(len(body)), "", "")
	if err != nil {
		t.Fatalf("second adopt: %v", err)
	}
	if a != b {
		t.Fatalf("digests differ: %+v vs %+v", a, b)
	}
	if st := s.Stats(); st.Objects != 1 || st.Bytes != int64(len(body)) {
		t.Fatalf("stats = %+v", st)
	}
	for _, path := range []string{first, second} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temp source %s survived: %v", path, err)
		}
	}
}

func TestAdoptParticipatesInLRU(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 100, MaxSize: 100})
	a := adoptBody(t, s, strings.Repeat("a", 40))
	b := adoptBody(t, s, strings.Repeat("b", 40))

	readContent(t, s, SHA256, a.SHA256)

	c := adoptBody(t, s, strings.Repeat("c", 40))

	if _, _, err := s.Get(context.Background(), SHA256, b.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B should have been evicted, got %v", err)
	}
	if got := readContent(t, s, SHA1, a.SHA1); got != strings.Repeat("a", 40) {
		t.Fatalf("A body = %q", got)
	}
	if got := readContent(t, s, SHA256, c.SHA256); got != strings.Repeat("c", 40) {
		t.Fatalf("C body = %q", got)
	}
	if st := s.Stats(); st.Objects != 2 || st.Bytes != 80 {
		t.Fatalf("stats = %+v", st)
	}
	if n := countFiles(t, filepath.Join(dir, dataDirName)); n != 2 {
		t.Fatalf("body tree holds %d files, want 2", n)
	}
}

func TestAdoptSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir, Capacity: 1 << 20}
	s := openLocal(t, cfg)

	const body = "adopted before restart"
	c := adoptBody(t, s, body)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := openLocal(t, cfg)
	if st := reopened.Stats(); st.Objects != 1 || st.Bytes != int64(len(body)) {
		t.Fatalf("restored stats = %+v", st)
	}
	if got := readContent(t, reopened, SHA1, c.SHA1); got != body {
		t.Fatalf("sha1 body after restart = %q", got)
	}
	if got := readContent(t, reopened, SHA256, c.SHA256); got != body {
		t.Fatalf("sha256 body after restart = %q", got)
	}
}

func TestAdoptRejectsBelowWaterMark(t *testing.T) {
	dir := t.TempDir()
	probe := &spaceProbe{free: 1 << 40}
	s := openLocal(t, watermarkConfig(dir, 1<<10, diskspace.WithProbe(probe.probe)))

	path := writeTempBody(t, s, "adopt under low water")
	probe.setFree(0)
	if _, err := s.Adopt(context.Background(), path, int64(len("adopt under low water")), "", ""); !errors.Is(err, diskspace.ErrLowDiskSpace) {
		t.Fatalf("got %v, want ErrLowDiskSpace", err)
	}
	assertNoAdoptSideEffects(t, s, dir, path)
}

// A failed local publish leaves the source for the caller and nothing in the
// index or the LRU.
func TestAdoptLocalPublishFailureHasNoSideEffects(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
	const body = "publish failure"

	be := newLocalBackend(filepath.Join(dir, dataDirName))
	dst, err := be.path(hashOf(body).SHA256)
	if err != nil {
		t.Fatal(err)
	}
	// A directory where the body belongs makes the rename fail.
	if err := os.MkdirAll(dst, 0o750); err != nil {
		t.Fatal(err)
	}
	path := writeTempBody(t, s, body)
	if _, err := s.Adopt(context.Background(), path, int64(len(body)), SHA256, hashOf(body).SHA256); err == nil {
		t.Fatal("expected the publish to fail")
	}
	if st := s.Stats(); st.Objects != 0 || st.Bytes != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if records, err := s.index.records(); err != nil || len(records) != 0 {
		t.Fatalf("records = %d, err = %v", len(records), err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("source was removed on failure: %v", err)
	}
}

// Concurrent Adopt and Put of one body must converge on a single self-consistent
// object.
func TestConcurrentAdoptAndPut(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1 << 16, MaxSize: 1 << 12})
	body := strings.Repeat("z", 300)

	const workers = 8
	sources := make([]string, workers)
	for i := 1; i < workers; i += 2 {
		sources[i] = writeTempBody(t, s, body)
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, err := s.Put(context.Background(), strings.NewReader(body), int64(len(body)))
				errs <- err
				return
			}
			_, err := s.Adopt(context.Background(), sources[i], int64(len(body)), "", "")
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}

	c := hashOf(body)
	if got := readContent(t, s, SHA256, c.SHA256); got != body {
		t.Fatalf("body = %q", got)
	}
	if st := s.Stats(); st.Objects != 1 || st.Bytes != int64(len(body)) {
		t.Fatalf("stats = %+v", st)
	}
	if n := countFiles(t, filepath.Join(dir, dataDirName)); n != 1 {
		t.Fatalf("body tree holds %d files, want 1", n)
	}
	if n := countFiles(t, s.tmpDir); n != 0 {
		t.Fatalf("temp dir holds %d files, want 0", n)
	}
}

func TestSharedAdoptPublishesBodyAndPointers(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	const body = "shared adopted body"

	path := writeTempBody(t, s, body)
	c, err := s.Adopt(context.Background(), path, int64(len(body)), SHA256, hashOf(body).SHA256)
	if err != nil {
		t.Fatalf("adopt: %v", err)
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

	// Another instance resolves the SHA-1 pointer without a local index.
	other := openShared(t, t.TempDir(), fake, 1<<20, 0)
	if got := readContent(t, other, SHA1, c.SHA1); got != body {
		t.Fatalf("adopted body on a second instance = %q", got)
	}
}

func TestSharedAdoptRepeatedIsIdempotent(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	const body = "re-adopt me"
	path := writeTempBody(t, s, body)

	for i := 0; i < 2; i++ {
		if _, err := s.Adopt(context.Background(), path, int64(len(body)), "", ""); err != nil {
			t.Fatalf("adopt %d: %v", i, err)
		}
	}
	if st := s.Stats(); st.Objects != 1 || st.Bytes != int64(len(body)) {
		t.Fatalf("stats = %+v", st)
	}
	if n := fake.lenObjects(); n != 3 {
		t.Fatalf("bucket holds %d objects, want body + two pointers", n)
	}
}

func TestSharedAdoptEvictsFromBucket(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 100, 100)

	a := adoptBody(t, s, strings.Repeat("a", 60))
	b := adoptBody(t, s, strings.Repeat("b", 60)) // evicts A

	if _, ok := fake.get(s3BodyKey(a.SHA256)); ok {
		t.Fatal("evicted shared body still present")
	}
	if _, ok := fake.get(s3SHA1AliasKey(a.SHA1)); ok {
		t.Fatal("evicted sha1 pointer still present")
	}
	if _, ok := fake.get(s3BodyKey(b.SHA256)); !ok {
		t.Fatal("resident shared body missing")
	}
	if st := s.Stats(); st.Objects != 1 || st.Bytes != 60 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestSharedAdoptMismatchLeavesBucketEmpty(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	const body = "no expectation match"
	path := writeTempBody(t, s, body)

	_, err := s.Adopt(context.Background(), path, int64(len(body)), SHA256, hashOf("other").SHA256)
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

func TestSharedAdoptPublishFailureCleansUp(t *testing.T) {
	fake := newFakeS3()
	fake.failPutPrefix = "relay/aliases/sha1/"
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	const body = "rollback adopt"
	path := writeTempBody(t, s, body)

	if _, err := s.Adopt(context.Background(), path, int64(len(body)), "", ""); err == nil {
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
