package server

import (
	"bytes"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// Response disposition: the decision table, the exactly-once observation, body
// ownership and the pre-handoff fallback. These tests count the observation and
// the body reads directly instead of inferring them from registered tasks.

func TestChooseDisposition(t *testing.T) {
	cases := []struct {
		name string
		in   dispositionInput
		want dispositionPlan
	}{
		{
			name: "ordinary store",
			in:   dispositionInput{status: http.StatusOK, writable: true, responseStorable: true},
			want: planOrdinaryStore,
		},
		{
			name: "response no-store with prefetch stays raw",
			in:   dispositionInput{status: http.StatusOK, writable: true, responseStorable: false},
			want: planOrdinaryRaw,
		},
		{
			name: "client no-store stays raw even for a storable response",
			in:   dispositionInput{status: http.StatusOK, writable: false, responseStorable: true},
			want: planOrdinaryRaw,
		},
		{
			name: "client forced revalidation on a cold miss bypasses the store",
			in:   dispositionInput{status: http.StatusNotModified, writable: true, responseStorable: false},
			want: planOrdinaryRaw,
		},
		{
			name: "policy seed is attempted before the ordinary store",
			in:   dispositionInput{status: http.StatusOK, writable: true, responseStorable: true, policySeed: true},
			want: planPolicySeed,
		},
		{
			name: "policy seed that must refuse still plans the seed",
			in:   dispositionInput{status: http.StatusOK, writable: true, responseStorable: false, policySeed: true},
			want: planPolicySeed,
		},
		{
			name: "response seed is attempted before the ordinary store",
			in:   dispositionInput{status: http.StatusOK, writable: true, responseStorable: true, responseSeed: true},
			want: planResponseSeed,
		},
		{
			name: "response seed refused by admission goes to the store",
			in:   dispositionInput{status: http.StatusOK, writable: true, responseStorable: true, responseSeed: false},
			want: planOrdinaryStore,
		},
		{
			name: "old variant bypass with a full 200 stores",
			in:   dispositionInput{status: http.StatusOK, writable: true, responseStorable: true},
			want: planOrdinaryStore,
		},
		{
			name: "old variant bypass with a raw response",
			in:   dispositionInput{status: http.StatusOK, writable: true, responseStorable: false},
			want: planOrdinaryRaw,
		},
		{
			name: "ambiguous cleanup stores",
			in:   dispositionInput{status: http.StatusOK, writable: true, responseStorable: true},
			want: planOrdinaryStore,
		},
		{
			name: "revalidated 304 folds metadata",
			in:   dispositionInput{revalidated: true, status: http.StatusNotModified, foldMetadata: true},
			want: planRevalidatedMetadata,
		},
		{
			name: "revalidated 304 without a stored variant stays raw",
			in:   dispositionInput{revalidated: true, status: http.StatusNotModified, foldMetadata: false},
			want: planOrdinaryRaw,
		},
		{
			name: "revalidated 200 answers the client 304",
			in: dispositionInput{
				revalidated: true, status: http.StatusOK,
				clientPrecondition: preconditionNotModified, writable: true, responseStorable: true,
			},
			want: planClientCondition,
		},
		{
			name: "revalidated 200 answers the client 412",
			in: dispositionInput{
				revalidated: true, status: http.StatusOK,
				clientPrecondition: preconditionFailed, writable: true, responseStorable: true,
			},
			want: planClientCondition,
		},
		{
			name: "revalidated 200 proceeds",
			in: dispositionInput{
				revalidated: true, status: http.StatusOK,
				clientPrecondition: preconditionProceed, writable: true, responseStorable: true,
			},
			want: planOrdinaryStore,
		},
		{
			name: "revalidated 200 without storage",
			in: dispositionInput{
				revalidated: true, status: http.StatusOK,
				clientPrecondition: preconditionProceed, writable: true, responseStorable: false,
			},
			want: planOrdinaryRaw,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseDisposition(tc.in); got != tc.want {
				t.Fatalf("chooseDisposition(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// countingObservation counts observations directly and can forward to the real
// lease table, so a test can assert both the count and a registered task.
type countingObservation struct {
	next responseObservation

	mu          sync.Mutex
	headers     []string
	revalidated []string
}

func (o *countingObservation) observeHeaders(r *http.Request, header http.Header) {
	o.mu.Lock()
	o.headers = append(o.headers, header.Get("ETag"))
	o.mu.Unlock()
	if o.next != nil {
		o.next.observeHeaders(r, header)
	}
}

func (o *countingObservation) observeRevalidated(r *http.Request, key string, v hashstore.Variant, directives RelayDirectives, noStore bool) {
	o.mu.Lock()
	o.revalidated = append(o.revalidated, v.ResponseHeaders.Get("ETag"))
	o.mu.Unlock()
	if o.next != nil {
		o.next.observeRevalidated(r, key, v, directives, noStore)
	}
}

func (o *countingObservation) headerETags() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.headers...)
}

func (o *countingObservation) revalidatedETags() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.revalidated...)
}

func (o *countingObservation) counts() (headers, revalidated int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.headers), len(o.revalidated)
}

// reset drops prior observations so a test can count one exchange at a time.
func (o *countingObservation) reset() {
	o.mu.Lock()
	o.headers = nil
	o.revalidated = nil
	o.mu.Unlock()
}

// dispositionSpyBody counts how many bytes were read and how often the body was
// closed, so body ownership is measured rather than inferred from a nil check.
type dispositionSpyBody struct {
	io.ReadCloser

	mu     sync.Mutex
	reads  int
	closes int
}

func (b *dispositionSpyBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	b.reads += n
	b.mu.Unlock()
	return n, err
}

func (b *dispositionSpyBody) Close() error {
	b.mu.Lock()
	b.closes++
	b.mu.Unlock()
	return b.ReadCloser.Close()
}

func (b *dispositionSpyBody) stats() (reads, closes int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reads, b.closes
}

// dispositionSpyTransport wraps each response body for accounting and may
// rewrite the response before the cache chain sees it.
type dispositionSpyTransport struct {
	base   http.RoundTripper
	mutate func(*http.Response)

	mu     sync.Mutex
	bodies []*dispositionSpyBody
}

func (t *dispositionSpyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if resp == nil || resp.Body == nil {
		return resp, err
	}
	body := &dispositionSpyBody{ReadCloser: resp.Body}
	resp.Body = body
	t.mu.Lock()
	t.bodies = append(t.bodies, body)
	t.mu.Unlock()
	if t.mutate != nil {
		t.mutate(resp)
	}
	return resp, err
}

func (t *dispositionSpyTransport) responseBodies() []*dispositionSpyBody {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*dispositionSpyBody(nil), t.bodies...)
}

func responseImmutablePlainServer(t *testing.T, upstream string, opts cacheServerOptions) (*Server, *hashstore.Store, *countingObservation) {
	t.Helper()
	opts.mirrors = []Mirror{{Name: "files", Path: "files"}}
	s, store := newCacheServer(t, upstream, opts)
	obs := &countingObservation{}
	s.observer = obs
	return s, store, obs
}

// TestDispositionPolicyFallbackObservesOnceAndClosesOnce pins the observation
// fix and body ownership: the active segment refuses the first response, that
// response is observed exactly once, the ordinary chain stores it, and the body
// is closed exactly once.
func TestDispositionPolicyFallbackObservesOnceAndClosesOnce(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control": "max-age=3600",
	}))
	// A 64-byte active budget refuses the 4 KiB body before the adapter exists.
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:        []Mirror{{Name: "docker", Path: "docker"}},
		policies:       activeOCIPolicies(),
		activeCapacity: 64,
	})
	obs := &countingObservation{}
	s.observer = obs
	tracker := &dispositionSpyTransport{base: http.DefaultTransport}
	s.client.Transport = tracker
	target := "/docker/v2/app/blobs/sha256:" + digest

	rec := cacheRequest(s, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("fallback GET = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("fallback upstream = %d, want 1", up.count())
	}
	if headers, revalidated := obs.counts(); headers != 1 || revalidated != 0 {
		t.Fatalf("observations = %d headers / %d revalidated, want 1/0", headers, revalidated)
	}
	if got := len(activeVariants(t, store, "mirror://docker/v2/app/blobs/sha256:"+digest)); got != 1 {
		t.Fatalf("ordinary variants = %d, want 1", got)
	}
	bodies := tracker.responseBodies()
	if len(bodies) != 1 {
		t.Fatalf("upstream responses = %d, want 1", len(bodies))
	}
	if reads, closes := bodies[0].stats(); reads != len(body) || closes != 1 {
		t.Fatalf("body reads=%d closes=%d, want %d reads and exactly one close", reads, closes, len(body))
	}
}

// TestDispositionResponseRefusalObservesOnceAndClosesOnce covers a refusal
// before any handoff: a no-store response that declares immutability and
// prefetch is delivered raw, observed once, stored nowhere, and its body is
// closed exactly once.
func TestDispositionResponseRefusalObservesOnceAndClosesOnce(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control":      "no-store",
		RelayDirectiveHeader: "immutable, prefetch, sha256=" + digest,
	}))
	s, store, obs := responseImmutablePlainServer(t, upSrv.URL, cacheServerOptions{})
	tracker := &dispositionSpyTransport{base: http.DefaultTransport}
	s.client.Transport = tracker

	rec := cacheRequest(s, http.MethodGet, "/files/blob", nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("refused GET = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("upstream = %d, want 1", up.count())
	}
	if headers, revalidated := obs.counts(); headers != 1 || revalidated != 0 {
		t.Fatalf("observations = %d headers / %d revalidated, want 1/0", headers, revalidated)
	}
	if got := len(activeVariants(t, store, "mirror://files/blob")); got != 0 {
		t.Fatalf("refused response left %d variants", got)
	}
	bodies := tracker.responseBodies()
	if len(bodies) != 1 {
		t.Fatalf("upstream responses = %d, want 1", len(bodies))
	}
	if reads, closes := bodies[0].stats(); reads != len(body) || closes != 1 {
		t.Fatalf("body reads=%d closes=%d, want %d reads and exactly one close", reads, closes, len(body))
	}
}

// TestDispositionRevalidation304ObservesOnce keeps a 304 in the metadata shape:
// no body handoff, one revalidated observation, no header observation.
func TestDispositionRevalidation304ObservesOnce(t *testing.T) {
	var conditional int
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			conditional++
			writeResponse(w, r, http.StatusNotModified, "", map[string]string{
				"ETag":          `"v1"`,
				"Cache-Control": "max-age=600",
			})
			return
		}
		writeResponse(w, r, http.StatusOK, "one", map[string]string{
			"ETag":          `"v1"`,
			"Cache-Control": "max-age=3600",
		})
	})
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	obs := &countingObservation{}
	s.observer = obs

	if rec := cacheRequest(s, http.MethodGet, "/test/rev", nil); rec.Code != http.StatusOK {
		t.Fatalf("first GET = %d", rec.Code)
	}
	tracker := &dispositionSpyTransport{base: http.DefaultTransport}
	s.client.Transport = tracker
	obs.reset()
	rec := cacheRequest(s, http.MethodGet, "/test/rev", map[string]string{"Cache-Control": "no-cache"})
	if rec.Code != http.StatusOK || rec.Body.String() != "one" {
		t.Fatalf("revalidated = %d %q", rec.Code, rec.Body.String())
	}
	if up.count() != 2 || conditional != 1 {
		t.Fatalf("upstream = %d conditional = %d, want 2/1", up.count(), conditional)
	}
	if headers, revalidated := obs.counts(); headers != 0 || revalidated != 1 {
		t.Fatalf("observations = %d headers / %d revalidated, want 0/1", headers, revalidated)
	}
	bodies := tracker.responseBodies()
	if len(bodies) != 1 {
		t.Fatalf("revalidation responses = %d, want 1", len(bodies))
	}
	if reads, closes := bodies[0].stats(); reads != 0 || closes != 1 {
		t.Fatalf("304 body reads=%d closes=%d, want 0 reads and exactly one close", reads, closes)
	}
	if got := firstVariant(t, store, "mirror://test/rev").ResponseHeaders.Get("ETag"); got != `"v1"` {
		t.Fatalf("variant ETag = %q, want the folded 304", got)
	}
}

// TestDispositionRevalidation200ClientConditionObservesOnce pins the other half:
// the fetched 200 is observed once even though the client's own condition
// short-circuits it to a 304, and no new body is stored.
func TestDispositionRevalidation200ClientConditionObservesOnce(t *testing.T) {
	var mu sync.Mutex
	body, etag := "one", `"1"`
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		currentBody, currentETag := body, etag
		mu.Unlock()
		writeResponse(w, r, http.StatusOK, currentBody, map[string]string{
			"ETag":          currentETag,
			"Cache-Control": "max-age=0",
		})
	})
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	obs := &countingObservation{}
	s.observer = obs

	if rec := cacheRequest(s, http.MethodGet, "/test/rev", nil); rec.Code != http.StatusOK {
		t.Fatalf("first GET = %d", rec.Code)
	}
	mu.Lock()
	body, etag = "two", `"2"`
	mu.Unlock()
	tracker := &dispositionSpyTransport{base: http.DefaultTransport}
	s.client.Transport = tracker
	obs.reset()

	rec := cacheRequest(s, http.MethodGet, "/test/rev", map[string]string{"If-None-Match": `"2"`})
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("conditional revalidation = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if up.count() != 2 {
		t.Fatalf("upstream = %d, want 2", up.count())
	}
	if headers, revalidated := obs.counts(); headers != 1 || revalidated != 0 {
		t.Fatalf("observations = %d headers / %d revalidated, want 1/0", headers, revalidated)
	}
	bodies := tracker.responseBodies()
	if len(bodies) != 1 {
		t.Fatalf("revalidation responses = %d, want 1", len(bodies))
	}
	if reads, closes := bodies[0].stats(); reads != 0 || closes != 1 {
		t.Fatalf("short-circuited 200 body reads=%d closes=%d, want 0 reads and exactly one close", reads, closes)
	}
	// The fetched 200 was not stored: the stale variant still names the old
	// representation and the next request revalidates again.
	v := firstVariant(t, store, "mirror://test/rev")
	if got := v.ResponseHeaders.Get("ETag"); got != `"1"` {
		t.Fatalf("stored ETag = %q, want the unchanged stale variant", got)
	}
	if again := cacheRequest(s, http.MethodGet, "/test/rev", nil); again.Body.String() != "two" || up.count() != 3 {
		t.Fatalf("post-304 GET = %q upstream %d, want the stale variant kept", again.Body.String(), up.count())
	}
}

// TestDispositionPreHandoffFallbackDeliversAndClosesOnce proves a refusal before
// the adapter leaves the body deliverable: it is read exactly once by the
// ordinary chain, the response is fetched once, and the body is closed exactly
// once.
func TestDispositionPreHandoffFallbackDeliversAndClosesOnce(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control":      "max-age=3600",
		RelayDirectiveHeader: "immutable, sha256=" + digest,
	}))
	// A 64-byte active budget refuses the body at admission, before the adapter.
	s, store, obs := responseImmutablePlainServer(t, upSrv.URL, cacheServerOptions{activeCapacity: 64})
	tracker := &dispositionSpyTransport{base: http.DefaultTransport}
	s.client.Transport = tracker

	rec := cacheRequest(s, http.MethodGet, "/files/blob", nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("refused GET = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if up.count() != 1 {
		t.Fatalf("upstream = %d, want 1", up.count())
	}
	if headers, revalidated := obs.counts(); headers != 1 || revalidated != 0 {
		t.Fatalf("observations = %d/0 headers, want 1/0", headers)
	}
	bodies := tracker.responseBodies()
	if len(bodies) != 1 {
		t.Fatalf("upstream responses = %d, want 1", len(bodies))
	}
	reads, closes := bodies[0].stats()
	if reads != len(body) || closes != 1 {
		t.Fatalf("refused body reads=%d closes=%d, want %d reads and exactly one close", reads, closes, len(body))
	}
	if got := len(activeVariants(t, store, "mirror://files/blob")); got != 1 {
		t.Fatalf("ordinary variants = %d, want 1", got)
	}
}

// TestDispositionPostHandoffPolicyStartsFreshExchange pins the policy-path
// failure difference: when the adapter took the body and could not start, the
// caller starts a new exchange with its own disposition. The first body is never
// replayed and the client sees the new response.
func TestDispositionPostHandoffPolicyStartsFreshExchange(t *testing.T) {
	seedBody := activeBody()
	ordinaryBody := bytes.ToUpper(seedBody)
	digest := bodyDigestHex(string(seedBody))

	var calls int
	var mu sync.Mutex
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		body := ordinaryBody
		etag := `"ordinary"`
		if n == 1 {
			body, etag = seedBody, `"seed"`
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Cache-Control", "max-age=3600")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
		mirrors:  []Mirror{{Name: "docker", Path: "docker"}},
		policies: activeOCIPolicies(),
	})
	obs := &countingObservation{}
	s.observer = obs
	tracker := &dispositionSpyTransport{
		base: http.DefaultTransport,
		// A URL the adapter rejects after it has taken ownership of the body.
		mutate: func(resp *http.Response) {
			resp.Request = &http.Request{
				URL:    &url.URL{Scheme: "ftp", Host: "invalid"},
				Header: make(http.Header),
			}
		},
	}
	s.client.Transport = tracker
	target := "/docker/v2/app/blobs/sha256:" + digest

	rec := cacheRequest(s, http.MethodGet, target, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), ordinaryBody) {
		t.Fatalf("post-handoff fallback = %d, %d bytes (want the second response)", rec.Code, rec.Body.Len())
	}
	if up.count() != 2 {
		t.Fatalf("upstream = %d, want one seed plus one fresh exchange", up.count())
	}
	if got := obs.headerETags(); len(got) != 2 || got[0] != `"seed"` || got[1] != `"ordinary"` {
		t.Fatalf("observations = %v, want one per response [seed ordinary]", got)
	}
	bodies := tracker.responseBodies()
	if len(bodies) != 2 {
		t.Fatalf("upstream responses = %d, want 2", len(bodies))
	}
	if reads, closes := bodies[0].stats(); reads != 0 || closes != 1 {
		t.Fatalf("seed body reads=%d closes=%d, want 0 reads and exactly one close", reads, closes)
	}
	if reads, closes := bodies[1].stats(); reads != len(ordinaryBody) || closes != 1 {
		t.Fatalf("fresh body reads=%d closes=%d, want %d reads and exactly one close", reads, closes, len(ordinaryBody))
	}
	v := firstVariant(t, store, "mirror://docker/v2/app/blobs/sha256:"+digest)
	if v.BodySHA256 != bodyDigestHex(string(ordinaryBody)) {
		t.Fatalf("stored body = %q, want the fresh exchange", v.BodySHA256)
	}
}

// TestDispositionPostHandoffResponseSeedAnswers502 keeps the response-declared
// difference: the body left the ordinary chain, so the request answers 502
// instead of asking upstream again.
func TestDispositionPostHandoffResponseSeedAnswers502(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
		"Cache-Control":      "max-age=3600",
		RelayDirectiveHeader: "immutable, sha256=" + digest,
	}))
	s, store, obs := responseImmutablePlainServer(t, upSrv.URL, cacheServerOptions{})
	tracker := &dispositionSpyTransport{
		base: http.DefaultTransport,
		mutate: func(resp *http.Response) {
			resp.Request = &http.Request{
				URL:    &url.URL{Scheme: "ftp", Host: "invalid"},
				Header: make(http.Header),
			}
		},
	}
	s.client.Transport = tracker

	rec := cacheRequest(s, http.MethodGet, "/files/blob", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("post-handoff response seed = %d, want 502", rec.Code)
	}
	if up.count() != 1 {
		t.Fatalf("upstream = %d, want 1", up.count())
	}
	if headers, revalidated := obs.counts(); headers != 1 || revalidated != 0 {
		t.Fatalf("observations = %d/0 headers, want 1/0", headers)
	}
	bodies := tracker.responseBodies()
	if len(bodies) != 1 {
		t.Fatalf("upstream responses = %d, want 1", len(bodies))
	}
	if reads, closes := bodies[0].stats(); reads != 0 || closes != 1 {
		t.Fatalf("seed body reads=%d closes=%d, want 0 reads and exactly one close", reads, closes)
	}
	if got := len(activeVariants(t, store, "mirror://files/blob")); got != 0 {
		t.Fatalf("unusable response left %d variants", got)
	}
}

// TestDispositionLowDiskRefusal covers both admission doors: a low watermark
// refuses the active segment, the already-fetched response is delivered once,
// and the ordinary store still accepts it.
func TestDispositionLowDiskRefusal(t *testing.T) {
	body := activeBody()
	digest := bodyDigestHex(string(body))
	// A watermark above any real free space refuses every active admission.
	const lowDisk = int64(math.MaxInt64)

	t.Run("policy seed", func(t *testing.T) {
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			"Cache-Control": "max-age=3600",
		}))
		s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{
			mirrors:             []Mirror{{Name: "docker", Path: "docker"}},
			policies:            activeOCIPolicies(),
			activeDiskWaterMark: lowDisk,
		})
		obs := &countingObservation{}
		s.observer = obs
		target := "/docker/v2/app/blobs/sha256:" + digest

		rec := cacheRequest(s, http.MethodGet, target, nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("low-disk GET = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if up.count() != 1 {
			t.Fatalf("upstream = %d, want 1", up.count())
		}
		if headers, _ := obs.counts(); headers != 1 {
			t.Fatalf("observations = %d, want 1", headers)
		}
		if got := len(activeVariants(t, store, "mirror://docker/v2/app/blobs/sha256:"+digest)); got != 1 {
			t.Fatalf("ordinary variants = %d, want 1", got)
		}
	})

	t.Run("response seed", func(t *testing.T) {
		up, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			"Cache-Control":      "max-age=3600",
			RelayDirectiveHeader: "immutable, sha256=" + digest,
		}))
		s, store, obs := responseImmutablePlainServer(t, upSrv.URL, cacheServerOptions{activeDiskWaterMark: lowDisk})

		rec := cacheRequest(s, http.MethodGet, "/files/blob", nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("low-disk GET = %d, %d bytes", rec.Code, rec.Body.Len())
		}
		if up.count() != 1 {
			t.Fatalf("upstream = %d, want 1", up.count())
		}
		if headers, _ := obs.counts(); headers != 1 {
			t.Fatalf("observations = %d, want 1", headers)
		}
		if got := len(activeVariants(t, store, "mirror://files/blob")); got != 1 {
			t.Fatalf("ordinary variants = %d, want 1", got)
		}
	})
}

// TestAPTRevalidation304FormsOrdinaryLease characterizes the preserved current
// behaviour: an APT Release revalidated with a 304 feeds the ordinary lease
// table (ObserveNormalVariant), not the per-mirror index task. Changing that to
// an APT index lease is a separate decision.
func TestAPTRevalidation304FormsOrdinaryLease(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"r1"` {
			w.Header().Set("ETag", `"r1"`)
			w.Header().Set("Cache-Control", "max-age=600")
			w.Header().Set(RelayDirectiveHeader, "prefetch")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"r1"`)
		w.Header().Set("Cache-Control", "max-age=0")
		w.Header().Set(RelayDirectiveHeader, "prefetch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("release"))
	})
	clock := newTestClock()
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		clock:   clock,
		mirrors: []Mirror{aptTestMirror()},
	})
	disablePrefetchJitter(s)

	path := "/debian/dists/bookworm/Release"
	if rec := cacheRequest(s, http.MethodGet, path, nil); rec.Code != http.StatusOK {
		t.Fatalf("first Release GET = %d", rec.Code)
	}
	// The full response taught the per-mirror index task.
	s.prefetch.mu.Lock()
	aptTask := s.prefetch.tasks[aptTaskKey("debian")]
	s.prefetch.mu.Unlock()
	if aptTask == nil || aptTask.kind != prefetchKindAPT {
		t.Fatalf("apt task after the full Release = %+v, want the index task", aptTask)
	}

	rec := cacheRequest(s, http.MethodGet, path, map[string]string{"Cache-Control": "no-cache"})
	if rec.Code != http.StatusOK || rec.Body.String() != "release" {
		t.Fatalf("revalidated Release = %d %q", rec.Code, rec.Body.String())
	}
	if up.count() != 2 {
		t.Fatalf("upstream = %d, want 2", up.count())
	}

	s.prefetch.mu.Lock()
	var normal *prefetchTask
	for _, task := range s.prefetch.tasks {
		if task.kind == prefetchKindNormal {
			copy := *task
			normal = &copy
		}
	}
	s.prefetch.mu.Unlock()
	if normal == nil {
		t.Fatal("the 304 did not form an ordinary lease")
	}
	if normal.cacheKey != "mirror://debian/dists/bookworm/Release" {
		t.Fatalf("ordinary lease key = %q", normal.cacheKey)
	}
	if normal.mirror.Name != "debian" {
		t.Fatalf("ordinary lease mirror = %q", normal.mirror.Name)
	}
}

// TestDispositionColdMiss304IsNotFolded keeps a 304 that answers a cold MISS
// out of the metadata path: it is the client's response, stores nothing, and is
// observed once from its headers.
func TestDispositionColdMiss304IsNotFolded(t *testing.T) {
	up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"x"` {
			w.Header().Set("ETag", `"x"`)
			w.Header().Set("Cache-Control", "max-age=3600")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeResponse(w, r, http.StatusOK, "body", map[string]string{
			"ETag":          `"x"`,
			"Cache-Control": "max-age=3600",
		})
	})
	s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	obs := &countingObservation{}
	s.observer = obs

	rec := cacheRequest(s, http.MethodGet, "/test/cold304", map[string]string{"If-None-Match": `"x"`})
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("cold 304 = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if headers, revalidated := obs.counts(); headers != 1 || revalidated != 0 {
		t.Fatalf("observations = %d headers / %d revalidated, want 1/0", headers, revalidated)
	}
	if got := len(activeVariants(t, store, "mirror://test/cold304")); got != 0 {
		t.Fatalf("cold 304 left %d variants", got)
	}
	if next := cacheRequest(s, http.MethodGet, "/test/cold304", nil); next.Code != http.StatusOK || next.Body.String() != "body" {
		t.Fatalf("next GET = %d %q", next.Code, next.Body.String())
	}
	if up.count() != 2 {
		t.Fatalf("upstream = %d, want 2", up.count())
	}
}

// TestDispositionStaleRangeReplacement keeps the stale-ranged rule: a full 200
// replaces the old variant, while a 206 leaves it in place.
func TestDispositionStaleRangeReplacement(t *testing.T) {
	const first = "0123456789"
	const second = "abcdefghij"

	t.Run("full 200 replaces", func(t *testing.T) {
		var calls int
		var mu sync.Mutex
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				writeResponse(w, r, http.StatusOK, first, map[string]string{
					"ETag":          `"1"`,
					"Cache-Control": "max-age=0",
				})
				return
			}
			// The origin ignores Range and returns the whole new representation.
			writeResponse(w, r, http.StatusOK, second, map[string]string{
				"ETag":          `"2"`,
				"Cache-Control": "max-age=3600",
			})
		})
		s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
		key := "mirror://test/stale-range"

		cacheRequest(s, http.MethodGet, "/test/stale-range", nil)
		rec := cacheRequest(s, http.MethodGet, "/test/stale-range", map[string]string{"Range": "bytes=0-1"})
		if rec.Code != http.StatusOK || rec.Body.String() != second {
			t.Fatalf("ignored Range = %d %q", rec.Code, rec.Body.String())
		}
		if up.count() != 2 {
			t.Fatalf("upstream = %d, want 2", up.count())
		}
		v := firstVariant(t, store, key)
		if v.BodySHA256 != bodyDigestHex(second) {
			t.Fatalf("variant body = %q, want the replacing 200", v.BodySHA256)
		}
	})

	t.Run("206 keeps", func(t *testing.T) {
		var calls int
		var mu sync.Mutex
		up, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				writeResponse(w, r, http.StatusOK, first, map[string]string{
					"ETag":          `"1"`,
					"Cache-Control": "max-age=0",
				})
				return
			}
			w.Header().Set("Content-Range", "bytes 0-1/10")
			writeResponse(w, r, http.StatusPartialContent, first[:2], nil)
		})
		s, store := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
		key := "mirror://test/stale-range"

		cacheRequest(s, http.MethodGet, "/test/stale-range", nil)
		rec := cacheRequest(s, http.MethodGet, "/test/stale-range", map[string]string{"Range": "bytes=0-1"})
		if rec.Code != http.StatusPartialContent || rec.Body.String() != first[:2] {
			t.Fatalf("206 = %d %q", rec.Code, rec.Body.String())
		}
		if up.count() != 2 {
			t.Fatalf("upstream = %d, want 2", up.count())
		}
		v := firstVariant(t, store, key)
		if v.BodySHA256 != bodyDigestHex(first) {
			t.Fatalf("206 replaced the old variant: %q", v.BodySHA256)
		}
	})
}

// TestDispositionWithoutResponseOrServerAnswersError covers the two defensive
// early exits: both answer an explicit 502 instead of reporting a delivery that
// never happened, and a body that exists is still released.
func TestDispositionWithoutResponseOrServerAnswersError(t *testing.T) {
	t.Run("nil response", func(t *testing.T) {
		rec := httptest.NewRecorder()
		out := dispositioner{}.settle(rec, httptest.NewRequest(http.MethodGet, "/x", nil), dispositionRequest{})
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", rec.Code)
		}
		if out != settleDone {
			t.Fatalf("outcome = %v, want settleDone after the explicit error", out)
		}
	})

	t.Run("nil server releases the body", func(t *testing.T) {
		body := &dispositionSpyBody{ReadCloser: io.NopCloser(bytes.NewReader([]byte("x")))}
		resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
		rec := httptest.NewRecorder()
		out := dispositioner{}.settle(rec, httptest.NewRequest(http.MethodGet, "/x", nil), dispositionRequest{resp: resp})
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", rec.Code)
		}
		if out != settleDone {
			t.Fatalf("outcome = %v, want settleDone after the explicit error", out)
		}
		if reads, closes := body.stats(); reads != 0 || closes != 1 {
			t.Fatalf("body reads=%d closes=%d, want 0 reads and exactly one close", reads, closes)
		}
	})
}

// TestDispositionRevalidated304WithoutVariantStaysRaw guards the invariant that
// the metadata fold needs a stored variant: a 304 with no variant is delivered
// as the client's response instead of panicking on a nil fold source.
func TestDispositionRevalidated304WithoutVariantStaysRaw(t *testing.T) {
	_, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
	body := &dispositionSpyBody{ReadCloser: io.NopCloser(bytes.NewReader(nil))}
	resp := &http.Response{
		StatusCode: http.StatusNotModified,
		Header:     http.Header{"ETag": {`"x"`}},
		Body:       body,
	}
	rec := httptest.NewRecorder()
	out := s.disposition().settle(rec, httptest.NewRequest(http.MethodGet, "http://relay.example.com/test/x", nil), dispositionRequest{
		key: "mirror://test/x", path: "/x", resp: resp, revalidated: true,
	})
	if out != settleDone {
		t.Fatalf("outcome = %v, want settleDone", out)
	}
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
	if reads, closes := body.stats(); reads != 0 || closes != 1 {
		t.Fatalf("304 body reads=%d closes=%d, want 0 reads and exactly one close", reads, closes)
	}
}

// TestDispositionBodyClosesExactlyOnce pins the ownership rule on the two
// successful delivery paths: the ordinary store and an active assembly each read
// the body once and close it exactly once.
func TestDispositionBodyClosesExactlyOnce(t *testing.T) {
	t.Run("ordinary store", func(t *testing.T) {
		body := activeBody()
		_, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			"Cache-Control": "max-age=3600",
		}))
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{mirrors: []Mirror{testMirror()}})
		tracker := &dispositionSpyTransport{base: http.DefaultTransport}
		s.client.Transport = tracker

		if rec := cacheRequest(s, http.MethodGet, "/test/body", nil); rec.Code != http.StatusOK {
			t.Fatalf("GET = %d", rec.Code)
		}
		bodies := tracker.responseBodies()
		if len(bodies) != 1 {
			t.Fatalf("upstream responses = %d, want 1", len(bodies))
		}
		if reads, closes := bodies[0].stats(); reads != len(body) || closes != 1 {
			t.Fatalf("body reads=%d closes=%d, want %d reads and exactly one close", reads, closes, len(body))
		}
	})

	t.Run("active assembly", func(t *testing.T) {
		body := activeBody()
		digest := bodyDigestHex(string(body))
		_, upSrv := startUpstream(t, activeUpstreamHandler(body, map[string]string{
			"Cache-Control":      "max-age=3600",
			RelayDirectiveHeader: "immutable, sha256=" + digest,
		}))
		s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
			mirrors: []Mirror{{Name: "files", Path: "files"}},
		})
		tracker := &dispositionSpyTransport{base: http.DefaultTransport}
		s.client.Transport = tracker

		if rec := cacheRequest(s, http.MethodGet, "/files/blob", nil); rec.Code != http.StatusOK {
			t.Fatalf("GET = %d", rec.Code)
		}
		bodies := tracker.responseBodies()
		if len(bodies) != 1 {
			t.Fatalf("upstream responses = %d, want 1", len(bodies))
		}
		if reads, closes := bodies[0].stats(); reads != len(body) || closes != 1 {
			t.Fatalf("body reads=%d closes=%d, want %d reads and exactly one close", reads, closes, len(body))
		}
	})
}
