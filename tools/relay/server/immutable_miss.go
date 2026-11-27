package server

import (
	"log"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Immutable-miss diagnostic (CACHE_LOG_IMMUTABLE_MISS).
//
// It reports a request that resolved to a mirror and whose merged policy did not
// declare immutability, while its mount-relative path contains a digest-shaped
// run. That combination usually means a manifest is missing an `immutable`
// pattern, so the operator can widen policy instead of paying a MISS for every
// request.
//
// The expression is the operator's own, deliberately unmodified:
//
//	[^0-9a-zA-Z][0-9a-fA-F]{40}([0-9a-fA-F]{24})?[^0-9a-zA-Z]
//
// No lookaround or boundary was added. Its consequences are part of the
// behaviour and documented in README and ADR 0017:
//
//   - a digest at the very start or end of the path has no surrounding
//     non-alphanumeric rune, so it is not reported;
//   - a 41..63 character hex run fits neither 40 nor 64 and is not reported, and
//     a run longer than 64 characters yields no candidate either: the expression
//     must start on a non-alphanumeric character, so it cannot be entered from
//     inside the run and neither 40 nor 64 fits before the run ends;
//   - escaped paths are matched as escaped, so a delimiter written as an escape
//     sequence is several characters, not one;
//   - two digests sharing one delimiter are found as one non-overlapping match,
//     so the second is not reported.
const immutableMissPatternSource = `[^0-9a-zA-Z][0-9a-fA-F]{40}([0-9a-fA-F]{24})?[^0-9a-zA-Z]`

var immutableMissPattern = regexp.MustCompile(immutableMissPatternSource)

// immutableMissCandidates returns the digest-shaped runs of a mount-relative
// path as `sha1:<40 hex>` / `sha256:<64 hex>`, lowercase, in match order. The
// surrounding non-alphanumeric runes are part of the match and are removed
// before classification.
func immutableMissCandidates(path string) []string {
	var out []string
	for _, match := range immutableMissPattern.FindAllStringSubmatchIndex(path, -1) {
		start, end := match[0], match[1]
		_, lead := utf8.DecodeRuneInString(path[start:end])
		_, tail := utf8.DecodeLastRuneInString(path[start:end])
		if lead+tail >= end-start {
			continue
		}
		hexValue := strings.ToLower(path[start+lead : end-tail])
		switch len(hexValue) {
		case 40:
			out = append(out, "sha1:"+hexValue)
		case 64:
			out = append(out, "sha256:"+hexValue)
		}
	}
	return out
}

// noteImmutableMiss logs one resolved request when the diagnostic is enabled and
// the path carries a digest-shaped run. The switch is checked first, so no path
// is scanned while it is off. Mirror name, method, mount-relative path and the
// candidates are logged; the query string, request headers and client address
// never are.
func (s *Server) noteImmutableMiss(r *http.Request, mirror Mirror, path string) {
	if s == nil || !s.logImmutableMiss {
		return
	}
	candidates := immutableMissCandidates(path)
	if len(candidates) == 0 {
		return
	}
	log.Printf("relay cache immutable miss: mirror=%s method=%s path=%s candidates=%s",
		mirror.Name, r.Method, path, strings.Join(candidates, ","))
}
