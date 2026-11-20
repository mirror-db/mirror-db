package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newPolicyTestServer(t *testing.T, mirrors []Mirror, policies ...Policy) *Server {
	t.Helper()
	st, err := newManifestState(mirrorsResponse{Mirrors: mirrors, Policies: policies}, `"test"`)
	if err != nil {
		t.Fatalf("newManifestState: %v", err)
	}
	s := &Server{baseDomains: []string{"relay.example.com"}}
	s.manifest.Store(st)
	return s
}

func TestCompilePoliciesValidation(t *testing.T) {
	valid := Policy{
		Name:  "ok",
		Match: PolicyMatch{PathPattern: `^/v2/.+/blobs/`},
		Rules: PolicyRules{TrafficPolicy: trafficSpeed},
	}

	cases := []struct {
		name     string
		policies []Policy
		wantErr  bool
	}{
		{"empty list", nil, false},
		{"valid", []Policy{valid}, false},
		{"empty match is valid", []Policy{{Name: "default", Rules: PolicyRules{TrafficPolicy: trafficLatency}}}, false},
		{"missing name", []Policy{{Rules: PolicyRules{TrafficPolicy: trafficLatency}}}, true},
		{"blank name", []Policy{{Name: "  ", Rules: PolicyRules{TrafficPolicy: trafficLatency}}}, true},
		{"duplicate name", []Policy{valid, valid}, true},
		{"unknown traffic policy", []Policy{{
			Name:  "bad",
			Rules: PolicyRules{TrafficPolicy: "turbo"},
		}}, true},
		{"empty traffic policy", []Policy{{Name: "bad"}}, true},
		// Lookbehind is not expressible in RE2, so it must be rejected rather
		// than compiled by a backtracking engine.
		{"non-RE2 pattern", []Policy{{
			Name:  "bad",
			Match: PolicyMatch{PathPattern: `(?<=/foo)/bar`},
			Rules: PolicyRules{TrafficPolicy: trafficSpeed},
		}}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := compilePolicies(c.policies)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %d policies", len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(c.policies) {
				t.Fatalf("got %d policies, want %d", len(got), len(c.policies))
			}
		})
	}
}

// The first complete match wins; rules are never merged.
func TestPolicyFirstMatchWins(t *testing.T) {
	st := &manifestState{}
	policies, err := compilePolicies([]Policy{
		{Name: "top", Rules: PolicyRules{TrafficPolicy: trafficSpeed}, Match: PolicyMatch{PathSuffix: ".deb"}},
		{Name: "catch", Rules: PolicyRules{TrafficPolicy: trafficLatency}},
	})
	if err != nil {
		t.Fatal(err)
	}
	st.policies = policies

	if got := st.policyFor(Mirror{Name: "debian"}, "/pool/x.deb"); got != trafficSpeed {
		t.Fatalf("got %q, want speed (first policy)", got)
	}
	// A later, more specific policy cannot override an earlier broad one.
	policies, err = compilePolicies([]Policy{
		{Name: "broad", Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
		{Name: "narrow", Rules: PolicyRules{TrafficPolicy: trafficLatency}, Match: PolicyMatch{PathSuffix: ".deb"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	st.policies = policies
	if got := st.policyFor(Mirror{Name: "debian"}, "/pool/x.deb"); got != trafficSpeed {
		t.Fatalf("got %q, want speed (order is part of the contract)", got)
	}
}

func TestPolicyConditionsAreANDed(t *testing.T) {
	policies, err := compilePolicies([]Policy{{
		Name:  "apt-pool",
		Match: PolicyMatch{Tag: "apt", Mirror: "debian", PathPrefix: "/pool/"},
		Rules: PolicyRules{TrafficPolicy: trafficSpeed},
	}})
	if err != nil {
		t.Fatal(err)
	}
	st := &manifestState{policies: policies}

	cases := []struct {
		name   string
		mirror Mirror
		path   string
		want   trafficPolicy
	}{
		{"all present", Mirror{Name: "debian", PolicyTags: []string{"apt"}}, "/pool/x.deb", trafficSpeed},
		{"wrong tag", Mirror{Name: "debian", PolicyTags: []string{"oci"}}, "/pool/x.deb", trafficLatency},
		{"no tags", Mirror{Name: "debian"}, "/pool/x.deb", trafficLatency},
		{"wrong mirror", Mirror{Name: "ubuntu", PolicyTags: []string{"apt"}}, "/pool/x.deb", trafficLatency},
		{"wrong path", Mirror{Name: "debian", PolicyTags: []string{"apt"}}, "/dists/stable/Release", trafficLatency},
	}
	for _, c := range cases {
		if got := st.policyFor(c.mirror, c.path); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPolicyPathConditions(t *testing.T) {
	policies, err := compilePolicies([]Policy{
		{Name: "deb", Match: PolicyMatch{PathSuffix: ".deb"}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
		{Name: "blobs", Match: PolicyMatch{PathPattern: `^/v2/.+/blobs/`}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
		{Name: "iso", Match: PolicyMatch{PathPrefix: "/releases/"}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
	})
	if err != nil {
		t.Fatal(err)
	}
	st := &manifestState{policies: policies}
	m := Mirror{Name: "x"}

	cases := []struct {
		path string
		want trafficPolicy
	}{
		{"/pool/x.deb", trafficSpeed},
		{"/pool/x.dsc", trafficLatency},
		{"/v2/library/nginx/blobs/sha256:abc", trafficSpeed},
		{"/v2/library/nginx/manifests/latest", trafficLatency},
		{"/releases/24.04/ubuntu.iso", trafficSpeed},
		{"/dists/stable/Release", trafficLatency},
	}
	for _, c := range cases {
		if got := st.policyFor(m, c.path); got != c.want {
			t.Errorf("path %q: got %q, want %q", c.path, got, c.want)
		}
	}
}

func TestPolicyEmptyMatchCatchesAll(t *testing.T) {
	policies, err := compilePolicies([]Policy{
		{Name: "everything", Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
	})
	if err != nil {
		t.Fatal(err)
	}
	st := &manifestState{policies: policies}
	if got := st.policyFor(Mirror{Name: "x"}, "/anything"); got != trafficSpeed {
		t.Fatalf("got %q", got)
	}
}

// The initial Worker-side policy table must compile under Go's RE2. A pattern
// that only works in a backtracking engine would make every relay reject the
// manifest and keep an empty policy set forever.
func TestCompileWorkerDefaultPolicies(t *testing.T) {
	const largeArtifacts = `\.(?:tar|tar\.gz|tgz|tar\.xz|txz|tar\.zst|tar\.bz2|tbz2|zip|7z|iso|img|squashfs|bin|deb|ddeb|udeb|rpm|apk|exe|msi|dmg|pkg)$`

	policies, err := compilePolicies([]Policy{
		{Name: "oci-blobs", Match: PolicyMatch{Tag: "oci", PathPattern: `^/v2/.+/blobs/`}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
		{Name: "apt-packages", Match: PolicyMatch{Tag: "apt", PathPrefix: "/pool/"}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
		{Name: "large-artifacts", Match: PolicyMatch{PathPattern: largeArtifacts}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
		{Name: "default", Rules: PolicyRules{TrafficPolicy: trafficLatency}},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	st := &manifestState{policies: policies}

	if got := st.policyFor(Mirror{Name: "docker", PolicyTags: []string{"oci"}}, "/v2/library/nginx/blobs/sha256:abc"); got != trafficSpeed {
		t.Errorf("oci blob: %q", got)
	}
	if got := st.policyFor(Mirror{Name: "debian", PolicyTags: []string{"apt"}}, "/pool/main/n/nginx/nginx.deb"); got != trafficSpeed {
		t.Errorf("apt pool: %q", got)
	}
	// Packages.gz is small and latency-sensitive: bare .gz is deliberately absent.
	if got := st.policyFor(Mirror{Name: "debian", PolicyTags: []string{"apt"}}, "/dists/bookworm/main/binary-amd64/Packages.gz"); got != trafficLatency {
		t.Errorf("Packages.gz: %q, want latency", got)
	}
}

func TestPolicyDefaultIsLatency(t *testing.T) {
	st := &manifestState{}
	if got := st.policyFor(Mirror{Name: "x"}, "/x"); got != trafficLatency {
		t.Fatalf("empty policy list: got %q, want latency", got)
	}
	var nilState *manifestState
	if got := nilState.policyFor(Mirror{Name: "x"}, "/x"); got != trafficLatency {
		t.Fatalf("nil snapshot: got %q, want latency", got)
	}
}

// Path mounts are matched relative to their mount; subdomain mounts keep the
// URL path. Only requests that resolve to a manifest mirror can match.
func TestResolveMirror(t *testing.T) {
	s := newPolicyTestServer(t,
		[]Mirror{
			{Name: "debian", Path: "debian"},
			{Name: "docker", Host: "dcr", PolicyTags: []string{"oci"}},
			{Name: "ubuntu", Host: "ubuntu", Path: "", PolicyTags: nil},
		},
	)

	cases := []struct {
		name     string
		host     string
		target   string
		wantName string
		wantPath string
		wantOK   bool
	}{
		{"mount root", "relay.example.com", "/debian", "debian", "/", true},
		{"mount root slash", "relay.example.com", "/debian/", "debian", "/", true},
		{"mount nested", "relay.example.com", "/debian/pool/x.deb", "debian", "/pool/x.deb", true},
		{"mount keeps case", "relay.example.com", "/debian/Pool/X.DEB", "debian", "/Pool/X.DEB", true},
		{"mount keeps escaping", "relay.example.com", "/debian/pool/a%2Fb.deb", "debian", "/pool/a%2Fb.deb", true},
		{"subdomain", "dcr.relay.example.com", "/v2/library/nginx/blobs/sha256:abc", "docker", "/v2/library/nginx/blobs/sha256:abc", true},
		{"unknown subdomain", "nope.relay.example.com", "/v2/", "", "", false},
		{"control plane", "relay.example.com", "/api/mirrors", "", "", false},
		{"keys control plane", "relay.example.com", "/keys/apt/debian", "", "", false},
		{"unknown first segment", "relay.example.com", "/unknown/x", "", "", false},
		{"bare root", "relay.example.com", "/", "", "", false},
		// A host outside baseDomains is still forwarded by the open proxy, but
		// its path must not be mistaken for a mount.
		{"foreign host", "other.example.net", "/debian/pool/x.deb", "", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, c.target, nil)
			r.Host = c.host
			m, path, ok := s.resolveMirror(s.manifest.Load(), r)
			if ok != c.wantOK || m.Name != c.wantName || path != c.wantPath {
				t.Fatalf("got (%q,%q,%v), want (%q,%q,%v)", m.Name, path, ok, c.wantName, c.wantPath, c.wantOK)
			}
		})
	}
}

// End to end through the manifest snapshot: tag, mirror and mount-relative path.
func TestMatchTrafficPolicy(t *testing.T) {
	s := newPolicyTestServer(t,
		[]Mirror{
			{Name: "debian", Path: "debian", PolicyTags: []string{"apt"}},
			{Name: "docker", Host: "dcr", PolicyTags: []string{"oci"}},
			{Name: "mystery", Path: "mystery"},
		},
		Policy{Name: "oci-blobs", Match: PolicyMatch{Tag: "oci", PathPattern: `^/v2/.+/blobs/`}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
		Policy{Name: "apt-pool", Match: PolicyMatch{Tag: "apt", PathPrefix: "/pool/"}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
		Policy{Name: "artifacts", Match: PolicyMatch{PathSuffix: ".deb"}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
		Policy{Name: "default", Rules: PolicyRules{TrafficPolicy: trafficLatency}},
	)

	cases := []struct {
		name   string
		host   string
		target string
		want   trafficPolicy
	}{
		{"oci blob", "dcr.relay.example.com", "/v2/library/nginx/blobs/sha256:abc", trafficSpeed},
		{"oci manifest", "dcr.relay.example.com", "/v2/library/nginx/manifests/latest", trafficLatency},
		{"apt pool", "relay.example.com", "/debian/pool/main/n/nginx/nginx.deb", trafficSpeed},
		{"apt index", "relay.example.com", "/debian/dists/stable/Release", trafficLatency},
		{"generic suffix after resolution", "relay.example.com", "/mystery/x.deb", trafficSpeed},
		{"unresolved control path", "relay.example.com", "/keys/apt/debian", trafficLatency},
		{"unknown subdomain", "nope.relay.example.com", "/x.deb", trafficLatency},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, c.target, nil)
			r.Host = c.host
			if got := s.matchTrafficPolicy(r); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A request that does not resolve to a concrete mirror must never enter policy
// matching, not even when a policy has an empty match or matches an empty path.
func TestUnresolvedRequestsIgnorePolicies(t *testing.T) {
	s := newPolicyTestServer(t,
		[]Mirror{
			{Name: "debian", Path: "debian"},
			{Name: "docker", Host: "dcr"},
		},
		Policy{Name: "everything", Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
		Policy{Name: "empty-path", Match: PolicyMatch{PathPattern: `^$`}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
	)

	cases := []struct {
		name   string
		host   string
		target string
	}{
		{"unknown subdomain", "nope.relay.example.com", "/anything"},
		{"subdomain with policy path", "nope.relay.example.com", "/v2/x/blobs/sha256:abc"},
		{"api control plane", "relay.example.com", "/api/mirrors"},
		{"keys control plane", "relay.example.com", "/keys/apt/debian"},
		{"unknown first segment", "relay.example.com", "/unknown/x"},
		{"bare root", "relay.example.com", "/"},
		{"foreign host", "other.example.net", "/debian/pool/x.deb"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, c.target, nil)
			r.Host = c.host
			if got := s.matchTrafficPolicy(r); got != trafficLatency {
				t.Fatalf("got %q, want latency", got)
			}
		})
	}
}

// An empty-match policy still applies once a mirror is resolved; the unresolved
// guard must not disable matching altogether.
func TestResolvedRequestsStillMatchEmptyPolicy(t *testing.T) {
	s := newPolicyTestServer(t,
		[]Mirror{{Name: "debian", Path: "debian"}},
		Policy{Name: "everything", Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
	)

	r := httptest.NewRequest(http.MethodGet, "/debian/dists/stable/Release", nil)
	r.Host = "relay.example.com"
	if got := s.matchTrafficPolicy(r); got != trafficSpeed {
		t.Fatalf("got %q, want speed", got)
	}
}

func TestTrafficPolicyContext(t *testing.T) {
	ctx := withTrafficPolicy(httptest.NewRequest(http.MethodGet, "/", nil).Context(), trafficSpeed)
	if got := trafficPolicyFromContext(ctx); got != trafficSpeed {
		t.Fatalf("got %q", got)
	}
	if got := trafficPolicyFromContext(httptest.NewRequest(http.MethodGet, "/", nil).Context()); got != trafficLatency {
		t.Fatalf("missing value: got %q, want latency", got)
	}
}
