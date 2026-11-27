package hashstore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

const testVariantURL = "https://upstream.example/api/v1/items?page=1"

func testVariant(t *testing.T, url, body string, vary []string, matched map[string]string) Variant {
	t.Helper()
	c := hashOf(body)
	req := make(http.Header, len(matched))
	for name, value := range matched {
		req[name] = []string{value}
	}
	return Variant{
		URL:             url,
		Vary:            vary,
		RequestHeaders:  req,
		ResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		BodySHA256:      c.SHA256,
		BodySize:        c.Size,
		StoredAt:        time.Unix(1_700_000_000, 123),
		ExpiresAt:       time.Unix(1_700_000_600, 0),
	}
}

func putVariant(t *testing.T, s *Store, v Variant) {
	t.Helper()
	if err := s.PutVariant(context.Background(), v); err != nil {
		t.Fatalf("put variant %q: %v", v.URL, err)
	}
}

func variantsFor(t *testing.T, s *Store, url string) []Variant {
	t.Helper()
	got, err := s.Variants(context.Background(), url)
	if err != nil {
		t.Fatalf("variants %q: %v", url, err)
	}
	return got
}

// Metadata survives a restart, including the Vary contract, the matched request
// values, the response headers and the body digest.
func TestVariantPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir, Capacity: 1 << 20}
	s := openLocal(t, cfg)

	gzipBody := putBody(t, s, "gzip body")
	brBody := putBody(t, s, "br body")
	gzip := testVariant(t, testVariantURL, "gzip body", []string{"Accept-Encoding"}, map[string]string{"accept-encoding": "gzip"})
	br := testVariant(t, testVariantURL, "br body", []string{"Accept-Encoding"}, map[string]string{"accept-encoding": "br"})
	br.ResponseHeaders.Set("ETag", `"br"`)
	noVary := testVariant(t, "https://upstream.example/asset.js", "plain", nil, nil)
	noVary.NoVary = true

	putVariant(t, s, gzip)
	putVariant(t, s, br)
	putVariant(t, s, noVary)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := openLocal(t, cfg)
	got := variantsFor(t, reopened, testVariantURL)
	if len(got) != 2 {
		t.Fatalf("variants = %d, want 2", len(got))
	}
	// Key order puts the Accept-Encoding value "br" before "gzip".
	if got[0].BodySHA256 != brBody.SHA256 || got[0].BodySize != brBody.Size {
		t.Fatalf("first variant body = %s/%d, want %s/%d", got[0].BodySHA256, got[0].BodySize, brBody.SHA256, brBody.Size)
	}
	if got[1].BodySHA256 != gzipBody.SHA256 {
		t.Fatalf("second variant body = %s, want %s", got[1].BodySHA256, gzipBody.SHA256)
	}
	for _, v := range got {
		if v.URL != testVariantURL || v.NoVary {
			t.Fatalf("identity changed across restart: %+v", v)
		}
		if len(v.Vary) != 1 || v.Vary[0] != "accept-encoding" {
			t.Fatalf("vary = %v", v.Vary)
		}
		if v.ResponseHeaders.Get("Content-Type") != "application/json" {
			t.Fatalf("response headers = %v", v.ResponseHeaders)
		}
		if !v.StoredAt.Equal(gzip.StoredAt) || !v.ExpiresAt.Equal(gzip.ExpiresAt) {
			t.Fatalf("times = %v/%v, want %v/%v", v.StoredAt, v.ExpiresAt, gzip.StoredAt, gzip.ExpiresAt)
		}
	}
	if got[1].RequestHeaders["accept-encoding"][0] != "gzip" {
		t.Fatalf("gzip matched header = %v", got[1].RequestHeaders)
	}
	if got[0].RequestHeaders["accept-encoding"][0] != "br" {
		t.Fatalf("br matched header = %v", got[0].RequestHeaders)
	}
	if got[0].ResponseHeaders.Get("ETag") != `"br"` {
		t.Fatalf("ETag = %q", got[0].ResponseHeaders.Get("ETag"))
	}

	plain := variantsFor(t, reopened, "https://upstream.example/asset.js")
	if len(plain) != 1 || !plain[0].NoVary || plain[0].Vary != nil {
		t.Fatalf("no-vary variant = %+v", plain)
	}

	// The reverse references persisted too: deleting a body removes exactly the
	// variant that named it.
	if err := reopened.Delete(context.Background(), SHA256, gzipBody.SHA256); err != nil {
		t.Fatalf("delete gzip body: %v", err)
	}
	got = variantsFor(t, reopened, testVariantURL)
	if len(got) != 1 || got[0].BodySHA256 != brBody.SHA256 {
		t.Fatalf("after body delete variants = %+v, want only br", got)
	}
}

// The same identity replaces in place and moves the body reference with it: the
// old body can be deleted without taking the new variant with it.
func TestPutVariantSameIdentityReplaces(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	oldBody := putBody(t, s, "old representation")
	newBody := putBody(t, s, "new representation")

	v := testVariant(t, testVariantURL, "old representation", []string{"Accept-Encoding"}, map[string]string{"accept-encoding": "gzip"})
	putVariant(t, s, v)

	replacement := v
	replacement.BodySHA256 = newBody.SHA256
	replacement.BodySize = newBody.Size
	replacement.StoredAt = v.StoredAt.Add(time.Minute)
	replacement.ResponseHeaders = http.Header{"Content-Type": {"application/json"}, "X-Generation": {"2"}}
	putVariant(t, s, replacement)

	got := variantsFor(t, s, testVariantURL)
	if len(got) != 1 {
		t.Fatalf("variants = %d, want 1", len(got))
	}
	if got[0].BodySHA256 != newBody.SHA256 || got[0].ResponseHeaders.Get("X-Generation") != "2" {
		t.Fatalf("not replaced: %+v", got[0])
	}
	if !got[0].StoredAt.Equal(replacement.StoredAt) {
		t.Fatalf("storedAt = %v, want %v", got[0].StoredAt, replacement.StoredAt)
	}

	// The old body's reverse reference must be gone, or this delete would also
	// erase the variant that now points at the new body.
	if err := s.Delete(context.Background(), SHA256, oldBody.SHA256); err != nil {
		t.Fatalf("delete old body: %v", err)
	}
	if got := variantsFor(t, s, testVariantURL); len(got) != 1 {
		t.Fatalf("old body delete removed the replacement: %d variants", len(got))
	}

	if err := s.Delete(context.Background(), SHA256, newBody.SHA256); err != nil {
		t.Fatalf("delete new body: %v", err)
	}
	if got := variantsFor(t, s, testVariantURL); len(got) != 0 {
		t.Fatalf("new body delete kept %d variants", len(got))
	}
}

// Revalidation may change the Vary set, which changes the key. The old variant
// and its body reference must be gone; sibling variants stay.
func TestReplaceVariantChangesVary(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	gzipBody := putBody(t, s, "gzip")
	brBody := putBody(t, s, "br")
	nextBody := putBody(t, s, "negotiated")

	gzip := testVariant(t, testVariantURL, "gzip", []string{"Accept-Encoding"}, map[string]string{"accept-encoding": "gzip"})
	br := testVariant(t, testVariantURL, "br", []string{"Accept-Encoding"}, map[string]string{"accept-encoding": "br"})
	putVariant(t, s, gzip)
	putVariant(t, s, br)
	if got := variantsFor(t, s, testVariantURL); len(got) != 2 {
		t.Fatalf("before replace: %d variants", len(got))
	}

	next := testVariant(t, testVariantURL, "negotiated", []string{"Accept-Encoding", "Accept"}, map[string]string{
		"Accept-Encoding": "gzip",
		"Accept":          "text/html",
	})
	if err := s.ReplaceVariant(context.Background(), gzip, next); err != nil {
		t.Fatalf("replace: %v", err)
	}

	got := variantsFor(t, s, testVariantURL)
	if len(got) != 2 {
		t.Fatalf("after replace: %d variants, want 2", len(got))
	}
	var moved *Variant
	for i := range got {
		if got[i].BodySHA256 == nextBody.SHA256 {
			moved = &got[i]
		}
	}
	if moved == nil {
		t.Fatalf("replacement missing: %+v", got)
	}
	if want := []string{"accept", "accept-encoding"}; strings.Join(moved.Vary, ",") != strings.Join(want, ",") {
		t.Fatalf("vary = %v, want %v", moved.Vary, want)
	}
	if moved.RequestHeaders["accept"][0] != "text/html" || moved.RequestHeaders["accept-encoding"][0] != "gzip" {
		t.Fatalf("matched headers = %v", moved.RequestHeaders)
	}

	// Deleting the old body must not touch the moved variant or the sibling.
	if err := s.Delete(context.Background(), SHA256, gzipBody.SHA256); err != nil {
		t.Fatalf("delete old body: %v", err)
	}
	if got := variantsFor(t, s, testVariantURL); len(got) != 2 {
		t.Fatalf("old body delete changed variants: %d", len(got))
	}

	// Replacing the moved variant with a no-vary one leaves the sibling alone.
	base := next
	base.Vary = nil
	base.RequestHeaders = nil
	if err := s.ReplaceVariant(context.Background(), next, base); err != nil {
		t.Fatalf("replace to no-vary: %v", err)
	}
	got = variantsFor(t, s, testVariantURL)
	if len(got) != 2 {
		t.Fatalf("after no-vary replace: %d variants", len(got))
	}
	var noVary, stillBr bool
	for _, v := range got {
		switch {
		case v.NoVary:
			noVary = true
		case len(v.Vary) == 1 && v.Vary[0] == "accept-encoding" && v.BodySHA256 == brBody.SHA256:
			stillBr = true
		}
	}
	if !noVary || !stillBr {
		t.Fatalf("variants = %+v, want a no-vary and the br sibling", got)
	}
}

// A no-vary response is one variant per URL, whatever Vary says; Vary "*" and an
// empty Vary collapse into the same identity.
func TestVariantNoVaryIsSingle(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	firstBody := putBody(t, s, "first")
	secondBody := putBody(t, s, "second")

	first := testVariant(t, testVariantURL, "first", []string{"Accept-Encoding"}, map[string]string{"accept-encoding": "gzip"})
	first.NoVary = true
	putVariant(t, s, first)

	second := testVariant(t, testVariantURL, "second", []string{"*"}, map[string]string{"accept-encoding": "br"})
	putVariant(t, s, second)

	got := variantsFor(t, s, testVariantURL)
	if len(got) != 1 {
		t.Fatalf("variants = %d, want a single no-vary entry", len(got))
	}
	if !got[0].NoVary || got[0].Vary != nil || len(got[0].RequestHeaders) != 0 {
		t.Fatalf("identity = %+v, want no-vary with no Vary fields", got[0])
	}
	if got[0].BodySHA256 != secondBody.SHA256 {
		t.Fatalf("body = %s, want the second write %s", got[0].BodySHA256, secondBody.SHA256)
	}

	// An empty Vary set is the same single-representation case.
	third := testVariant(t, testVariantURL, "first", nil, nil)
	putVariant(t, s, third)
	got = variantsFor(t, s, testVariantURL)
	if len(got) != 1 || got[0].BodySHA256 != firstBody.SHA256 {
		t.Fatalf("empty Vary did not collapse into the no-vary entry: %+v", got)
	}
}

// Eviction removes the body and every variant that named it, in one transaction.
func TestVariantRemovedWithBodyEviction(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 100, MaxSize: 100})
	a := putBody(t, s, strings.Repeat("a", 40))
	b := putBody(t, s, strings.Repeat("b", 40))
	putVariant(t, s, testVariant(t, "https://upstream.example/a", strings.Repeat("a", 40), nil, nil))
	putVariant(t, s, testVariant(t, "https://upstream.example/b", strings.Repeat("b", 40), nil, nil))

	// Push past the budget: the LRU tail (A) is evicted.
	putBody(t, s, strings.Repeat("c", 40))

	if _, _, err := s.Get(context.Background(), SHA256, a.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A was not evicted: %v", err)
	}
	if got := variantsFor(t, s, "https://upstream.example/a"); len(got) != 0 {
		t.Fatalf("evicted body kept %d variants", len(got))
	}
	if got := variantsFor(t, s, "https://upstream.example/b"); len(got) != 1 {
		t.Fatalf("live body lost its variant: %d", len(got))
	}
	if got := readContent(t, s, SHA256, b.SHA256); got != strings.Repeat("b", 40) {
		t.Fatalf("B body = %q", got)
	}
}

func TestVariantRemovedWithBodyDelete(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	c := putBody(t, s, "delete together")
	putVariant(t, s, testVariant(t, testVariantURL, "delete together", nil, nil))

	if err := s.Delete(context.Background(), SHA256, c.SHA256); err != nil {
		t.Fatalf("delete body: %v", err)
	}
	if got := variantsFor(t, s, testVariantURL); len(got) != 0 {
		t.Fatalf("deleted body kept %d variants", len(got))
	}
}

// Prune repairs metadata whose body record is gone without changing the
// established body counts.
func TestPruneDropsDanglingVariants(t *testing.T) {
	dir := t.TempDir()
	seed, err := Open(Config{Dir: dir, Capacity: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	body := putBody(t, seed, "indexed")
	putVariant(t, seed, testVariant(t, "https://upstream.example/kept", "indexed", nil, nil))
	// A variant whose body was never stored: possible after a body write failed
	// or a shared body was reclaimed by another relay.
	dangling := testVariant(t, "https://upstream.example/dangling", "never stored", nil, nil)
	putVariant(t, seed, dangling)
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	s := pruneLocal(t, dir)
	stats, err := s.Prune(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if want := (PruneStats{Scanned: 1, Kept: 1}); stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if got := variantsFor(t, s, "https://upstream.example/dangling"); len(got) != 0 {
		t.Fatalf("dangling metadata survived: %+v", got)
	}
	if got := variantsFor(t, s, "https://upstream.example/kept"); len(got) != 1 {
		t.Fatalf("live metadata was pruned: %+v", got)
	}

	// Now remove the live body: prune drops the record and its metadata.
	be := newLocalBackend(filepath.Join(dir, dataDirName))
	p, err := be.path(body.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	stats, err = s.Prune(context.Background())
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if want := (PruneStats{Stale: 1}); stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if got := variantsFor(t, s, "https://upstream.example/kept"); len(got) != 0 {
		t.Fatalf("metadata outlived its body: %+v", got)
	}
}

// Metadata is local: a shared bucket gains no metadata objects, and a body a
// peer deleted leaves this instance's metadata alone until the read MISS repairs
// it.
func TestSharedVariantMetadataStaysLocal(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	c := putBody(t, s, "shared variant body")
	if n := fake.lenObjects(); n != 3 {
		t.Fatalf("objects after body put = %d, want 3", n)
	}

	putVariant(t, s, testVariant(t, testVariantURL, "shared variant body", []string{"Accept-Encoding"}, map[string]string{"accept-encoding": "gzip"}))
	if n := fake.lenObjects(); n != 3 {
		t.Fatalf("variant metadata wrote %d objects to the bucket", n-3)
	}

	// Another relay evicts the body.
	for _, key := range []string{s3BodyKey(c.SHA256), s3SHA1AliasKey(c.SHA1), s3SHA256AliasKey(c.SHA256)} {
		fake.deleteRaw(key)
	}
	if got := variantsFor(t, s, testVariantURL); len(got) != 1 {
		t.Fatalf("peer deletion touched local metadata: %d variants", len(got))
	}

	// The next read is a MISS, which forgets the body and its metadata.
	if _, _, err := s.Get(context.Background(), SHA256, c.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted body: got %v, want ErrNotFound", err)
	}
	if got := variantsFor(t, s, testVariantURL); len(got) != 0 {
		t.Fatalf("MISS left %d variants", len(got))
	}
}

func TestVariantValidation(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	valid := testVariant(t, testVariantURL, "x", nil, nil)

	cases := []struct {
		name string
		v    Variant
		want error
	}{
		{"empty url", func() Variant { v := valid; v.URL = "  "; return v }(), ErrInvalidVariant},
		{"bad digest", func() Variant { v := valid; v.BodySHA256 = "abc"; return v }(), ErrInvalidDigest},
		{"negative size", func() Variant { v := valid; v.BodySize = -1; return v }(), ErrInvalidVariant},
		{"oversized key", func() Variant { v := valid; v.URL = strings.Repeat("u", maxVariantKeySize); return v }(), ErrInvalidVariant},
	}
	for _, tc := range cases {
		if err := s.PutVariant(context.Background(), tc.v); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := s.Variants(context.Background(), " "); !errors.Is(err, ErrInvalidVariant) {
		t.Errorf("empty URL listing: got %v", err)
	}
	// Deleting an absent variant is idempotent.
	if err := s.DeleteVariant(context.Background(), valid); err != nil {
		t.Errorf("delete absent: %v", err)
	}
}

func TestVariantMatchesRequestHeaders(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	putBody(t, s, "gzip")
	putBody(t, s, "br")

	gzip := testVariant(t, testVariantURL, "gzip", []string{"accept-encoding"}, map[string]string{"accept-encoding": "gzip"})
	putVariant(t, s, gzip)
	br := testVariant(t, testVariantURL, "br", []string{"accept-encoding"}, map[string]string{"accept-encoding": "br"})
	putVariant(t, s, br)

	stored := variantsFor(t, s, testVariantURL)
	if len(stored) != 2 {
		t.Fatalf("variants = %d", len(stored))
	}
	for _, v := range stored {
		want := v.RequestHeaders["accept-encoding"][0]
		if !v.Matches(http.Header{"Accept-Encoding": {want}}) {
			t.Errorf("variant %s should match %q", v.BodySHA256, want)
		}
		other := "br"
		if want == "br" {
			other = "gzip"
		}
		if v.Matches(http.Header{"Accept-Encoding": {other}}) {
			t.Errorf("variant %s matched the wrong encoding %q", v.BodySHA256, other)
		}
	}

	noVary := testVariant(t, "https://upstream.example/one", "gzip", nil, nil)
	if !noVary.Matches(http.Header{"Accept-Encoding": {"zstd"}}) {
		t.Error("a no-vary variant must match every request")
	}
}

func TestReconcileDropsVariantsOfOrphans(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 100, MaxSize: 100})

	// A body file with no index record, as a crash between publish and commit
	// would leave. A variant naming it is dangling.
	orphan := hashOf("orphan body").SHA256
	be := newLocalBackend(filepath.Join(dir, dataDirName))
	p, err := be.path(orphan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("orphan body"), 0o600); err != nil {
		t.Fatal(err)
	}
	putVariant(t, s, testVariant(t, "https://upstream.example/orphan", "orphan body", nil, nil))

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan body survived: %v", err)
	}
	if got := variantsFor(t, s, "https://upstream.example/orphan"); len(got) != 0 {
		t.Fatalf("orphan metadata survived: %+v", got)
	}
}

// Prune repairs the reverse references too: a missing one is recreated, so a
// later eviction still finds the metadata, and one pointing at the wrong body is
// dropped.
func TestPruneRepairsVariantReferences(t *testing.T) {
	dir := t.TempDir()
	seed, err := Open(Config{Dir: dir, Capacity: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	body := putBody(t, seed, "kept")
	putVariant(t, seed, testVariant(t, testVariantURL, "kept", nil, nil))

	correct, err := rawKey(body.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := rawKey(hashOf("some other body").SHA256)
	if err != nil {
		t.Fatal(err)
	}

	seed.mu.Lock()
	var variantKey []byte
	if err := seed.index.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(variantsBucket).ForEach(func(k, _ []byte) error {
			variantKey = append([]byte(nil), k...)
			return nil
		})
	}); err != nil {
		seed.mu.Unlock()
		t.Fatal(err)
	}
	if len(variantKey) == 0 {
		seed.mu.Unlock()
		t.Fatal("variant row missing")
	}
	if err := seed.index.db.Update(func(tx *bolt.Tx) error {
		refs := tx.Bucket(variantRefsBucket)
		if err := refs.Delete(append(correct, variantKey...)); err != nil {
			return err
		}
		return refs.Put(append(wrong, variantKey...), []byte{1})
	}); err != nil {
		seed.mu.Unlock()
		t.Fatal(err)
	}
	seed.mu.Unlock()
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	s := pruneLocal(t, dir)
	stats, err := s.Prune(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if want := (PruneStats{Scanned: 1, Kept: 1}); stats != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if got := variantsFor(t, s, testVariantURL); len(got) != 1 {
		t.Fatalf("variant lost by the repair: %d", len(got))
	}

	s.mu.Lock()
	err = s.index.db.View(func(tx *bolt.Tx) error {
		refs := tx.Bucket(variantRefsBucket)
		if refs.Get(append(correct, variantKey...)) == nil {
			return errors.New("correct reference not restored")
		}
		if refs.Get(append(wrong, variantKey...)) != nil {
			return errors.New("wrong-body reference survived")
		}
		return nil
	})
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

// Concurrent metadata writes, replacements, reads and body removals must stay
// consistent under -race.
func TestConcurrentVariants(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 22, MaxSize: 1 << 20})
	ctx := context.Background()

	var workers sync.WaitGroup
	for w := 0; w < 8; w++ {
		workers.Add(1)
		go func(w int) {
			defer workers.Done()
			for i := 0; i < 20; i++ {
				body := fmt.Sprintf("concurrent-%d-%d", w, i)
				c, err := s.Put(ctx, strings.NewReader(body), -1)
				if err != nil {
					t.Errorf("put body: %v", err)
					return
				}
				url := fmt.Sprintf("https://upstream.example/item/%d", i%4)
				v := Variant{
					URL:             url,
					Vary:            []string{"Accept-Encoding"},
					RequestHeaders:  http.Header{"accept-encoding": {fmt.Sprintf("enc-%d", w)}},
					ResponseHeaders: http.Header{"Content-Type": {"text/plain"}},
					BodySHA256:      c.SHA256,
					BodySize:        c.Size,
				}
				if err := s.PutVariant(ctx, v); err != nil {
					t.Errorf("put variant: %v", err)
					return
				}
				if _, err := s.Variants(ctx, url); err != nil {
					t.Errorf("list variants: %v", err)
					return
				}

				next := v
				next.BodySHA256 = hashOf(body + "-next").SHA256
				next.BodySize = int64(len(body + "-next"))
				if i%4 == 0 {
					if err := s.ReplaceVariant(ctx, v, next); err != nil {
						t.Errorf("replace variant: %v", err)
						return
					}
				}
				if i%5 == 0 {
					if err := s.DeleteVariant(ctx, v); err != nil {
						t.Errorf("delete variant: %v", err)
						return
					}
				}
				if err := s.Delete(ctx, SHA256, c.SHA256); err != nil && !errors.Is(err, ErrNotFound) {
					t.Errorf("delete body: %v", err)
					return
				}
			}
		}(w)
	}
	workers.Wait()

	if st := s.Stats(); st.Bytes > st.Capacity {
		t.Fatalf("over capacity: %+v", st)
	}
	for i := 0; i < 4; i++ {
		url := fmt.Sprintf("https://upstream.example/item/%d", i)
		if _, err := s.Variants(ctx, url); err != nil {
			t.Fatalf("variants %s: %v", url, err)
		}
	}
}

// TestVariantPersistentFlagRoundTrip pins the spare-bit encoding: a persistent
// variant survives encode/decode, and a row written before the bit existed
// decodes as non-persistent (the zero value), so no stored row needs rewriting.
func TestVariantPersistentFlagRoundTrip(t *testing.T) {
	v := testVariant(t, testVariantURL, "body", nil, nil)
	v.NoVary = true
	v.Persistent = true

	row, err := encodeVariant(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := decodeVariant(row)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded.Persistent {
		t.Fatal("persistent flag did not survive the round trip")
	}

	// A legacy row is a version-2 row with only the no-vary bit set, which is
	// exactly what encoding a non-persistent variant produces.
	v.Persistent = false
	legacy, err := encodeVariant(v)
	if err != nil {
		t.Fatalf("encode legacy: %v", err)
	}
	old, err := decodeVariant(legacy)
	if err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if old.Persistent {
		t.Fatal("a row without the persistent bit decoded as persistent")
	}
}
