package server

import (
	"encoding/json"
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

// decodeRules parses the wire shape, so the tests exercise the same optional
// field handling the manifest loader does.
func decodeRules(t *testing.T, raw string) PolicyRules {
	t.Helper()
	var r PolicyRules
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("decode rules %s: %v", raw, err)
	}
	return r
}

// compileState compiles a policy list and wraps it in a snapshot, the way
// newManifestState does after validation.
func compileState(t *testing.T, policies ...Policy) *manifestState {
	t.Helper()
	compiled, err := compilePolicies(policies)
	if err != nil {
		t.Fatalf("compilePolicies: %v", err)
	}
	return &manifestState{policies: compiled}
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
		{"missing traffic policy inherits", []Policy{{Name: "inherit"}}, false},
		{"missing name", []Policy{{Rules: PolicyRules{TrafficPolicy: trafficLatency}}}, true},
		{"blank name", []Policy{{Name: "  ", Rules: PolicyRules{TrafficPolicy: trafficLatency}}}, true},
		{"duplicate name", []Policy{valid, valid}, true},
		{"unknown traffic policy", []Policy{{
			Name:  "bad",
			Rules: PolicyRules{TrafficPolicy: "turbo"},
		}}, true},
		// An explicit empty string is rejected during decoding; see
		// TestPolicyRulesRejectInvalidJSON.
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

// Rules merge at the top level in array order: later matches overwrite the
// fields they declare, omitted fields inherit, and a rule may stop evaluation.
func TestPolicyMergeTruthTable(t *testing.T) {
	cases := []struct {
		name     string
		policies []Policy
		mirror   Mirror
		path     string
		want     policyDecision
	}{
		{
			name: "default when nothing matches",
			policies: []Policy{
				{Name: "deb", Match: PolicyMatch{PathSuffix: ".deb"}, Rules: PolicyRules{TrafficPolicy: trafficSpeed, Passthrough: boolRule(true)}},
			},
			mirror: Mirror{Name: "debian"},
			path:   "/dists/stable/Release",
			want:   defaultPolicyDecision,
		},
		{
			name: "one rule sets one field",
			policies: []Policy{
				{Name: "deb", Match: PolicyMatch{PathSuffix: ".deb"}, Rules: PolicyRules{Passthrough: boolRule(true)}},
			},
			mirror: Mirror{Name: "debian"},
			path:   "/pool/x.deb",
			want:   policyDecision{TrafficPolicy: trafficLatency, Passthrough: true},
		},
		{
			name: "omitted fields inherit from earlier matches",
			policies: []Policy{
				{Name: "speed", Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
				{Name: "passthrough", Match: PolicyMatch{PathSuffix: ".deb"}, Rules: PolicyRules{Passthrough: boolRule(true)}},
			},
			mirror: Mirror{Name: "debian"},
			path:   "/pool/x.deb",
			want:   policyDecision{TrafficPolicy: trafficSpeed, Passthrough: true},
		},
		{
			name: "later match overwrites traffic policy",
			policies: []Policy{
				{Name: "broad", Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
				{Name: "narrow", Match: PolicyMatch{PathSuffix: ".deb"}, Rules: PolicyRules{TrafficPolicy: trafficLatency}},
			},
			mirror: Mirror{Name: "debian"},
			path:   "/pool/x.deb",
			want:   policyDecision{TrafficPolicy: trafficLatency},
		},
		{
			name: "explicit false overwrites an earlier true",
			policies: []Policy{
				{Name: "all", Rules: PolicyRules{Passthrough: boolRule(true)}},
				{Name: "indexes", Match: PolicyMatch{PathPrefix: "/dists/"}, Rules: PolicyRules{Passthrough: boolRule(false)}},
			},
			mirror: Mirror{Name: "debian"},
			path:   "/dists/stable/Release",
			want:   policyDecision{TrafficPolicy: trafficLatency, Passthrough: false},
		},
		{
			name: "explicit true overwrites an earlier false",
			policies: []Policy{
				{Name: "all", Rules: PolicyRules{Passthrough: boolRule(false)}},
				{Name: "pool", Match: PolicyMatch{PathPrefix: "/pool/"}, Rules: PolicyRules{Passthrough: boolRule(true)}},
			},
			mirror: Mirror{Name: "debian"},
			path:   "/pool/x.deb",
			want:   policyDecision{TrafficPolicy: trafficLatency, Passthrough: true},
		},
		{
			name: "last match wins across three rules",
			policies: []Policy{
				{Name: "one", Rules: PolicyRules{TrafficPolicy: trafficSpeed, Passthrough: boolRule(true)}},
				{Name: "two", Match: PolicyMatch{PathPrefix: "/pool/"}, Rules: PolicyRules{Passthrough: boolRule(false)}},
				{Name: "three", Match: PolicyMatch{PathSuffix: ".deb"}, Rules: PolicyRules{TrafficPolicy: trafficLatency}},
			},
			mirror: Mirror{Name: "debian"},
			path:   "/pool/x.deb",
			want:   policyDecision{TrafficPolicy: trafficLatency, Passthrough: false},
		},
		{
			name: "exit stops after applying the current rule",
			policies: []Policy{
				{Name: "stop", Match: PolicyMatch{PathSuffix: ".deb"}, Rules: PolicyRules{TrafficPolicy: trafficSpeed, Exit: boolRule(true)}},
				{Name: "later", Rules: PolicyRules{TrafficPolicy: trafficLatency, Passthrough: boolRule(true)}},
			},
			mirror: Mirror{Name: "debian"},
			path:   "/pool/x.deb",
			want:   policyDecision{TrafficPolicy: trafficSpeed},
		},
		{
			name: "exit rule with no merge fields still stops",
			policies: []Policy{
				{Name: "stop", Rules: PolicyRules{Exit: boolRule(true)}},
				{Name: "later", Rules: PolicyRules{TrafficPolicy: trafficSpeed, Passthrough: boolRule(true)}},
			},
			mirror: Mirror{Name: "debian"},
			path:   "/anything",
			want:   defaultPolicyDecision,
		},
		{
			name: "exit false keeps evaluating",
			policies: []Policy{
				{Name: "no-stop", Rules: PolicyRules{Exit: boolRule(false)}},
				{Name: "later", Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
			},
			mirror: Mirror{Name: "debian"},
			path:   "/anything",
			want:   policyDecision{TrafficPolicy: trafficSpeed},
		},
		{
			name: "unmatched exit rule does not stop",
			policies: []Policy{
				{Name: "stop", Match: PolicyMatch{PathSuffix: ".deb"}, Rules: PolicyRules{Exit: boolRule(true)}},
				{Name: "later", Rules: PolicyRules{TrafficPolicy: trafficSpeed}},
			},
			mirror: Mirror{Name: "debian"},
			path:   "/dists/stable/Release",
			want:   policyDecision{TrafficPolicy: trafficSpeed},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := compileState(t, c.policies...)
			if got := st.policyFor(c.mirror, c.path); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// End to end from manifest JSON: the wire shape feeds the same merge that
// policyFor evaluates against a resolved mirror.
func TestPolicyMergeFromManifestJSON(t *testing.T) {
	const body = `{
		"mirrors": [{"name": "debian", "path": "debian", "policy_tags": ["apt"]}],
		"policies": [
			{"name": "base", "match": {"tag": "apt"}, "rules": {"traffic_policy": "speed", "passthrough": true}},
			{"name": "index", "match": {"path_prefix": "/dists/"}, "rules": {"passthrough": false}},
			{"name": "stop", "match": {"path_suffix": "/Release"}, "rules": {"exit": true}},
			{"name": "later", "match": {}, "rules": {"traffic_policy": "latency", "passthrough": true}}
		]
	}`

	var r mirrorsResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	st, err := newManifestState(r, "")
	if err != nil {
		t.Fatalf("newManifestState: %v", err)
	}
	debian := Mirror{Name: "debian", Path: "debian", PolicyTags: []string{"apt"}}

	// base merges speed+passthrough, index overrides passthrough, stop exits
	// before the trailing catch-all can override anything.
	if got, want := st.policyFor(debian, "/dists/stable/Release"), (policyDecision{TrafficPolicy: trafficSpeed}); got != want {
		t.Fatalf("index path: got %+v, want %+v", got, want)
	}
	// No exit here, so the final empty-match rule wins.
	if got, want := st.policyFor(debian, "/pool/x.deb"), (policyDecision{TrafficPolicy: trafficLatency, Passthrough: true}); got != want {
		t.Fatalf("pool path: got %+v, want %+v", got, want)
	}
}

// A declared empty traffic_policy is invalid; an omitted one is not.
func TestPolicyTrafficPolicyOptional(t *testing.T) {
	ps := compileState(t,
		Policy{Name: "inherit", Match: PolicyMatch{PathSuffix: ".deb"}},
	)
	if got := ps.policyFor(Mirror{Name: "debian"}, "/x.deb"); got.TrafficPolicy != trafficLatency {
		t.Fatalf("omitted traffic_policy: got %q, want latency", got.TrafficPolicy)
	}

	var rules PolicyRules
	if err := json.Unmarshal([]byte(`{"traffic_policy":""}`), &rules); err == nil {
		t.Fatal("explicit empty traffic_policy was accepted")
	}
}

// Null and malformed rule values are configuration errors, never "unset".
func TestPolicyRulesRejectInvalidJSON(t *testing.T) {
	cases := []string{
		`null`,
		`{"traffic_policy":null}`,
		`{"traffic_policy":""}`,
		`{"passthrough":null}`,
		`{"exit":null}`,
		`{"traffic_policy":1}`,
		`{"passthrough":"true"}`,
		`{"exit":"false"}`,
		`{"cache":true}`,
		`{"cache_ttl":600}`,
		`[]`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			var r PolicyRules
			if err := json.Unmarshal([]byte(raw), &r); err == nil {
				t.Fatalf("accepted invalid rules %s", raw)
			}
		})
	}

	var p Policy
	if err := json.Unmarshal([]byte(`{"name":"x","rules":null}`), &p); err == nil {
		t.Fatal("rules:null was accepted")
	}
	if err := json.Unmarshal([]byte(`{"name":"x","match":null}`), &p); err == nil {
		t.Fatal("match:null was accepted")
	}
}

// Absent, false and true stay distinct through a JSON round trip; the wire
// shape never exposes the internal option wrapper.
func TestPolicyRulesJSONShape(t *testing.T) {
	var r PolicyRules
	if err := json.Unmarshal([]byte(`{"passthrough":false}`), &r); err != nil {
		t.Fatal(err)
	}
	if !r.Passthrough.set || r.Passthrough.value {
		t.Fatalf("passthrough = %+v, want explicitly false", r.Passthrough)
	}
	if r.TrafficPolicy != "" || r.Exit.set {
		t.Fatalf("absent fields marked set: %+v", r)
	}

	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), `{"passthrough":false}`; got != want {
		t.Fatalf("marshal = %s, want %s", got, want)
	}

	out, err = json.Marshal(PolicyRules{})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{}` {
		t.Fatalf("empty rules marshalled to %s", out)
	}
}

// A full manifest in the Worker's current shape (name/description/match/rules)
// decodes and keeps the description.
func TestManifestPolicyDescription(t *testing.T) {
	const body = `{
		"mirrors": [{"name": "docker", "host": "dcr", "policy_tags": ["oci"]}],
		"policies": [{
			"name": "oci-blobs",
			"description": "OCI layer blobs are large and benefit from sustained throughput.",
			"match": {"tag": "oci", "path_pattern": "^/v2/.+/blobs/"},
			"rules": {"traffic_policy": "speed"}
		}]
	}`

	var r mirrorsResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	st, err := newManifestState(r, "")
	if err != nil {
		t.Fatalf("newManifestState: %v", err)
	}
	if len(st.policies) != 1 || st.policies[0].Description != "OCI layer blobs are large and benefit from sustained throughput." {
		t.Fatalf("description not preserved: %+v", st.policies)
	}
	if got := st.policyFor(Mirror{Name: "docker", PolicyTags: []string{"oci"}}, "/v2/library/nginx/blobs/sha256:abc"); got.TrafficPolicy != trafficSpeed {
		t.Fatalf("policy not applied: %+v", got)
	}
}

// The policy object accepts exactly name, description, match and rules.
func TestPolicyLegalFields(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want Policy
	}{
		{"name only", `{"name":"x"}`, Policy{Name: "x"}},
		{"description only", `{"name":"x","description":"d"}`, Policy{Name: "x", Description: "d"}},
		{"match only", `{"name":"x","match":{"tag":"apt"}}`, Policy{Name: "x", Match: PolicyMatch{Tag: "apt"}}},
		{"rules only", `{"name":"x","rules":{"traffic_policy":"speed"}}`, Policy{Name: "x", Rules: PolicyRules{TrafficPolicy: trafficSpeed}}},
		// The current Worker shape includes description on every policy.
		{"worker shape", `{"name":"oci-blobs","description":"OCI layer blobs are large and benefit from sustained throughput.","match":{"tag":"oci","path_pattern":"^/v2/.+/blobs/"},"rules":{"traffic_policy":"speed"}}`,
			Policy{Name: "oci-blobs", Description: "OCI layer blobs are large and benefit from sustained throughput.", Match: PolicyMatch{Tag: "oci", PathPattern: "^/v2/.+/blobs/"}, Rules: PolicyRules{TrafficPolicy: trafficSpeed}}},
		{"full", `{"name":"x","match":{"mirror":"debian","path_prefix":"/pool/"},"rules":{"passthrough":true,"exit":true}}`,
			Policy{Name: "x", Match: PolicyMatch{Mirror: "debian", PathPrefix: "/pool/"}, Rules: PolicyRules{Passthrough: boolRule(true), Exit: boolRule(true)}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got Policy
			if err := json.Unmarshal([]byte(c.raw), &got); err != nil {
				t.Fatalf("decode %s: %v", c.raw, err)
			}
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// A misspelled top-level key (notably `matc`) must reject the policy instead of
// leaving an empty match that catches every resolved request.
func TestPolicyRejectsInvalidJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"null", `null`},
		{"misspelled match", `{"name":"x","matc":{"path_prefix":"/pool/"},"rules":{"traffic_policy":"speed"}}`},
		{"unknown key", `{"name":"x","matchers":{},"rules":{"traffic_policy":"speed"}}`},
		{"null name", `{"name":null}`},
		{"null description", `{"name":"x","description":null}`},
		{"number description", `{"name":"x","description":1}`},
		{"null match", `{"name":"x","match":null}`},
		{"null rules", `{"name":"x","rules":null}`},
		{"number name", `{"name":1}`},
		{"string match", `{"name":"x","match":"apt"}`},
		{"array rules", `{"name":"x","rules":[]}`},
		{"non-object", `[]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var p Policy
			if err := json.Unmarshal([]byte(c.raw), &p); err == nil {
				t.Fatalf("accepted invalid policy %s", c.raw)
			}
		})
	}
}

// The match whitelist accepts exactly the documented fields and nothing else.
func TestPolicyMatchLegalFields(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want PolicyMatch
	}{
		{"empty", `{}`, PolicyMatch{}},
		{"tag", `{"tag":"apt"}`, PolicyMatch{Tag: "apt"}},
		{"mirror", `{"mirror":"debian"}`, PolicyMatch{Mirror: "debian"}},
		{"path prefix", `{"path_prefix":"/pool/"}`, PolicyMatch{PathPrefix: "/pool/"}},
		{"path suffix", `{"path_suffix":".deb"}`, PolicyMatch{PathSuffix: ".deb"}},
		{"path pattern", `{"path_pattern":"^/v2/.+/blobs/"}`, PolicyMatch{PathPattern: `^/v2/.+/blobs/`}},
		{"all", `{"tag":"apt","mirror":"debian","path_prefix":"/pool/","path_suffix":".deb","path_pattern":"^/pool/"}`, PolicyMatch{
			Tag: "apt", Mirror: "debian", PathPrefix: "/pool/", PathSuffix: ".deb", PathPattern: "^/pool/",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got PolicyMatch
			if err := json.Unmarshal([]byte(c.raw), &got); err != nil {
				t.Fatalf("decode %s: %v", c.raw, err)
			}
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// A misspelled condition must fail the manifest, not silently degrade to an
// empty match that catches every resolved request.
func TestPolicyMatchRejectsInvalidJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"null", `null`},
		{"misspelled path_prefix", `{"path_prefx":"/pool/"}`},
		{"misspelled tag", `{"tags":"apt"}`},
		{"unknown field alongside valid ones", `{"path_prefix":"/pool/","path_suffix":".deb","extra":true}`},
		{"null tag", `{"tag":null}`},
		{"null mirror", `{"mirror":null}`},
		{"null path_prefix", `{"path_prefix":null}`},
		{"null path_suffix", `{"path_suffix":null}`},
		{"null path_pattern", `{"path_pattern":null}`},
		{"number tag", `{"tag":1}`},
		{"boolean mirror", `{"mirror":false}`},
		{"array path_pattern", `{"path_pattern":["x"]}`},
		{"object path_prefix", `{"path_prefix":{}}`},
		{"non-object", `[]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var m PolicyMatch
			if err := json.Unmarshal([]byte(c.raw), &m); err == nil {
				t.Fatalf("accepted invalid match %s", c.raw)
			}
		})
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
		if got := st.policyFor(c.mirror, c.path).TrafficPolicy; got != c.want {
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
		if got := st.policyFor(m, c.path).TrafficPolicy; got != c.want {
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
	if got := st.policyFor(Mirror{Name: "x"}, "/anything").TrafficPolicy; got != trafficSpeed {
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
		// No trailing catch-all: the latency default is implicit, and under
		// last-match-wins an empty-match rule here would override every rule
		// above it.
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	st := &manifestState{policies: policies}

	if got := st.policyFor(Mirror{Name: "docker", PolicyTags: []string{"oci"}}, "/v2/library/nginx/blobs/sha256:abc").TrafficPolicy; got != trafficSpeed {
		t.Errorf("oci blob: %q", got)
	}
	if got := st.policyFor(Mirror{Name: "debian", PolicyTags: []string{"apt"}}, "/pool/main/n/nginx/nginx.deb").TrafficPolicy; got != trafficSpeed {
		t.Errorf("apt pool: %q", got)
	}
	// Packages.gz is small and latency-sensitive: bare .gz is deliberately absent.
	if got := st.policyFor(Mirror{Name: "debian", PolicyTags: []string{"apt"}}, "/dists/bookworm/main/binary-amd64/Packages.gz").TrafficPolicy; got != trafficLatency {
		t.Errorf("Packages.gz: %q, want latency", got)
	}
}

func TestPolicyDefaultIsLatency(t *testing.T) {
	want := policyDecision{TrafficPolicy: trafficLatency, Passthrough: false}
	st := &manifestState{}
	if got := st.policyFor(Mirror{Name: "x"}, "/x"); got != want {
		t.Fatalf("empty policy list: got %+v, want %+v", got, want)
	}
	var nilState *manifestState
	if got := nilState.policyFor(Mirror{Name: "x"}, "/x"); got != want {
		t.Fatalf("nil snapshot: got %+v, want %+v", got, want)
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
		// Unmatched paths use the implicit latency default; a trailing
		// empty-match rule would override the rules above under last-match-wins.
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
	if passthroughFromContext(ctx) {
		t.Fatal("traffic-only context reported passthrough")
	}
	if got := trafficPolicyFromContext(httptest.NewRequest(http.MethodGet, "/", nil).Context()); got != trafficLatency {
		t.Fatalf("missing value: got %q, want latency", got)
	}
}

// The full decision travels to the cache chain, not just the transport choice.
func TestPolicyDecisionContext(t *testing.T) {
	base := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	ctx := withPolicyDecision(base, policyDecision{TrafficPolicy: trafficSpeed, Passthrough: true})

	if got := trafficPolicyFromContext(ctx); got != trafficSpeed {
		t.Fatalf("traffic = %q", got)
	}
	if !passthroughFromContext(ctx) {
		t.Fatal("passthrough lost on the context")
	}

	// withTrafficPolicy updates the transport field without dropping passthrough.
	ctx = withTrafficPolicy(ctx, trafficLatency)
	if got := trafficPolicyFromContext(ctx); got != trafficLatency {
		t.Fatalf("traffic = %q", got)
	}
	if !passthroughFromContext(ctx) {
		t.Fatal("withTrafficPolicy dropped passthrough")
	}

	if passthroughFromContext(base) {
		t.Fatal("missing decision reported passthrough")
	}
}
