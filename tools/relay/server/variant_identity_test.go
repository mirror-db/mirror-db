package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// An ambiguous refetch rewrites a URL down to one identity. A stored variant
// whose Vary header was absent must not be mistaken for the freshly built
// present-but-empty one: they are different identities, so leaving the old row
// behind would resurrect a representation the refetch just replaced.
func TestCacheAmbiguityCleanupDistinguishesEmptyHeader(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "fresh", map[string]string{
			"Cache-Control": "max-age=3600",
			"Vary":          "X-A",
		})
	})
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	key := "mirror://test/presence"
	now := s.currentTime()

	seed := func(vary []string, headers http.Header) {
		t.Helper()
		content, err := store.Put(context.Background(), strings.NewReader("old"), 3)
		if err != nil {
			t.Fatalf("put body: %v", err)
		}
		v := hashstore.Variant{
			URL:             key,
			Vary:            vary,
			RequestHeaders:  headers,
			ResponseHeaders: http.Header{"Cache-Control": {"max-age=3600"}},
			BodySHA256:      content.SHA256,
			BodySize:        content.Size,
			StoredAt:        now,
			ExpiresAt:       now.Add(time.Hour),
		}
		if err := store.PutVariant(context.Background(), v); err != nil {
			t.Fatalf("put variant %v/%v: %v", vary, headers, err)
		}
	}
	seed([]string{"x-a"}, nil)                                           // header absent
	seed([]string{"x-a"}, http.Header{"x-a": {""}})                      // header present and empty
	seed([]string{"x-a", "x-b"}, http.Header{"x-a": {""}, "x-b": {"1"}}) // second match

	// The request matches two stored variants, so the server re-fetches and
	// then rewrites the URL to the identity the response produced.
	rec := cacheRequest(s, http.MethodGet, "/test/presence", map[string]string{"X-A": "", "X-B": "1"})
	if rec.Code != http.StatusOK || rec.Body.String() != "fresh" {
		t.Fatalf("ambiguous response = %d %q", rec.Code, rec.Body.String())
	}
	if up.count() != 1 {
		t.Fatalf("ambiguous request did not refetch: %d", up.count())
	}

	variants, err := store.Variants(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 1 {
		t.Fatalf("cleanup left %d variants, want exactly one", len(variants))
	}
	value, present := variants[0].Identity().Value("x-a")
	if !present || value != "" {
		t.Fatalf("kept identity = %q/%v, want X-A present and empty", value, present)
	}
	if variants[0].NoVary {
		t.Fatal("cleanup folded a varied identity into the single-representation case")
	}
}

// Build, lookup, equality and the prefetch task key all consume one identity.
// The relay excludes Accept-Encoding from all of them, while the stored row
// still records the normalised Vary set the response actually stated.
func TestRelayVariantIdentityIsOneDefinition(t *testing.T) {
	_, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeResponse(w, r, http.StatusOK, "payload", nil)
	})
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	_ = s
	ctx := context.Background()
	key := "mirror://test/identity"
	now := time.Unix(1_700_000_000, 0)

	content, err := store.Put(ctx, strings.NewReader("payload"), 7)
	if err != nil {
		t.Fatal(err)
	}
	storedHeaders := http.Header{"Vary": {"X-Lang, Accept-Encoding"}}
	requestHeaders := http.Header{"X-Lang": {" en "}, "Accept-Encoding": {"gzip"}}

	built := buildVariant(key, storedHeaders, RelayDirectives{}, requestHeaders, now, now.Add(time.Hour), content)
	if built.NoVary || strings.Join(built.Vary, ",") != "x-lang" {
		t.Fatalf("built identity = NoVary=%v Vary=%v, want only x-lang", built.NoVary, built.Vary)
	}
	if _, present := built.Identity().Value("accept-encoding"); present {
		t.Fatal("build filed Accept-Encoding as a dimension")
	}
	if value, present := built.Identity().Value("x-lang"); !present || value != "en" {
		t.Fatalf("built x-lang = %q/%v, want en", value, present)
	}

	if err := store.PutVariant(ctx, built); err != nil {
		t.Fatal(err)
	}
	listed, err := store.Variants(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("stored variants = %d, want 1", len(listed))
	}
	stored := relayStoredIdentity(listed[0])
	want := relayResponseIdentity(false, storedHeaders.Values("Vary"), requestHeaders)
	if !stored.Equal(want) {
		t.Fatalf("lookup identity %v/%v disagrees with build identity %v/%v",
			stored.Vary(), stored.RequestHeaders(), want.Vary(), want.RequestHeaders())
	}
	if got, wantKey := normalTaskKey(key, stored), normalTaskKey(key, want); got != wantKey {
		t.Fatalf("task key disagrees with the stored identity:\n got %q\nwant %q", got, wantKey)
	}
	if got, wantHeaders := listed[0].RequestHeaders, want.RequestHeaders(); len(got) != len(wantHeaders) {
		t.Fatalf("replay headers = %v, want %v", got, wantHeaders)
	} else {
		for name, values := range wantHeaders {
			if len(got[name]) != len(values) || got[name][0] != values[0] {
				t.Fatalf("replay header %s = %v, want %v", name, got[name], values)
			}
		}
	}

	// Lookup ignores a different coding but still distinguishes a missing
	// varied header.
	if got := matchingVariants(listed, http.Header{"X-Lang": {"en"}, "Accept-Encoding": {"br"}}); len(got) != 1 {
		t.Fatalf("lookup by a different coding matched %d variants, want 1", len(got))
	}
	if got := matchingVariants(listed, http.Header{"Accept-Encoding": {"br"}}); len(got) != 0 {
		t.Fatalf("lookup without X-Lang matched %d variants, want 0", len(got))
	}
	if got := matchingVariants(listed, http.Header{"X-Lang": {""}, "Accept-Encoding": {"br"}}); len(got) != 0 {
		t.Fatalf("lookup with an empty X-Lang matched %d variants, want 0", len(got))
	}
}
