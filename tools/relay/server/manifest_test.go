package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

func newSyncTestServer(t *testing.T, h http.HandlerFunc) *Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test upstream: %v", err)
	}
	return &Server{upstream: u, client: srv.Client()}
}

const (
	manifestV1 = `{
		"mirrors": [
			{"name": "debian", "path": "debian", "keepHTTP": true, "policy_tags": ["apt"]},
			{"name": "docker", "host": "dcr", "policy_tags": ["oci"]}
		],
		"policies": [
			{"name": "apt-pool", "match": {"tag": "apt", "path_prefix": "/pool/"}, "rules": {"traffic_policy": "speed"}},
			{"name": "default", "match": {}, "rules": {"traffic_policy": "latency"}}
		]
	}`

	manifestV2 = `{
		"mirrors": [
			{"name": "debian", "path": "debian", "policy_tags": ["apt"]},
			{"name": "docker", "host": "dcr", "policy_tags": ["oci"]},
			{"name": "npm", "path": "npm"}
		],
		"policies": [
			{"name": "default", "match": {}, "rules": {"traffic_policy": "speed"}}
		]
	}`
)

// manifestUpstream is a fake /api/mirrors origin with the ETag/HEAD behaviour a
// relay has to cope with. The mutex keeps the race detector honest: the handler
// runs on the server's goroutine.
type manifestUpstream struct {
	mu          sync.Mutex
	etag        string
	body        string
	failHead    bool
	failGet     bool
	always200   bool // HEAD answers 200 with the current ETag instead of 304
	gets        int
	heads       int
	conditional string
}

func (u *manifestUpstream) set(etag, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.etag, u.body = etag, body
}

func (u *manifestUpstream) setAlways200(v bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.always200 = v
}

func (u *manifestUpstream) setFailHead(fail bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.failHead = fail
}

func (u *manifestUpstream) counts() (gets, heads int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.gets, u.heads
}

func (u *manifestUpstream) lastConditional() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.conditional
}

func (u *manifestUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()

		switch r.Method {
		case http.MethodGet:
			u.gets++
			if u.failGet {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			if u.etag != "" {
				w.Header().Set("ETag", u.etag)
			}
			fmt.Fprint(w, u.body)
		case http.MethodHead:
			u.heads++
			u.conditional = r.Header.Get("If-None-Match")
			if u.failHead {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			if u.etag == "" {
				// 200 without a validator: the relay must fall back to GET.
				return
			}
			if u.conditional == u.etag && !u.always200 {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", u.etag)
		}
	}
}

// The first load is a GET; the ETag it carries is kept for later revalidation.
func TestManifestFirstLoadIsGET(t *testing.T) {
	up := &manifestUpstream{etag: `"v1"`, body: manifestV1}
	s := newSyncTestServer(t, up.handler())

	if err := s.syncManifest(); err != nil {
		t.Fatalf("syncManifest: %v", err)
	}
	if gets, heads := up.counts(); gets != 1 || heads != 0 {
		t.Fatalf("gets=%d heads=%d, want 1/0", gets, heads)
	}
	if !s.ready.Load() || !s.loaded.Load() {
		t.Fatal("manifest not marked ready/loaded")
	}

	st := s.manifest.Load()
	if st == nil {
		t.Fatal("no snapshot")
	}
	if st.etag != `"v1"` {
		t.Fatalf("etag %q", st.etag)
	}
	if len(st.mirrors) != 2 || len(st.policies) != 2 {
		t.Fatalf("mirrors=%d policies=%d", len(st.mirrors), len(st.policies))
	}
	if _, ok := st.keepHTTP["debian"]; !ok {
		t.Errorf("keepHTTP missing path mirror: %v", st.keepHTTP)
	}
	if got := st.policyFor(Mirror{Name: "debian", PolicyTags: []string{"apt"}}, "/pool/x.deb"); got != trafficSpeed {
		t.Errorf("policyFor = %q, want speed", got)
	}
}

// An unchanged manifest costs one conditional HEAD and no body download.
func TestManifestConditionalHEADNotModified(t *testing.T) {
	up := &manifestUpstream{etag: `"v1"`, body: manifestV1}
	s := newSyncTestServer(t, up.handler())

	if err := s.syncManifest(); err != nil {
		t.Fatal(err)
	}
	before := s.manifest.Load()

	if err := s.syncManifest(); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if gets, heads := up.counts(); gets != 1 || heads != 1 {
		t.Fatalf("gets=%d heads=%d, want 1/1", gets, heads)
	}
	if got := up.lastConditional(); got != `"v1"` {
		t.Fatalf("If-None-Match = %q, want %q", got, `"v1"`)
	}
	if s.manifest.Load() != before {
		t.Fatal("304 replaced the snapshot")
	}
}

// HEAD may answer 200 with the same ETag instead of 304; still no GET.
func TestManifestHEAD200SameETagSkipsGET(t *testing.T) {
	up := &manifestUpstream{etag: `"v1"`, body: manifestV1, always200: true}
	s := newSyncTestServer(t, up.handler())

	if err := s.syncManifest(); err != nil {
		t.Fatal(err)
	}
	before := s.manifest.Load()

	if err := s.syncManifest(); err != nil {
		t.Fatal(err)
	}
	if gets, heads := up.counts(); gets != 1 || heads != 1 {
		t.Fatalf("gets=%d heads=%d, want 1/1", gets, heads)
	}
	if s.manifest.Load() != before {
		t.Fatal("snapshot replaced by an unchanged manifest")
	}
}

// A changed ETag is followed by a GET and an atomic replacement.
func TestManifestChangedETagTriggersGET(t *testing.T) {
	up := &manifestUpstream{etag: `"v1"`, body: manifestV1}
	s := newSyncTestServer(t, up.handler())

	if err := s.syncManifest(); err != nil {
		t.Fatal(err)
	}

	up.set(`"v2"`, manifestV2)

	if err := s.syncManifest(); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if gets, heads := up.counts(); gets != 2 || heads != 1 {
		t.Fatalf("gets=%d heads=%d, want 2/1", gets, heads)
	}

	st := s.manifest.Load()
	if st.etag != `"v2"` {
		t.Fatalf("etag = %q", st.etag)
	}
	if len(st.mirrors) != 3 {
		t.Fatalf("mirrors = %d, want 3", len(st.mirrors))
	}
	if got := st.policyFor(Mirror{Name: "npm"}, "/x"); got != trafficSpeed {
		t.Fatalf("new policies not active: %q", got)
	}
}

// A failed HEAD leaves the last known good snapshot untouched.
func TestManifestHEADFailureKeepsSnapshot(t *testing.T) {
	up := &manifestUpstream{etag: `"v1"`, body: manifestV1}
	s := newSyncTestServer(t, up.handler())

	if err := s.syncManifest(); err != nil {
		t.Fatal(err)
	}
	before := s.manifest.Load()

	up.setFailHead(true)

	if err := s.syncManifest(); err == nil {
		t.Fatal("expected error from failed HEAD")
	}
	if s.manifest.Load() != before {
		t.Fatal("failed refresh replaced the snapshot")
	}
}

// A failed GET keeps the previous snapshot too.
func TestManifestGETFailureKeepsSnapshot(t *testing.T) {
	up := &manifestUpstream{etag: "", body: manifestV1}
	s := newSyncTestServer(t, up.handler())

	if err := s.syncManifest(); err != nil {
		t.Fatal(err)
	}
	before := s.manifest.Load()

	up.mu.Lock()
	up.failGet = true
	up.mu.Unlock()

	if err := s.syncManifest(); err == nil {
		t.Fatal("expected error from failed GET")
	}
	if s.manifest.Load() != before {
		t.Fatal("failed GET replaced the snapshot")
	}
}

// Validation is all-or-nothing: an invalid policy list must not publish a
// partially compiled manifest.
func TestManifestInvalidUpdateKeepsSnapshot(t *testing.T) {
	invalid := []struct {
		name string
		body string
	}{
		{"duplicate names", `{"mirrors": [], "policies": [
			{"name": "a", "rules": {"traffic_policy": "latency"}},
			{"name": "a", "rules": {"traffic_policy": "speed"}}]}`},
		{"invalid regex", `{"mirrors": [], "policies": [
			{"name": "a", "match": {"path_pattern": "(?<=x)/y"}, "rules": {"traffic_policy": "speed"}}]}`},
		{"unknown traffic policy", `{"mirrors": [], "policies": [
			{"name": "a", "rules": {"traffic_policy": "turbo"}}]}`},
		{"malformed json", `{"mirrors": [`},
	}

	for _, c := range invalid {
		t.Run(c.name, func(t *testing.T) {
			up := &manifestUpstream{etag: `"v1"`, body: manifestV1}
			s := newSyncTestServer(t, up.handler())

			if err := s.syncManifest(); err != nil {
				t.Fatal(err)
			}
			before := s.manifest.Load()

			up.set(`"v2"`, c.body)

			if err := s.syncManifest(); err == nil {
				t.Fatal("expected validation error")
			}
			if s.manifest.Load() != before {
				t.Fatal("invalid update replaced the snapshot")
			}
			if got := s.manifest.Load().etag; got != `"v1"` {
				t.Fatalf("etag = %q, want the last known good one", got)
			}
		})
	}
}

// An upstream that predates ETags is polled with GET every cycle, and updates
// are still applied.
func TestManifestWithoutETagStillUpdates(t *testing.T) {
	up := &manifestUpstream{body: manifestV1}
	s := newSyncTestServer(t, up.handler())

	if err := s.syncManifest(); err != nil {
		t.Fatal(err)
	}
	if got := s.manifest.Load().etag; got != "" {
		t.Fatalf("etag = %q, want empty", got)
	}

	up.set("", manifestV2)

	if err := s.syncManifest(); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if gets, heads := up.counts(); gets != 2 || heads != 0 {
		t.Fatalf("gets=%d heads=%d, want 2/0", gets, heads)
	}
	if len(s.manifest.Load().mirrors) != 3 {
		t.Fatal("manifest without ETag was not updated")
	}
}

// HEAD returning 200 without an ETag is inconclusive: fetch the body.
func TestManifestHEADWithoutETagFetches(t *testing.T) {
	up := &manifestUpstream{etag: `"v1"`, body: manifestV1}
	s := newSyncTestServer(t, up.handler())

	if err := s.syncManifest(); err != nil {
		t.Fatal(err)
	}

	up.mu.Lock()
	up.etag = "" // still a relay-known etag locally; upstream stops emitting one
	up.mu.Unlock()

	if err := s.syncManifest(); err != nil {
		t.Fatal(err)
	}
	if gets, heads := up.counts(); gets != 2 || heads != 1 {
		t.Fatalf("gets=%d heads=%d, want 2/1", gets, heads)
	}
}

func TestJitteredWait(t *testing.T) {
	base := 10 * time.Minute
	for i := 0; i < 100; i++ {
		got := jitteredWait(base)
		if got < base-base/6 || got > base+base/6 {
			t.Fatalf("jitteredWait out of range: %v", got)
		}
	}
	// rand.Int64N(0) would panic; intervals below 3ns must pass through.
	if got := jitteredWait(1); got != 1 {
		t.Fatalf("jitteredWait(1) = %v", got)
	}
}

// The debugging endpoint carries the policies along with the mirrors.
func TestHandleMirrorsIncludesPolicies(t *testing.T) {
	s := newPolicyTestServer(t,
		[]Mirror{{Name: "debian", Path: "debian", PolicyTags: []string{"apt"}}},
		Policy{Name: "apt-pool", Match: PolicyMatch{Tag: "apt"}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
	)

	rec := httptest.NewRecorder()
	s.handleMirrors(rec, httptest.NewRequest(http.MethodGet, "/.mirror-relay/mirrors", nil))

	var got mirrorsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Mirrors) != 1 || len(got.Mirrors[0].PolicyTags) != 1 {
		t.Fatalf("mirrors = %+v", got.Mirrors)
	}
	if len(got.Policies) != 1 || got.Policies[0].Name != "apt-pool" {
		t.Fatalf("policies = %+v", got.Policies)
	}
}

func TestHandleMirrorsWithoutSnapshot(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleMirrors(rec, httptest.NewRequest(http.MethodGet, "/.mirror-relay/mirrors", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
