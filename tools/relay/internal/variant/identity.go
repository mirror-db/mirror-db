// Package variant owns the variant identity: the normalised tuple of request
// header values that files one stored representation of a URL apart from its
// siblings. It is the single place that decides what a Vary-named value is,
// what "the same identity" means, and whether a request matches an identity.
//
// The storage and scheduling layers are codecs over an Identity, not owners of
// its semantics: the hash store encodes it into a bbolt key and the prefetch
// scheduler into an in-memory task key, and each keeps its own byte format.
package variant

import (
	"net/http"
	"sort"
	"strings"
)

// Identity is one variant identity: either the single-representation case
// (NoVary), or the sorted set of Vary field names plus one normalised value per
// name. Values are keyed by lowercase field name.
//
// Header absence and presence with an empty value are different identities: a
// request that did not carry a header cannot be given the representation filed
// for one that carried it empty.
type Identity struct {
	noVary bool
	vary   []string
	values http.Header
}

// New builds an identity from a response's single-representation statement and
// its Vary field values.
//
// A nil or empty Vary set, a set that normalises to nothing, or one containing
// "*" is the single-representation case and is folded into NoVary, so the same
// response can never be filed under two identities. Each Vary element may be a
// comma-separated field list; names are trimmed, lowercased, deduplicated and
// sorted.
//
// requestHeaders is the whole request header set; only the names Vary selected
// are retained. Their values are trimmed, empty ones dropped, and the rest
// joined with ", " in a deterministic order, so a request cannot produce two
// identities across runs because Go's map iteration is unordered.
func New(noVary bool, vary []string, requestHeaders http.Header) Identity {
	names := normalizeVary(vary)
	if noVary || len(names) == 0 || contains(names, "*") {
		return Identity{noVary: true, values: http.Header{}}
	}
	return Identity{vary: names, values: selectValues(requestHeaders, names)}
}

// Excluding returns the same identity with the named request headers ignored.
// It is the caller's policy hook: a relay may decide that a header is not a
// dimension without changing what an identity is for any other store user.
func (i Identity) Excluding(names ...string) Identity {
	drop := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			drop[name] = struct{}{}
		}
	}
	if i.noVary || len(drop) == 0 {
		return i
	}

	kept := make([]string, 0, len(i.vary))
	for _, name := range i.vary {
		if _, excluded := drop[name]; !excluded {
			kept = append(kept, name)
		}
	}
	if len(kept) == len(i.vary) {
		return i
	}
	if len(kept) == 0 {
		return Identity{noVary: true, values: http.Header{}}
	}

	values := make(http.Header, len(kept))
	for _, name := range kept {
		if valuesForName, ok := i.values[name]; ok {
			values[name] = append([]string(nil), valuesForName...)
		}
	}
	return Identity{vary: kept, values: values}
}

// NoVary reports the single-representation case.
func (i Identity) NoVary() bool { return i.noVary }

// Vary returns a copy of the sorted, lowercased Vary field names.
func (i Identity) Vary() []string {
	if len(i.vary) == 0 {
		return nil
	}
	return append([]string(nil), i.vary...)
}

// RequestHeaders returns a copy of the values this identity selected, keyed by
// lowercase field name. A missing key means the request did not carry that
// header; a key holding "" means it carried it empty.
func (i Identity) RequestHeaders() http.Header {
	out := make(http.Header, len(i.values))
	for name, values := range i.values {
		out[name] = append([]string(nil), values...)
	}
	return out
}

// Value returns the identity's value for one Vary field. The boolean is
// presence, which is part of the identity.
func (i Identity) Value(name string) (string, bool) {
	values, ok := i.values[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return "", false
	}
	if len(values) == 0 {
		return "", true
	}
	return values[0], true
}

// Matches reports whether requestHeaders satisfy this identity. A no-vary
// identity matches every request; otherwise each field is compared by presence
// and normalised value, exactly as New would file it.
func (i Identity) Matches(requestHeaders http.Header) bool {
	if i.noVary {
		return true
	}
	got := selectValues(requestHeaders, i.vary)
	for _, name := range i.vary {
		want, wantPresent := i.Value(name)
		have, havePresent := valueIn(got, name)
		if wantPresent != havePresent || want != have {
			return false
		}
	}
	return true
}

// Equal reports whether two identities file the same representation. Presence
// is compared, so an absent header never equals a present-but-empty one.
func (i Identity) Equal(other Identity) bool {
	if i.noVary || other.noVary {
		return i.noVary == other.noVary
	}
	if len(i.vary) != len(other.vary) {
		return false
	}
	for idx, name := range i.vary {
		if other.vary[idx] != name {
			return false
		}
		want, wantPresent := i.Value(name)
		have, havePresent := other.Value(name)
		if wantPresent != havePresent || want != have {
			return false
		}
	}
	return true
}

// normalizeVary splits comma-separated Vary field values, trims and lowercases
// each name, drops empty and duplicate entries, and sorts the result.
func normalizeVary(vary []string) []string {
	seen := make(map[string]struct{}, len(vary))
	out := make([]string, 0, len(vary))
	for _, value := range vary {
		for _, name := range strings.Split(value, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			if _, duplicate := seen[name]; duplicate {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// selectValues keeps only the named headers and reduces each to one value.
// Header names are case-insensitive, so the map key is lowercased before
// grouping; a request may carry the same field under several casings, and
// those entries are merged in sorted original-name order so the result does not
// depend on map iteration order. A field the request did not carry stays
// absent; one it carried only as empty strings stays present with "".
func selectValues(h http.Header, vary []string) http.Header {
	type entry struct {
		name   string
		values []string
	}
	grouped := make(map[string][]entry, len(h))
	for name, values := range h {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		grouped[key] = append(grouped[key], entry{name: name, values: values})
	}

	out := make(http.Header, len(vary))
	for _, name := range vary {
		entries := grouped[name]
		if len(entries) == 0 {
			continue
		}
		if len(entries) > 1 {
			sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
		}
		var parts []string
		for _, e := range entries {
			for _, value := range e.values {
				if value = strings.TrimSpace(value); value != "" {
					parts = append(parts, value)
				}
			}
		}
		out[name] = []string{strings.Join(parts, ", ")}
	}
	return out
}

func valueIn(h http.Header, name string) (string, bool) {
	values, ok := h[name]
	if !ok {
		return "", false
	}
	if len(values) == 0 {
		return "", true
	}
	return values[0], true
}
