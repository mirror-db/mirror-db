package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// trafficPolicy is the transport preference a policy rule selects. It is a
// preference, not an availability requirement: the caller falls back to the
// latency pool when the speed pool is empty.
type trafficPolicy string

const (
	trafficLatency trafficPolicy = "latency"
	trafficSpeed   trafficPolicy = "speed"
)

// Policy mirrors one entry of the manifest's ordered `policies` array. Order is
// part of the public contract: the first complete match wins.
type Policy struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Match       PolicyMatch `json:"match"`
	Rules       PolicyRules `json:"rules"`
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

// PolicyRules is what a matched policy does. The only rule in this version is
// the traffic policy.
type PolicyRules struct {
	TrafficPolicy trafficPolicy `json:"traffic_policy"`
}

// compiledPolicy is a validated policy with its regular expression compiled.
type compiledPolicy struct {
	Policy
	pattern *regexp.Regexp
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

		switch p.Rules.TrafficPolicy {
		case trafficLatency, trafficSpeed:
		default:
			return nil, fmt.Errorf("policy %q: traffic_policy %q is not latency or speed",
				p.Name, p.Rules.TrafficPolicy)
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

// policyFor returns the traffic policy of the first policy that matches the
// resolved mirror and mount-relative path. Unmatched requests prefer latency.
func (m *manifestState) policyFor(mirror Mirror, path string) trafficPolicy {
	if m != nil {
		for i := range m.policies {
			if m.policies[i].matches(mirror, path) {
				return m.policies[i].Rules.TrafficPolicy
			}
		}
	}
	return trafficLatency
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

// matchTrafficPolicy selects the transport policy for an inbound request.
//
// Only a request that resolves to a mirror in the current manifest can match a
// policy: control-plane endpoints, unknown hosts and unknown paths keep the
// latency default even when a policy has an empty match.
func (s *Server) matchTrafficPolicy(r *http.Request) trafficPolicy {
	st := s.manifest.Load()
	mirror, path, ok := s.resolveMirror(st, r)
	if !ok {
		return trafficLatency
	}
	return st.policyFor(mirror, path)
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

// trafficPolicyKey carries the request's selected policy from the handler to the
// per-request RoundTripper.
type trafficPolicyKey struct{}

func withTrafficPolicy(ctx context.Context, p trafficPolicy) context.Context {
	return context.WithValue(ctx, trafficPolicyKey{}, p)
}

func trafficPolicyFromContext(ctx context.Context) trafficPolicy {
	if p, ok := ctx.Value(trafficPolicyKey{}).(trafficPolicy); ok {
		return p
	}
	return trafficLatency
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
