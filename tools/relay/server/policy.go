package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// trafficPolicy is the transport preference a rule selects. It is a preference,
// not an availability requirement: the caller falls back to the latency pool
// when the speed pool is empty.
type trafficPolicy string

const (
	trafficLatency trafficPolicy = "latency"
	trafficSpeed   trafficPolicy = "speed"
)

// ruleBool is a rule field that can be absent, explicitly false, or explicitly
// true. A pointer would collapse absent and null; a plain bool would collapse
// absent and false. Both distinctions matter: `passthrough: false` must
// override an earlier `true`, while an omitted field inherits.
type ruleBool struct {
	value bool
	set   bool
}

func boolRule(v bool) ruleBool { return ruleBool{value: v, set: true} }

func (b ruleBool) MarshalJSON() ([]byte, error) {
	return json.Marshal(b.value)
}

func (b *ruleBool) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return fmt.Errorf("rule boolean: null is not allowed")
	}
	b.set = true
	return json.Unmarshal(data, &b.value)
}

func isJSONNull(data []byte) bool {
	return string(bytes.TrimSpace(data)) == "null"
}

// ruleString is a rule field that can be absent or declared. Unlike a plain
// string it keeps omission distinct from a declared value, because omission
// inherits an earlier match's value while every declared value replaces it.
type ruleString struct {
	value string
	set   bool
}

func stringRule(v string) ruleString { return ruleString{value: v, set: true} }

func (s ruleString) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.value)
}

func (s *ruleString) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return fmt.Errorf("rule string: null is not allowed")
	}
	var v string
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	s.set = true
	s.value = v
	return nil
}

// Policy mirrors one entry of the manifest's ordered `policies` array. Order is
// part of the public contract: later matches overwrite earlier ones field by
// field.
type Policy struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Match       PolicyMatch `json:"match"`
	Rules       PolicyRules `json:"rules"`
}

// UnmarshalJSON is a strict whitelist for the policy object: only name,
// description, match and rules are accepted. A misspelled key (for example
// `matc`) must not be ignored, because the missing match would decode as an
// empty matcher and turn the rule into a catch-all for every resolved request.
// Null, unknown keys and wrong types reject the whole manifest, so the relay
// keeps its last known good snapshot.
func (p *Policy) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return fmt.Errorf("policy: null is not allowed")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("policy: %w", err)
	}

	var out Policy
	for key, value := range raw {
		switch key {
		case "name":
			if isJSONNull(value) {
				return fmt.Errorf("policy.name: null is not allowed")
			}
			if err := json.Unmarshal(value, &out.Name); err != nil {
				return fmt.Errorf("policy.name: %w", err)
			}
		case "description":
			if isJSONNull(value) {
				return fmt.Errorf("policy.description: null is not allowed")
			}
			if err := json.Unmarshal(value, &out.Description); err != nil {
				return fmt.Errorf("policy.description: %w", err)
			}
		case "match":
			if isJSONNull(value) {
				return fmt.Errorf("policy.match: null is not allowed")
			}
			if err := json.Unmarshal(value, &out.Match); err != nil {
				return fmt.Errorf("policy.match: %w", err)
			}
		case "rules":
			if isJSONNull(value) {
				return fmt.Errorf("policy.rules: null is not allowed")
			}
			if err := json.Unmarshal(value, &out.Rules); err != nil {
				return fmt.Errorf("policy.rules: %w", err)
			}
		default:
			return fmt.Errorf("policy: unknown field %q", key)
		}
	}
	*p = out
	return nil
}

// PolicyMatch selects requests. Every non-empty field must match (AND). An
// entirely empty match selects every request that resolved to a mirror.
type PolicyMatch struct {
	// Tag is an opaque label advertised by a mirror's policy_tags.
	Tag string `json:"tag,omitempty"`
	// Mirror is the canonical mirror name.
	Mirror string `json:"mirror,omitempty"`
	// PathPrefix, PathSuffix and PathPattern are matched against the
	// mount-relative path: no mount prefix, no query string, original case and
	// escaping.
	PathPrefix  string `json:"path_prefix,omitempty"`
	PathSuffix  string `json:"path_suffix,omitempty"`
	PathPattern string `json:"path_pattern,omitempty"`
}

// UnmarshalJSON is a strict whitelist. `match: null` is a configuration error,
// not a synonym for omission, and an unknown key is an error too: silently
// dropping a misspelled condition would turn a narrow rule into an empty match
// and make it catch every resolved request. The whole manifest is not decoded
// with DisallowUnknownFields — only the objects whose shape is a contract
// (policy, match and rules) reject stray keys.
func (m *PolicyMatch) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return fmt.Errorf("match: null is not allowed")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("match: %w", err)
	}

	var out PolicyMatch
	for key, value := range raw {
		var dst *string
		switch key {
		case "tag":
			dst = &out.Tag
		case "mirror":
			dst = &out.Mirror
		case "path_prefix":
			dst = &out.PathPrefix
		case "path_suffix":
			dst = &out.PathSuffix
		case "path_pattern":
			dst = &out.PathPattern
		default:
			return fmt.Errorf("match: unknown field %q", key)
		}
		if isJSONNull(value) {
			return fmt.Errorf("match.%s: null is not allowed", key)
		}
		if err := json.Unmarshal(value, dst); err != nil {
			return fmt.Errorf("match.%s: %w", key, err)
		}
	}
	*m = out
	return nil
}

// PolicyRules is what a matched rule does. Every field is optional: an omitted
// field inherits the value already accumulated from earlier matching rules.
//
// `exit` is rule-local evaluation control, not a merged value, so it never
// appears in policyDecision. Caching is not a policy field: whether a response
// may be stored is decided after the upstream responds (X-MDB-Relay and
// Cache-Control), not before the request is issued.
//
// `immutable` is a pre-request content-addressing fact, not a cache policy: it
// says the mount-relative path names bytes that cannot change, and may capture
// their digest. It merges like traffic_policy and never changes passthrough,
// exit or the transport selection.
type PolicyRules struct {
	TrafficPolicy trafficPolicy `json:"traffic_policy,omitempty"`
	Passthrough   ruleBool      `json:"passthrough,omitempty"`
	Exit          ruleBool      `json:"exit,omitempty"`
	Immutable     ruleString    `json:"immutable,omitempty"`
}

// MarshalJSON emits only the fields the rule actually declares. The zero
// option type is an internal detail and must not leak into the debugging
// endpoint's wire shape.
func (r PolicyRules) MarshalJSON() ([]byte, error) {
	out := struct {
		TrafficPolicy *string `json:"traffic_policy,omitempty"`
		Passthrough   *bool   `json:"passthrough,omitempty"`
		Exit          *bool   `json:"exit,omitempty"`
		Immutable     *string `json:"immutable,omitempty"`
	}{}
	if r.TrafficPolicy != "" {
		name := string(r.TrafficPolicy)
		out.TrafficPolicy = &name
	}
	if r.Passthrough.set {
		v := r.Passthrough.value
		out.Passthrough = &v
	}
	if r.Exit.set {
		v := r.Exit.value
		out.Exit = &v
	}
	if r.Immutable.set {
		v := r.Immutable.value
		out.Immutable = &v
	}
	return json.Marshal(out)
}

// UnmarshalJSON rejects null, unknown fields and an explicit empty
// traffic_policy. Empty is rejected here rather than treated as omission:
// omission inherits, while an explicit empty string is a configuration error.
func (r *PolicyRules) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		return fmt.Errorf("rules: null is not allowed")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("rules: %w", err)
	}

	var out PolicyRules
	for key, value := range raw {
		switch key {
		case "traffic_policy":
			if isJSONNull(value) {
				return fmt.Errorf("rules.traffic_policy: null is not allowed")
			}
			var name string
			if err := json.Unmarshal(value, &name); err != nil {
				return fmt.Errorf("rules.traffic_policy: %w", err)
			}
			if name == "" {
				return fmt.Errorf("rules.traffic_policy: empty string is not allowed")
			}
			out.TrafficPolicy = trafficPolicy(name)
		case "passthrough":
			if err := json.Unmarshal(value, &out.Passthrough); err != nil {
				return fmt.Errorf("rules.passthrough: %w", err)
			}
		case "exit":
			if err := json.Unmarshal(value, &out.Exit); err != nil {
				return fmt.Errorf("rules.exit: %w", err)
			}
		case "immutable":
			if err := json.Unmarshal(value, &out.Immutable); err != nil {
				return fmt.Errorf("rules.immutable: %w", err)
			}
		default:
			return fmt.Errorf("rules: unknown field %q", key)
		}
	}
	*r = out
	return nil
}

// compiledPolicy is a validated policy with its regular expressions compiled.
type compiledPolicy struct {
	Policy
	pattern   *regexp.Regexp
	immutable *regexp.Regexp
}

// contentDigest is a digest an immutable pattern captured from the path: the
// algorithm named by the group plus its lowercase hex value. An empty Algorithm
// means the pattern only declared immutability and named no digest.
type contentDigest struct {
	Algorithm string
	Hex       string
}

// matchImmutable runs the merged immutable pattern against the mount-relative
// escaped path. A pattern that does not match declares nothing; a match always
// declares immutable bytes, with a digest only when exactly one of the named
// groups captured a usable value.
func matchImmutable(re *regexp.Regexp, path string) (bool, contentDigest) {
	match := re.FindStringSubmatch(path)
	if match == nil {
		return false, contentDigest{}
	}
	return true, capturedDigest(re, match)
}

// capturedDigest extracts the optional sha256/sha1 named groups. Captures are
// considered before hex validation: two non-empty groups at once are not a
// digest the cache chain can use, so picking one would be arbitrary and the
// path is reported immutable without a digest. A single capture that is not hex
// is treated the same way.
func capturedDigest(re *regexp.Regexp, match []string) contentDigest {
	var captured []contentDigest
	for i, name := range re.SubexpNames() {
		if i == 0 || i >= len(match) || match[i] == "" {
			continue
		}
		switch name {
		case "sha256", "sha1":
			captured = append(captured, contentDigest{Algorithm: name, Hex: match[i]})
		}
	}
	if len(captured) != 1 {
		return contentDigest{}
	}
	hexValue, ok := lowerHex(captured[0].Hex)
	if !ok {
		return contentDigest{}
	}
	return contentDigest{Algorithm: captured[0].Algorithm, Hex: hexValue}
}

// lowerHex reports whether s is non-empty hexadecimal and returns it lowercased.
// Length is not checked here: an unusable length simply misses in the content
// store, while accepting any hex keeps the extraction reusable across mounts.
func lowerHex(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
			out[i] = c
		case c >= 'A' && c <= 'F':
			out[i] = c - 'A' + 'a'
		default:
			return "", false
		}
	}
	return string(out), true
}

// compilePolicies validates the whole list and compiles every path pattern.
// Validation is all-or-nothing: a single invalid policy rejects the update so
// the relay keeps its last known good configuration.
func compilePolicies(in []Policy) ([]compiledPolicy, error) {
	out := make([]compiledPolicy, 0, len(in))
	seen := make(map[string]bool, len(in))

	for i, p := range in {
		if strings.TrimSpace(p.Name) == "" {
			return nil, fmt.Errorf("policy %d: name is required", i)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("policy %d: duplicate name %q", i, p.Name)
		}
		seen[p.Name] = true

		// An omitted traffic_policy inherits; only a declared one is validated.
		if p.Rules.TrafficPolicy != "" {
			switch p.Rules.TrafficPolicy {
			case trafficLatency, trafficSpeed:
			default:
				return nil, fmt.Errorf("policy %q: traffic_policy %q is not latency or speed",
					p.Name, p.Rules.TrafficPolicy)
			}
		}

		cp := compiledPolicy{Policy: p}
		if p.Match.PathPattern != "" {
			// Go's regexp is RE2: no backreferences or lookaround, so a
			// pathological pattern cannot burn CPU backtracking.
			re, err := regexp.Compile(p.Match.PathPattern)
			if err != nil {
				return nil, fmt.Errorf("policy %q: path_pattern: %w", p.Name, err)
			}
			cp.pattern = re
		}
		if p.Rules.Immutable.set {
			// The same engine validates the immutable pattern, so an invalid
			// expression rejects the whole manifest instead of surfacing on a
			// request. Named groups such as (?<sha256>…) are part of RE2.
			re, err := regexp.Compile(p.Rules.Immutable.value)
			if err != nil {
				return nil, fmt.Errorf("policy %q: immutable: %w", p.Name, err)
			}
			cp.immutable = re
		}
		out = append(out, cp)
	}

	return out, nil
}

// matches reports whether every present condition holds for one request that
// has already resolved to mirror at path.
func (p *compiledPolicy) matches(m Mirror, path string) bool {
	if p.Match.Tag != "" && !slices.Contains(m.PolicyTags, p.Match.Tag) {
		return false
	}
	if p.Match.Mirror != "" && p.Match.Mirror != m.Name {
		return false
	}
	if p.Match.PathPrefix != "" && !strings.HasPrefix(path, p.Match.PathPrefix) {
		return false
	}
	if p.Match.PathSuffix != "" && !strings.HasSuffix(path, p.Match.PathSuffix) {
		return false
	}
	if p.pattern != nil && !p.pattern.MatchString(path) {
		return false
	}
	return true
}

// policyDecision is the merged result of evaluating the policy list. It is what
// the request handler needs before the upstream request is issued:
// TrafficPolicy selects the edge pool, Passthrough tells the later cache chain
// to skip the exchange entirely because the upstream is known to stream it
// through, and Immutable records that the merged immutable pattern matched this
// path — with Digest when that pattern captured one.
type policyDecision struct {
	TrafficPolicy trafficPolicy
	Passthrough   bool
	Immutable     bool
	Digest        contentDigest
}

// defaultPolicyDecision is the value an unmatched request gets. An unmatched
// path is never immutable.
var defaultPolicyDecision = policyDecision{TrafficPolicy: trafficLatency}

// apply overlays the fields this rule declares onto d. Omitted fields inherit.
// Immutable is not applied here because the merged pattern has to be evaluated
// against the path after the last matching rule is known.
func (p *compiledPolicy) apply(d *policyDecision) {
	if p.Rules.TrafficPolicy != "" {
		d.TrafficPolicy = p.Rules.TrafficPolicy
	}
	if p.Rules.Passthrough.set {
		d.Passthrough = p.Rules.Passthrough.value
	}
}

// policyFor evaluates every matching rule in array order, shallow-merging rule
// fields at the top level: later matches win, omitted fields inherit. A
// matching rule with `exit: true` stops evaluation after its own merge; `exit`
// itself is not part of the result.
//
// The immutable pattern merges like the other fields, then the surviving
// pattern is run once against the mount-relative path. A declared pattern that
// does not match the path declares nothing — in particular it does not fall
// back to an earlier match's pattern.
func (m *manifestState) policyFor(mirror Mirror, path string) policyDecision {
	d := defaultPolicyDecision
	var immutable *regexp.Regexp
	if m != nil {
		for i := range m.policies {
			p := &m.policies[i]
			if !p.matches(mirror, path) {
				continue
			}
			p.apply(&d)
			if p.immutable != nil {
				immutable = p.immutable
			}
			if p.Rules.Exit.set && p.Rules.Exit.value {
				break
			}
		}
	}
	if immutable != nil {
		d.Immutable, d.Digest = matchImmutable(immutable, path)
	}
	return d
}

// policyList returns the policies in the manifest's wire shape, for the
// debugging endpoint.
func (m *manifestState) policyList() []Policy {
	out := make([]Policy, len(m.policies))
	for i := range m.policies {
		out[i] = m.policies[i].Policy
	}
	return out
}

// matchPolicy returns the merged policy decision for an inbound request.
//
// Only a request that resolves to a mirror in the current manifest can match a
// policy: control-plane endpoints, unknown hosts and unknown paths keep the
// latency default even when a policy has an empty match.
func (s *Server) matchPolicy(r *http.Request) policyDecision {
	st := s.manifest.Load()
	mirror, path, ok := s.resolveMirror(st, r)
	if !ok {
		return defaultPolicyDecision
	}
	return st.policyFor(mirror, path)
}

// matchTrafficPolicy selects the transport policy for an inbound request. The
// full decision is available through matchPolicy; this is the transport view.
func (s *Server) matchTrafficPolicy(r *http.Request) trafficPolicy {
	return s.matchPolicy(r).TrafficPolicy
}

// resolveMirror reports the manifest mirror an inbound request targets and the
// mount-relative path policy conditions are matched against. ok is false when
// the request does not address a specific mirror.
//
// Subdomain mounts keep the URL path unchanged; path mounts drop their first
// segment. The path is the escaped one, so escaping and case survive.
func (s *Server) resolveMirror(st *manifestState, r *http.Request) (Mirror, string, bool) {
	if st == nil {
		return Mirror{}, "", false
	}

	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}

	sub, base := s.matchBaseDomain(requestHost(r))
	if base == "" {
		// Not one of this relay's domains. It is still forwarded by the open
		// proxy, but it addresses no manifest mirror, so no policy applies.
		return Mirror{}, "", false
	}
	if sub != "" {
		for _, m := range st.mirrors {
			if m.Host != "" && m.Host == sub {
				return m, path, true
			}
		}
		return Mirror{}, "", false
	}

	// Bare base domain: the first URL segment names the mount. The mount root
	// itself is "/".
	seg, rest, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if seg == "" {
		return Mirror{}, "", false
	}
	for _, m := range st.mirrors {
		if m.Path == seg {
			return m, "/" + rest, true
		}
	}
	return Mirror{}, "", false
}

// policyDecisionKey carries the merged decision from the handler to everything
// downstream: the transport reads the traffic policy, the cache chain reads
// passthrough.
type policyDecisionKey struct{}

func withPolicyDecision(ctx context.Context, d policyDecision) context.Context {
	return context.WithValue(ctx, policyDecisionKey{}, d)
}

func policyDecisionFromContext(ctx context.Context) policyDecision {
	if d, ok := ctx.Value(policyDecisionKey{}).(policyDecision); ok {
		return d
	}
	return defaultPolicyDecision
}

// withTrafficPolicy updates only the transport field, preserving passthrough
// already placed on the context.
func withTrafficPolicy(ctx context.Context, p trafficPolicy) context.Context {
	d := policyDecisionFromContext(ctx)
	d.TrafficPolicy = p
	return withPolicyDecision(ctx, d)
}

func trafficPolicyFromContext(ctx context.Context) trafficPolicy {
	return policyDecisionFromContext(ctx).TrafficPolicy
}

// passthroughFromContext reports whether the relay already knows, before
// issuing the upstream request, that the response must bypass the cache chain.
func passthroughFromContext(ctx context.Context) bool {
	return policyDecisionFromContext(ctx).Passthrough
}

// immutableFromContext reports whether the merged policy declared the request
// path content-addressed, and the digest its pattern captured when it named
// one. The cache chain reads this before issuing the upstream request: ok with
// a zero digest still means the bytes under the path cannot change.
func immutableFromContext(ctx context.Context) (bool, contentDigest) {
	d := policyDecisionFromContext(ctx)
	return d.Immutable, d.Digest
}

// policyTransport sends each request through the pool its policy selected.
//
// The decision is taken before RoundTrip, so a response that already started
// keeps the transport it began on even if the policy snapshot changes.
type policyTransport struct {
	edge *edgeSelection
}

func (t *policyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.edge.transportFor(trafficPolicyFromContext(req.Context())).RoundTrip(req)
}
