package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The two patterns the Worker distributes in server/relay-policy.ts. They must
// compile under Go's RE2, or every relay would reject the manifest.
const (
	workerImmutableOCI = `^/v2/.+/(?:blobs|manifests)/sha256:(?<sha256>[0-9a-f]{64})$`
	workerImmutableAPT = `^/dists/.+/by-hash/(?:SHA256/(?<sha256>[0-9a-f]{64})|SHA1/(?<sha1>[0-9a-f]{40}))$`
)

func hexString(n int, c byte) string { return strings.Repeat(string(c), n) }

// TestImmutableWorkerPatterns runs the Worker's initial policy table end to
// end: digest blobs and manifests, APT by-hash objects, and paths that must
// stay mutable (tags, Release indexes, pool payloads, other algorithms).
func TestImmutableWorkerPatterns(t *testing.T) {
	policies, err := compilePolicies([]Policy{
		{
			Name:  "oci-digests",
			Match: PolicyMatch{Tag: "oci", PathPattern: workerImmutableOCI},
			Rules: PolicyRules{Immutable: stringRule(workerImmutableOCI)},
		},
		{
			Name:  "apt-by-hash",
			Match: PolicyMatch{Tag: "apt", PathPattern: workerImmutableAPT},
			Rules: PolicyRules{Immutable: stringRule(workerImmutableAPT)},
		},
		{Name: "oci-blobs", Match: PolicyMatch{Tag: "oci", PathPattern: `^/v2/.+/blobs/`}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
		{Name: "apt-packages", Match: PolicyMatch{Tag: "apt", PathPrefix: "/pool/"}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
	})
	if err != nil {
		t.Fatalf("compile Worker patterns: %v", err)
	}
	st := &manifestState{policies: policies}

	docker := Mirror{Name: "docker", PolicyTags: []string{"oci"}}
	debian := Mirror{Name: "debian", PolicyTags: []string{"apt"}}
	sha256 := hexString(64, 'a')
	sha1 := hexString(40, 'b')

	cases := []struct {
		name          string
		mirror        Mirror
		path          string
		wantImmutable bool
		wantDigest    contentDigest
		wantTraffic   trafficPolicy
	}{
		{"oci digest blob", docker, "/v2/library/nginx/blobs/sha256:" + sha256, true, contentDigest{"sha256", sha256}, trafficSpeed},
		{"oci digest manifest", docker, "/v2/library/nginx/manifests/sha256:" + sha256, true, contentDigest{"sha256", sha256}, trafficLatency},
		{"oci tag manifest", docker, "/v2/library/nginx/manifests/latest", false, contentDigest{}, trafficLatency},
		{"apt by-hash sha256", debian, "/dists/bookworm/main/binary-amd64/by-hash/SHA256/" + sha256, true, contentDigest{"sha256", sha256}, trafficLatency},
		{"apt by-hash sha1", debian, "/dists/bookworm/main/binary-amd64/by-hash/SHA1/" + sha1, true, contentDigest{"sha1", sha1}, trafficLatency},
		{"apt Release", debian, "/dists/bookworm/Release", false, contentDigest{}, trafficLatency},
		{"apt InRelease", debian, "/dists/bookworm/InRelease", false, contentDigest{}, trafficLatency},
		{"apt package pool", debian, "/pool/main/n/nginx/nginx.deb", false, contentDigest{}, trafficSpeed},
		{"apt undeclared algorithm", debian, "/dists/bookworm/main/by-hash/SHA512/" + hexString(128, 'c'), false, contentDigest{}, trafficLatency},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := st.policyFor(c.mirror, c.path)
			if got.Immutable != c.wantImmutable || got.Digest != c.wantDigest {
				t.Fatalf("immutable = %v %+v, want %v %+v", got.Immutable, got.Digest, c.wantImmutable, c.wantDigest)
			}
			if got.TrafficPolicy != c.wantTraffic {
				t.Fatalf("traffic = %q, want %q", got.TrafficPolicy, c.wantTraffic)
			}
		})
	}
}

// The immutable pattern merges like traffic_policy: omission inherits, every
// declared value replaces even when its expression does not match, and exit
// still stops evaluation after its own merge.
func TestImmutableMerge(t *testing.T) {
	sha256 := hexString(64, 'd')
	sha1 := hexString(40, 'e')

	cases := []struct {
		name     string
		policies []Policy
		path     string
		want     policyDecision
	}{
		{
			name: "omitted immutable inherits an earlier match",
			policies: []Policy{
				{Name: "declare", Rules: PolicyRules{Immutable: stringRule(`^/(?<sha256>[0-9a-f]{64})`)}},
				{Name: "inherit", Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
			},
			path: "/" + sha256,
			want: policyDecision{TrafficPolicy: trafficSpeed, Immutable: true, Digest: contentDigest{"sha256", sha256}},
		},
		{
			name: "later declared pattern replaces the earlier one",
			policies: []Policy{
				{Name: "declare", Rules: PolicyRules{Immutable: stringRule(`^/(?<sha256>[0-9a-f]{64})`)}},
				{Name: "replace", Match: PolicyMatch{PathSuffix: "/" + sha1}, Rules: PolicyRules{Immutable: stringRule(`^/.*/(?<sha1>[0-9a-f]{40})$`)}},
			},
			path: "/" + sha256 + "/" + sha1,
			want: policyDecision{TrafficPolicy: trafficLatency, Immutable: true, Digest: contentDigest{"sha1", sha1}},
		},
		{
			name: "replacement pattern that does not match declares nothing",
			policies: []Policy{
				{Name: "declare", Rules: PolicyRules{Immutable: stringRule(`^/locked/`)}},
				{Name: "replace", Match: PolicyMatch{PathSuffix: ".txt"}, Rules: PolicyRules{Immutable: stringRule(`^/never/`)}},
			},
			path: "/locked/x.txt",
			want: policyDecision{TrafficPolicy: trafficLatency},
		},
		{
			name: "exit stops before a later immutable declaration",
			policies: []Policy{
				{Name: "stop", Match: PolicyMatch{PathSuffix: ".bin"}, Rules: PolicyRules{Immutable: stringRule(`^/(?<sha256>[0-9a-f]{64})`), Exit: boolRule(true)}},
				{Name: "later", Rules: PolicyRules{Immutable: stringRule(`^/.*/(?<sha1>[0-9a-f]{40})$`)}},
			},
			path: "/" + sha256 + "/" + sha1 + ".bin",
			want: policyDecision{TrafficPolicy: trafficLatency, Immutable: true, Digest: contentDigest{"sha256", sha256}},
		},
		{
			name: "exit rule without immutable keeps the inherited value",
			policies: []Policy{
				{Name: "declare", Rules: PolicyRules{Immutable: stringRule(`^/locked/`)}},
				{Name: "stop", Rules: PolicyRules{Exit: boolRule(true)}},
				{Name: "later", Rules: PolicyRules{Immutable: stringRule(`^/never/`)}},
			},
			path: "/locked/x",
			want: policyDecision{TrafficPolicy: trafficLatency, Immutable: true},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := compileState(t, c.policies...)
			got := st.policyFor(Mirror{Name: "debian"}, c.path)
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// A pattern without a digest group still declares immutable bytes; only one
// usable named group may contribute a digest.
func TestImmutableDigestExtraction(t *testing.T) {
	cases := []struct {
		name          string
		pattern       string
		path          string
		wantImmutable bool
		wantDigest    contentDigest
	}{
		{"no named group", `^/locked/`, "/locked/x", true, contentDigest{}},
		{"sha256 group", `^/(?<sha256>[0-9a-f]+)$`, "/abcdef", true, contentDigest{"sha256", "abcdef"}},
		{"sha1 group", `^/(?<sha1>[0-9a-f]+)$`, "/abcdef", true, contentDigest{"sha1", "abcdef"}},
		{"uppercase hex is normalised", `^/(?<sha256>[0-9A-F]+)$`, "/ABCDEF", true, contentDigest{"sha256", "abcdef"}},
		{"both groups capture", `^/(?<sha256>[0-9a-f]+)/(?<sha1>[0-9a-f]+)$`, "/abc/def", true, contentDigest{}},
		{"non-hex capture", `^/(?<sha256>.+)$`, "/not-hex", true, contentDigest{}},
		{"pattern does not match", `^/locked/`, "/open/x", false, contentDigest{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := compileState(t, Policy{Name: "immutable", Rules: PolicyRules{Immutable: stringRule(c.pattern)}})
			got := st.policyFor(Mirror{Name: "x"}, c.path)
			if got.Immutable != c.wantImmutable || got.Digest != c.wantDigest {
				t.Fatalf("got %v %+v, want %v %+v", got.Immutable, got.Digest, c.wantImmutable, c.wantDigest)
			}
		})
	}
}

// An invalid expression rejects the whole list, so the relay keeps its last
// known good manifest instead of serving with a partially applied policy.
func TestImmutableValidation(t *testing.T) {
	cases := []struct {
		name     string
		policies []Policy
	}{
		{"lookbehind is not RE2", []Policy{{Name: "bad", Rules: PolicyRules{Immutable: stringRule(`(?<=/x)y`)}}}},
		{"unclosed group", []Policy{{Name: "bad", Rules: PolicyRules{Immutable: stringRule(`^/(?<sha256>[0-9a-f]+`)}}}},
		{"invalid pattern among valid ones", []Policy{
			{Name: "ok", Rules: PolicyRules{Immutable: stringRule(`^/locked/`)}},
			{Name: "bad", Rules: PolicyRules{Immutable: stringRule(`(`)}},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := compilePolicies(c.policies); err == nil {
				t.Fatal("invalid immutable expression was accepted")
			}
		})
	}

	// The manifest loader must reject it too, not only compilePolicies.
	var r mirrorsResponse
	if err := json.Unmarshal([]byte(`{"mirrors":[],"policies":[{"name":"bad","rules":{"immutable":"("}}]}`), &r); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if _, err := newManifestState(r, ""); err == nil {
		t.Fatal("newManifestState accepted an invalid immutable expression")
	}
}

// The wire shape accepts any string, keeps absent distinct from declared, and
// keeps rejecting null, non-strings and unknown fields.
func TestImmutableJSONRules(t *testing.T) {
	var r PolicyRules
	if err := json.Unmarshal([]byte(`{"immutable":"^/x$"}`), &r); err != nil {
		t.Fatalf("decode immutable: %v", err)
	}
	if !r.Immutable.set || r.Immutable.value != `^/x$` {
		t.Fatalf("immutable = %+v, want declared", r.Immutable)
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), `{"immutable":"^/x$"}`; got != want {
		t.Fatalf("marshal = %s, want %s", got, want)
	}

	out, err = json.Marshal(PolicyRules{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{}` {
		t.Fatalf("absent immutable leaked into the wire shape: %s", out)
	}

	bad := []string{
		`{"immutable":null}`,
		`{"immutable":1}`,
		`{"immutable":true}`,
		`{"immutable":["^/x"]}`,
		`{"immutable":{}}`,
		`{"immutable":"^/x","immutable_ttl":60}`,
		`{"immutable":"^/x","cache":true}`,
	}
	for _, raw := range bad {
		t.Run(raw, func(t *testing.T) {
			var rules PolicyRules
			if err := json.Unmarshal([]byte(raw), &rules); err == nil {
				t.Fatalf("accepted invalid rules %s", raw)
			}
		})
	}
}

// The pattern runs against the mount-relative escaped path, so a path mount's
// prefix never hides or fakes a match, and the decision reaches the cache chain
// through the existing context value.
func TestImmutableMountRelativePath(t *testing.T) {
	sha256 := hexString(64, 'f')
	s := newPolicyTestServer(t,
		[]Mirror{
			{Name: "debian", Path: "debian", PolicyTags: []string{"apt"}},
			{Name: "docker", Host: "dcr", PolicyTags: []string{"oci"}},
		},
		Policy{Name: "apt-by-hash", Match: PolicyMatch{Tag: "apt", PathPattern: workerImmutableAPT}, Rules: PolicyRules{Immutable: stringRule(workerImmutableAPT)}},
		Policy{Name: "oci-digests", Match: PolicyMatch{Tag: "oci", PathPattern: workerImmutableOCI}, Rules: PolicyRules{Immutable: stringRule(workerImmutableOCI)}},
	)

	cases := []struct {
		name          string
		host          string
		target        string
		wantImmutable bool
		wantDigest    contentDigest
	}{
		{
			name:          "path mount drops the mount prefix",
			host:          "relay.example.com",
			target:        "/debian/dists/bookworm/main/by-hash/SHA256/" + sha256,
			wantImmutable: true,
			wantDigest:    contentDigest{"sha256", sha256},
		},
		{
			name:          "subdomain mount keeps the URL path",
			host:          "dcr.relay.example.com",
			target:        "/v2/library/nginx/blobs/sha256:" + sha256,
			wantImmutable: true,
			wantDigest:    contentDigest{"sha256", sha256},
		},
		{
			name:          "path mount Release stays mutable",
			host:          "relay.example.com",
			target:        "/debian/dists/bookworm/Release",
			wantImmutable: false,
		},
		{
			name:          "path mount tag manifest stays mutable",
			host:          "dcr.relay.example.com",
			target:        "/v2/library/nginx/manifests/latest",
			wantImmutable: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, c.target, nil)
			r.Host = c.host
			d := s.matchPolicy(r)
			if d.Immutable != c.wantImmutable || d.Digest != c.wantDigest {
				t.Fatalf("policy decision = %v %+v, want %v %+v", d.Immutable, d.Digest, c.wantImmutable, c.wantDigest)
			}
			ok, digest := immutableFromContext(withPolicyDecision(context.Background(), d))
			if ok != c.wantImmutable || digest != c.wantDigest {
				t.Fatalf("context = %v %+v, want %v %+v", ok, digest, c.wantImmutable, c.wantDigest)
			}
		})
	}

	// An absent decision is never immutable.
	if ok, digest := immutableFromContext(context.Background()); ok || digest != (contentDigest{}) {
		t.Fatalf("empty context = %v %+v, want not immutable", ok, digest)
	}
}
