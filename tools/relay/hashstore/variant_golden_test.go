package hashstore

import (
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// The bbolt variant key is a storage format: every relay that still has rows
// written by an older build reads them through this encoding. These bytes were
// produced by the algorithm before the identity was factored into its own
// domain value, and must never change. Each case covers a different part of the
// contract: the single-representation flag, Vary case/order/duplication, merged
// multi-values under differently cased map keys, and absent versus
// present-but-empty.
const (
	goldenNoVaryURL = "https://upstream.example/asset.js"

	goldenNoVaryKey = "0000002168747470733a2f2f757073747265616d2e6578616d706c652f61737365742e6a7301"
	goldenNoVaryVal = "02010000002168747470733a2f2f757073747265616d2e6578616d706c652f61737365742e6a7300000000000000010000000c436f6e74656e742d54797065000000010000000a746578742f706c61696ee55d2e7cbb136374c4590cf2c88e903e77d2b199e50b44e17a7be5f0d41c5fd3000000000000000b17979cfe362a000017979d89e8f37000"

	goldenComplexURL = "https://upstream.example/api/v1/items?page=1"

	goldenComplexKey = "0000002c68747470733a2f2f757073747265616d2e6578616d706c652f6170692f76312f6974656d733f706167653d31000000000200000003782d610100000007312c20322c203300000003782d620100000000"
	goldenComplexVal = "02000000002c68747470733a2f2f757073747265616d2e6578616d706c652f6170692f76312f6974656d733f706167653d310000000200000003782d610100000007312c20322c203300000003782d620100000000000000020000000c436f6e74656e742d5479706500000001000000106170706c69636174696f6e2f6a736f6e00000004566172790000000100000008582d412c20582d42e55d2e7cbb136374c4590cf2c88e903e77d2b199e50b44e17a7be5f0d41c5fd3000000000000000b17979cfe362a007b17979d89e8f37000"

	goldenPresenceURL = "https://upstream.example/presence"

	// X-A present and empty, X-C absent.
	goldenPresentEmptyKey = "0000002168747470733a2f2f757073747265616d2e6578616d706c652f70726573656e6365000000000200000003782d61010000000000000003782d6300"
	goldenPresentEmptyVal = "02000000002168747470733a2f2f757073747265616d2e6578616d706c652f70726573656e63650000000200000003782d61010000000000000003782d630000000000e55d2e7cbb136374c4590cf2c88e903e77d2b199e50b44e17a7be5f0d41c5fd3000000000000000b17979cfe362a000017979d89e8f37000"

	// X-A absent, X-C present and empty.
	goldenAbsentKey = "0000002168747470733a2f2f757073747265616d2e6578616d706c652f70726573656e6365000000000200000003782d610000000003782d630100000000"
	goldenAbsentVal = "02000000002168747470733a2f2f757073747265616d2e6578616d706c652f70726573656e63650000000200000003782d610000000003782d63010000000000000000e55d2e7cbb136374c4590cf2c88e903e77d2b199e50b44e17a7be5f0d41c5fd3000000000000000b17979cfe362a000017979d89e8f37000"
)

// goldenVariantBuilders returns the exact inputs that produced the bytes above.
// They are values, not test helpers over the new code: nothing here calls the
// implementation to derive the expectation.
func goldenVariantBuilders() []struct {
	name string
	v    Variant
	key  string
	val  string
} {
	body := hashOf("golden body")
	base := func(url string, vary []string, headers http.Header, response http.Header) Variant {
		return Variant{
			URL:             url,
			Vary:            vary,
			RequestHeaders:  headers,
			ResponseHeaders: response,
			BodySHA256:      body.SHA256,
			BodySize:        body.Size,
			StoredAt:        time.Unix(1_700_000_000, 0),
			ExpiresAt:       time.Unix(1_700_000_600, 0),
		}
	}

	noVary := base(goldenNoVaryURL, nil, nil, http.Header{"Content-Type": {"text/plain"}})
	noVary.NoVary = true

	complex := base(
		goldenComplexURL,
		[]string{"X-B", "x-a", "X-B", ""},
		http.Header{
			"X-A": {" 1 ", "2"},
			"x-a": {"3"},
			"X-B": {""},
		},
		http.Header{"Content-Type": {"application/json"}, "Vary": {"X-A, X-B"}},
	)
	complex.StoredAt = time.Unix(1_700_000_000, 123)

	presentEmpty := base(
		goldenPresenceURL,
		[]string{"X-A", "X-C"},
		http.Header{"x-a": {""}},
		http.Header{},
	)
	absent := base(
		goldenPresenceURL,
		[]string{"X-A", "X-C"},
		http.Header{"x-c": {""}},
		http.Header{},
	)

	return []struct {
		name string
		v    Variant
		key  string
		val  string
	}{
		{"no-vary", noVary, goldenNoVaryKey, goldenNoVaryVal},
		{"complex", complex, goldenComplexKey, goldenComplexVal},
		{"present-empty", presentEmpty, goldenPresentEmptyKey, goldenPresentEmptyVal},
		{"absent", absent, goldenAbsentKey, goldenAbsentVal},
	}
}

func TestVariantKeyBytesAreFrozen(t *testing.T) {
	for _, tc := range goldenVariantBuilders() {
		t.Run(tc.name, func(t *testing.T) {
			normalized := mustNormalizeVariant(t, tc.v)
			key, err := variantKey(normalized)
			if err != nil {
				t.Fatalf("key: %v", err)
			}
			if got := hex.EncodeToString(key); got != tc.key {
				t.Fatalf("key bytes changed\n got %s\nwant %s", got, tc.key)
			}
			value, err := encodeVariant(normalized)
			if err != nil {
				t.Fatalf("value: %v", err)
			}
			if got := hex.EncodeToString(value); got != tc.val {
				t.Fatalf("value bytes changed\n got %s\nwant %s", got, tc.val)
			}
		})
	}
}

// Raw rows written by an older relay must reopen, list, match and still encode
// back to the same key. The row is inserted as bytes, so this exercises the
// decoder and key codec, not the write path.
func TestVariantRowsFromOlderRelayReopen(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir, Capacity: 1 << 20}
	s := openLocal(t, cfg)

	complexKey := decodeGolden(t, goldenComplexKey)
	complexValue := decodeGolden(t, goldenComplexVal)
	presentKey := decodeGolden(t, goldenPresentEmptyKey)
	presentValue := decodeGolden(t, goldenPresentEmptyVal)
	absentKey := decodeGolden(t, goldenAbsentKey)
	absentValue := decodeGolden(t, goldenAbsentVal)

	s.mu.Lock()
	err := s.index.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(variantsBucket)
		for _, row := range [][2][]byte{
			{complexKey, complexValue},
			{presentKey, presentValue},
			{absentKey, absentValue},
		} {
			if err := bucket.Put(row[0], row[1]); err != nil {
				return err
			}
		}
		return nil
	})
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("insert golden rows: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := openLocal(t, cfg)

	got := variantsFor(t, reopened, goldenComplexURL)
	if len(got) != 1 {
		t.Fatalf("complex rows = %d, want 1", len(got))
	}
	complex := got[0]
	if complex.URL != goldenComplexURL || complex.NoVary {
		t.Fatalf("identity changed on reopen: %+v", complex)
	}
	if want := []string{"x-a", "x-b"}; len(complex.Vary) != len(want) || complex.Vary[0] != want[0] || complex.Vary[1] != want[1] {
		t.Fatalf("Vary = %v, want %v", complex.Vary, want)
	}
	if value, present := complex.Identity().Value("x-a"); !present || value != "1, 2, 3" {
		t.Fatalf("x-a = %q/%v, want the merged value", value, present)
	}
	if value, present := complex.Identity().Value("x-b"); !present || value != "" {
		t.Fatalf("x-b = %q/%v, want present and empty", value, present)
	}
	if !complex.Matches(http.Header{"X-A": {"1", "2", "3"}, "X-B": {""}}) {
		t.Error("reopened row did not match its own request shape")
	}
	if complex.Matches(http.Header{"X-A": {"1", "2", "3"}}) {
		t.Error("present-but-empty header matched a request that omitted it")
	}

	presence := variantsFor(t, reopened, goldenPresenceURL)
	if len(presence) != 2 {
		t.Fatalf("presence rows = %d, want absent and present-empty as two identities", len(presence))
	}
	var absentSeen, emptySeen bool
	for _, v := range presence {
		value, present := v.Identity().Value("x-a")
		switch {
		case !present:
			absentSeen = true
		case value == "":
			emptySeen = true
		}
	}
	if !absentSeen || !emptySeen {
		t.Fatalf("presence lost across reopen: %+v", presence)
	}
}

func decodeGolden(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("decode golden %q: %v", value, err)
	}
	return raw
}
