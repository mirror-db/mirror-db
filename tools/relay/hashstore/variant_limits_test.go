package hashstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/mirror-db/mirror-db/tools/relay/internal/variant"
)

func syntheticVariant(url, encoding string) Variant {
	body := hashOf("synthetic:" + url + ":" + encoding)
	return Variant{
		URL:             url,
		Vary:            []string{"accept-encoding"},
		RequestHeaders:  http.Header{"accept-encoding": {encoding}},
		ResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		BodySHA256:      body.SHA256,
		BodySize:        body.Size,
		StoredAt:        time.Unix(1_700_000_000, 0),
		ExpiresAt:       time.Unix(1_700_000_600, 0),
	}
}

func mustNormalizeVariant(t *testing.T, v Variant) Variant {
	t.Helper()
	normalized, err := normalizeVariant(v)
	if err != nil {
		t.Fatalf("normalize variant: %v", err)
	}
	return normalized
}

func overwriteVariantValue(t *testing.T, s *Store, key, value []byte) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.index.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(variantsBucket).Put(key, value)
	}); err != nil {
		t.Fatalf("overwrite variant: %v", err)
	}
}

func forceVariantTotal(t *testing.T, s *Store, n int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.index.db.Update(func(tx *bolt.Tx) error {
		return setVariantTotal(tx, n)
	}); err != nil {
		t.Fatalf("set variant total: %v", err)
	}
}

func hasVariantEncoding(v Variant, encoding string) bool {
	value, present := v.Identity().Value("accept-encoding")
	return present && value == encoding
}

// One URL may hold maxVariantsPerURL distinct identities. A full URL still
// accepts updates, and a replacement of an existing row can introduce a new
// identity because the old one is removed in the same transaction.
func TestVariantLimitPerURL(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	body := putBody(t, s, "real body")
	before := s.Stats()

	const url = "https://upstream.example/full"
	for i := 0; i < maxVariantsPerURL; i++ {
		v := syntheticVariant(url, fmt.Sprintf("enc-%d", i))
		if i == 0 {
			v.BodySHA256 = body.SHA256
			v.BodySize = body.Size
		}
		putVariant(t, s, v)
	}
	if got := variantsFor(t, s, url); len(got) != maxVariantsPerURL {
		t.Fatalf("variants = %d, want %d", len(got), maxVariantsPerURL)
	}

	if err := s.PutVariant(context.Background(), syntheticVariant(url, "overflow")); !errors.Is(err, ErrVariantLimit) {
		t.Fatalf("17th identity: got %v, want ErrVariantLimit", err)
	}
	if got := s.Stats(); got != before {
		t.Fatalf("variant metadata changed body accounting: %+v before %+v", got, before)
	}

	// Updating an existing identity is always allowed.
	update := syntheticVariant(url, "enc-0")
	update.StoredAt = update.StoredAt.Add(time.Hour)
	if err := s.PutVariant(context.Background(), update); err != nil {
		t.Fatalf("update at limit: %v", err)
	}

	// Replacing an existing identity with a new one keeps the URL at the cap.
	old := syntheticVariant(url, "enc-1")
	replacement := syntheticVariant(url, "enc-16")
	if err := s.ReplaceVariant(context.Background(), old, replacement); err != nil {
		t.Fatalf("replace at limit: %v", err)
	}
	got := variantsFor(t, s, url)
	if len(got) != maxVariantsPerURL {
		t.Fatalf("after replace: %d variants", len(got))
	}
	foundOld, foundNew := false, false
	for _, v := range got {
		if hasVariantEncoding(v, "enc-1") {
			foundOld = true
		}
		if hasVariantEncoding(v, "enc-16") {
			foundNew = true
		}
	}
	if foundOld {
		t.Fatal("replaced identity survived")
	}
	if !foundNew {
		t.Fatal("replacement identity missing")
	}

	// Deleting makes room for exactly one more.
	if err := s.DeleteVariant(context.Background(), syntheticVariant(url, "enc-2")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.PutVariant(context.Background(), syntheticVariant(url, "enc-17")); err != nil {
		t.Fatalf("put after delete: %v", err)
	}
	if got := variantsFor(t, s, url); len(got) != maxVariantsPerURL {
		t.Fatalf("after delete+put: %d variants", len(got))
	}
}

// A full global store still accepts an update or a replacement of an existing
// row, because neither grows the total. A genuinely new identity is rejected.
func TestVariantLimitGlobal(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	const url = "https://upstream.example/global"
	existing := syntheticVariant(url, "gzip")
	putVariant(t, s, existing)
	forceVariantTotal(t, s, maxVariantsGlobal)

	if err := s.PutVariant(context.Background(), syntheticVariant("https://upstream.example/other", "gzip")); !errors.Is(err, ErrVariantLimit) {
		t.Fatalf("new identity at full total: got %v, want ErrVariantLimit", err)
	}

	updated := existing
	updated.StoredAt = updated.StoredAt.Add(time.Hour)
	if err := s.PutVariant(context.Background(), updated); err != nil {
		t.Fatalf("update at full total: %v", err)
	}

	replacement := syntheticVariant(url, "br")
	if err := s.ReplaceVariant(context.Background(), existing, replacement); err != nil {
		t.Fatalf("replace at full total: %v", err)
	}
	if got := variantsFor(t, s, url); len(got) != 1 {
		t.Fatalf("variants = %d, want only the replacement", len(got))
	}

	if err := s.PutVariant(context.Background(), syntheticVariant("https://upstream.example/third", "gzip")); !errors.Is(err, ErrVariantLimit) {
		t.Fatalf("new identity after replace: got %v, want ErrVariantLimit", err)
	}
}

// One encoded row is capped independently of the row counts.
func TestVariantLimitEncodedValue(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	v := syntheticVariant("https://upstream.example/big", "gzip")
	v.ResponseHeaders = http.Header{"X-Big": {strings.Repeat("x", maxVariantValueSize)}}
	if err := s.PutVariant(context.Background(), v); !errors.Is(err, ErrVariantLimit) {
		t.Fatalf("oversized metadata: got %v, want ErrVariantLimit", err)
	}

	v.ResponseHeaders = http.Header{"X-Big": {strings.Repeat("x", 1024)}}
	if err := s.PutVariant(context.Background(), v); err != nil {
		t.Fatalf("normal metadata: %v", err)
	}

	next := v
	next.ResponseHeaders = http.Header{"X-Big": {strings.Repeat("x", maxVariantValueSize)}}
	if err := s.ReplaceVariant(context.Background(), v, next); !errors.Is(err, ErrVariantLimit) {
		t.Fatalf("oversized replacement: got %v, want ErrVariantLimit", err)
	}
	if got := variantsFor(t, s, v.URL); len(got) != 1 {
		t.Fatalf("failed replacement changed the entry: %d", len(got))
	}
}

// A body this instance indexes must agree on size. A shared body without a
// local record is accepted: another relay may hold it, and this instance cannot
// check it.
func TestVariantBodySizeMustMatchIndex(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	body := putBody(t, s, "known body size")
	good := syntheticVariant("https://upstream.example/body", "gzip")
	good.BodySHA256 = body.SHA256
	good.BodySize = body.Size

	bad := good
	bad.BodySize = body.Size + 1
	if err := s.PutVariant(context.Background(), bad); !errors.Is(err, ErrInvalidVariant) {
		t.Fatalf("put with wrong size: got %v, want ErrInvalidVariant", err)
	}
	if err := s.PutVariant(context.Background(), good); err != nil {
		t.Fatalf("put with right size: %v", err)
	}

	next := good
	next.BodySize = body.Size - 1
	if err := s.ReplaceVariant(context.Background(), good, next); !errors.Is(err, ErrInvalidVariant) {
		t.Fatalf("replace with wrong size: got %v, want ErrInvalidVariant", err)
	}
	got := variantsFor(t, s, good.URL)
	if len(got) != 1 || got[0].BodySize != body.Size {
		t.Fatalf("failed replace changed metadata: %+v", got)
	}

	fake := newFakeS3()
	shared := openShared(t, t.TempDir(), fake, 1<<20, 0)
	unindexed := syntheticVariant("https://upstream.example/shared", "gzip")
	if err := shared.PutVariant(context.Background(), unindexed); err != nil {
		t.Fatalf("unindexed shared body: %v", err)
	}

	sharedBody := putBody(t, shared, "shared indexed")
	indexed := syntheticVariant("https://upstream.example/shared-indexed", "gzip")
	indexed.BodySHA256 = sharedBody.SHA256
	indexed.BodySize = sharedBody.Size + 1
	if err := shared.PutVariant(context.Background(), indexed); !errors.Is(err, ErrInvalidVariant) {
		t.Fatalf("shared indexed body with wrong size: got %v", err)
	}
}

// Absent and present-but-empty request headers are distinct identities, but a
// header is the same whether the caller spelled its map key canonically or in
// lowercase.
func TestVariantHeaderPresenceIsIdentity(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	const url = "https://upstream.example/presence"
	absent := syntheticVariant(url, "ignored")
	absent.RequestHeaders = nil
	empty := syntheticVariant(url, "ignored")
	empty.RequestHeaders = http.Header{"accept-encoding": {""}}

	putVariant(t, s, absent)
	putVariant(t, s, empty)
	got := variantsFor(t, s, url)
	if len(got) != 2 {
		t.Fatalf("variants = %d, want absent and empty as two identities", len(got))
	}

	var absentStored, emptyStored *Variant
	for i := range got {
		if _, present := got[i].RequestHeaders["accept-encoding"]; present {
			emptyStored = &got[i]
		} else {
			absentStored = &got[i]
		}
	}
	if absentStored == nil || emptyStored == nil {
		t.Fatalf("presence lost across storage: %+v", got)
	}
	if !absentStored.Matches(http.Header{}) {
		t.Error("absent variant should match a request without the header")
	}
	if absentStored.Matches(http.Header{"accept-encoding": {""}}) {
		t.Error("absent variant matched an explicit empty header")
	}
	if !emptyStored.Matches(http.Header{"accept-encoding": {""}}) {
		t.Error("empty variant should match an explicit empty header")
	}
	if emptyStored.Matches(http.Header{}) {
		t.Error("empty variant matched a request without the header")
	}
	if !emptyStored.Matches(http.Header{"Accept-Encoding": {""}}) {
		t.Error("matches must lowercase the header name")
	}

	// Rewriting the absent identity must not touch the present-empty one.
	absentUpdate := absent
	absentUpdate.StoredAt = absentUpdate.StoredAt.Add(time.Hour)
	putVariant(t, s, absentUpdate)
	if got := variantsFor(t, s, url); len(got) != 2 {
		t.Fatalf("rewrite collapsed identities: %d", len(got))
	}
}

func TestNormalizeRequestHeadersMergeIsDeterministic(t *testing.T) {
	h := http.Header{
		"Accept-Encoding": {" gzip "},
		"ACCEPT-ENCODING": {"br"},
	}
	const want = "br, gzip" // sorted original names: ACCEPT-ENCODING before Accept-Encoding
	for i := 0; i < 50; i++ {
		got := variant.New(false, []string{"accept-encoding"}, h).RequestHeaders()
		if values := got["accept-encoding"]; len(values) != 1 || values[0] != want {
			t.Fatalf("iteration %d: %v, want %q", i, values, want)
		}
	}

	// Casing differences in otherwise identical requests must produce one key.
	first := mustNormalizeVariant(t, Variant{
		URL:            "https://upstream.example/key",
		Vary:           []string{"Accept-Encoding"},
		RequestHeaders: http.Header{"Accept-Encoding": {"gzip"}},
		BodySHA256:     hashOf("key body").SHA256,
		BodySize:       hashOf("key body").Size,
	})
	second := mustNormalizeVariant(t, Variant{
		URL:            "https://upstream.example/key",
		Vary:           []string{"accept-encoding"},
		RequestHeaders: http.Header{"accept-encoding": {"gzip"}},
		BodySHA256:     hashOf("key body").SHA256,
		BodySize:       hashOf("key body").Size,
	})
	if !first.Identity().Equal(second.Identity()) {
		t.Fatal("header casing changed the identity")
	}
	firstKey, err := variantKey(first)
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := variantKey(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstKey, secondKey) {
		t.Fatal("header casing changed the identity key")
	}
}

// Stored response headers are filtered even when the caller hands over raw
// upstream headers; Vary survives every filter.
func TestNormalizeResponseHeadersStripsStorageUnsafe(t *testing.T) {
	in := http.Header{
		"Content-Type":        {"text/plain"},
		"Vary":                {"Accept-Encoding"},
		"Cache-Control":       {"max-age=60"},
		"Set-Cookie":          {"a=b"},
		"Set-Cookie2":         {"c=d"},
		"Www-Authenticate":    {"Basic realm=x"},
		"X-MDB-Relay":         {"ttl=60"},
		"X-MDB-Relay-Host":    {"relay.example"},
		"Content-Length":      {"10"},
		"Age":                 {"5"},
		"Connection":          {"X-Custom, Keep-Alive"},
		"X-Custom":            {"drop"},
		"Keep-Alive":          {"timeout=5"},
		"Transfer-Encoding":   {"chunked"},
		"Proxy-Authenticate":  {"Basic"},
		"Proxy-Authorization": {"Basic x"},
		"Trailer":             {"X-T"},
		"TE":                  {"trailers"},
		"Upgrade":             {"h2c"},
	}
	out := normalizeResponseHeaders(in)
	for _, name := range []string{
		"Set-Cookie", "Set-Cookie2", "Www-Authenticate", "X-MDB-Relay",
		"X-MDB-Relay-Host", "Content-Length", "Age", "Connection", "X-Custom",
		"Keep-Alive", "Transfer-Encoding", "Proxy-Authenticate",
		"Proxy-Authorization", "Trailer", "TE", "Upgrade",
	} {
		if _, present := out[http.CanonicalHeaderKey(name)]; present {
			t.Errorf("%s survived response-header filtering", name)
		}
	}
	for _, name := range []string{"Content-Type", "Vary", "Cache-Control"} {
		if _, present := out[http.CanonicalHeaderKey(name)]; !present {
			t.Errorf("%s was dropped", name)
		}
	}

	// Lowercase map keys are filtered the same way.
	lower := normalizeResponseHeaders(http.Header{
		"set-cookie":     {"x=y"},
		"content-length": {"9"},
		"vary":           {"Accept"},
	})
	if _, present := lower["Set-Cookie"]; present {
		t.Error("lowercase set-cookie survived")
	}
	if _, present := lower["Content-Length"]; present {
		t.Error("lowercase content-length survived")
	}
	if got := lower["Vary"]; len(got) != 1 || got[0] != "Accept" {
		t.Errorf("lowercase vary = %v", got)
	}

	// A Connection token naming Vary must not remove it.
	conn := normalizeResponseHeaders(http.Header{
		"Connection": {"Vary"},
		"Vary":       {"Accept"},
	})
	if got := conn["Vary"]; len(got) != 1 || got[0] != "Accept" {
		t.Errorf("Vary named by Connection = %v, want it kept", got)
	}
}

// A row whose value no longer matches its key cannot be found by any identity
// listing; prune removes it rather than leaving unreachable metadata.
func TestPruneDropsKeyValueMismatch(t *testing.T) {
	dir := t.TempDir()
	seed, err := Open(Config{Dir: dir, Capacity: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	real := syntheticVariant("https://upstream.example/real", "gzip")
	key, err := variantKey(mustNormalizeVariant(t, real))
	if err != nil {
		t.Fatal(err)
	}
	putVariant(t, seed, real)
	other := syntheticVariant("https://upstream.example/other", "gzip")
	value, err := encodeVariant(mustNormalizeVariant(t, other))
	if err != nil {
		t.Fatal(err)
	}
	overwriteVariantValue(t, seed, key, value)

	if got := variantsFor(t, seed, real.URL); len(got) != 0 {
		t.Fatalf("mismatched row was listed: %+v", got)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	s := pruneLocal(t, dir)
	stats, err := s.Prune(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if stats != (PruneStats{}) {
		t.Fatalf("stats = %+v, want zero", stats)
	}
	if got := variantsFor(t, s, real.URL); len(got) != 0 {
		t.Fatalf("mismatched row survived prune: %+v", got)
	}
}

// A stale reference left by a corrupt-row rewrite must not delete a live
// variant that now names a different body.
func TestStaleVariantRefDoesNotDeleteLiveRow(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	oldBody := putBody(t, s, "old body")
	newBody := putBody(t, s, "new body")

	const url = "https://upstream.example/stale-ref"
	v := syntheticVariant(url, "gzip")
	v.BodySHA256 = oldBody.SHA256
	v.BodySize = oldBody.Size
	putVariant(t, s, v)

	key, err := variantKey(mustNormalizeVariant(t, v))
	if err != nil {
		t.Fatal(err)
	}
	overwriteVariantValue(t, s, key, []byte("corrupt"))

	replacement := v
	replacement.BodySHA256 = newBody.SHA256
	replacement.BodySize = newBody.Size
	putVariant(t, s, replacement)

	// The corrupt row was rewritten but its old body reference is still there.
	// Deleting the old body must only drop that stale reference.
	if err := s.Delete(context.Background(), SHA256, oldBody.SHA256); err != nil {
		t.Fatalf("delete old body: %v", err)
	}
	got := variantsFor(t, s, url)
	if len(got) != 1 || got[0].BodySHA256 != newBody.SHA256 {
		t.Fatalf("stale reference deleted the live variant: %+v", got)
	}
}

// A corrupt or unknown-version row is skipped for reads and replaceable or
// deletable by identity. Prune, by contrast, reports it.
func TestVariantCorruptRowsSelfHeal(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	const url = "https://upstream.example/corrupt"
	good1 := syntheticVariant(url, "gzip")
	good2 := syntheticVariant(url, "br")
	bad := syntheticVariant(url, "corrupt")
	putVariant(t, s, good1)
	putVariant(t, s, good2)
	putVariant(t, s, bad)

	badKey, err := variantKey(mustNormalizeVariant(t, bad))
	if err != nil {
		t.Fatal(err)
	}

	// Unknown version: reads skip it.
	overwriteVariantValue(t, s, badKey, []byte{variantEncodingVersion + 1})
	if got := variantsFor(t, s, url); len(got) != 2 {
		t.Fatalf("variants with a corrupt row = %d, want 2", len(got))
	}

	// A same-identity write replaces the corrupt row.
	putVariant(t, s, bad)
	if got := variantsFor(t, s, url); len(got) != 3 {
		t.Fatalf("rewrite did not restore the row: %d", len(got))
	}

	// Truncated garbage: delete removes it.
	overwriteVariantValue(t, s, badKey, []byte("garbage"))
	if err := s.DeleteVariant(context.Background(), bad); err != nil {
		t.Fatalf("delete corrupt: %v", err)
	}
	if got := variantsFor(t, s, url); len(got) != 2 {
		t.Fatalf("delete left %d variants", len(got))
	}

	// Replace treats a corrupt old row as absent and still writes the new one.
	good1Key, err := variantKey(mustNormalizeVariant(t, good1))
	if err != nil {
		t.Fatal(err)
	}
	overwriteVariantValue(t, s, good1Key, []byte{variantEncodingVersion + 1})
	next := syntheticVariant(url, "zstd")
	if err := s.ReplaceVariant(context.Background(), good1, next); err != nil {
		t.Fatalf("replace corrupt old: %v", err)
	}
	got := variantsFor(t, s, url)
	if len(got) != 2 {
		t.Fatalf("after replace: %d variants, want good2 and next", len(got))
	}
	foundBr, foundZstd := false, false
	for _, v := range got {
		if hasVariantEncoding(v, "br") {
			foundBr = true
		}
		if hasVariantEncoding(v, "zstd") {
			foundZstd = true
		}
	}
	if !foundBr || !foundZstd {
		t.Fatalf("variants = %+v, want br and zstd", got)
	}
}

func TestPruneReportsCorruptVariant(t *testing.T) {
	dir := t.TempDir()
	seed, err := Open(Config{Dir: dir, Capacity: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	bad := syntheticVariant("https://upstream.example/prune-corrupt", "gzip")
	putVariant(t, seed, bad)
	key, err := variantKey(mustNormalizeVariant(t, bad))
	if err != nil {
		t.Fatal(err)
	}
	overwriteVariantValue(t, seed, key, []byte{variantEncodingVersion + 1})
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	s := pruneLocal(t, dir)
	stats, err := s.Prune(context.Background())
	if err == nil {
		t.Fatal("prune silently accepted a corrupt variant row")
	}
	if stats.Failed == 0 {
		t.Fatalf("stats = %+v, want a reported failure", stats)
	}

	s.mu.Lock()
	rowExists := false
	dbErr := s.index.db.View(func(tx *bolt.Tx) error {
		rowExists = tx.Bucket(variantsBucket).Get(key) != nil
		return nil
	})
	s.mu.Unlock()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if !rowExists {
		t.Fatal("prune deleted a corrupt row instead of reporting it")
	}
}
