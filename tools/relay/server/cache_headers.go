package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// hopByHopHeaders are removed in both directions. Connection-named tokens are
// handled separately.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Connection",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// storageHeaders returns the end-to-end header set persisted with a body.
// Hop-by-hop headers, credentials-bearing headers, the relay control header and
// the original Content-Length never enter the store. Age is recomputed per
// delivery. Vary, validators, Content-Encoding, Cache-Control and Expires are
// kept because delivery, conditional requests and range evaluation need them.
func storageHeaders(h http.Header) http.Header {
	out := h.Clone()
	if out == nil {
		out = make(http.Header)
	}
	stripHopByHop(out)
	out.Del("Set-Cookie")
	out.Del("Set-Cookie2")
	out.Del("WWW-Authenticate")
	out.Del(RelayDirectiveHeader)
	out.Del("Content-Length")
	out.Del("Age")
	return out
}

func stripHopByHop(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				h.Del(token)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

func stripRequestHopByHop(h http.Header) {
	stripHopByHop(h)
}

func cacheControlHas(values []string, want string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			name, _, _ := strings.Cut(strings.TrimSpace(token), "=")
			if strings.EqualFold(strings.TrimSpace(name), want) {
				return true
			}
		}
	}
	return false
}

func cacheControlSeconds(values []string, want string) (int64, bool) {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			name, arg, ok := strings.Cut(strings.TrimSpace(token), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(name), want) {
				continue
			}
			arg = strings.Trim(strings.TrimSpace(arg), `"`)
			if arg == "" {
				continue
			}
			seconds, err := strconv.ParseInt(arg, 10, 64)
			if err != nil || seconds < 0 {
				continue
			}
			return seconds, true
		}
	}
	return 0, false
}

// requestCachePolicy is the client's request-side cache control. It is a
// request fact, separate from the response-controlled store rules.
type requestCachePolicy struct {
	// noStore skips the cache entirely: no lookup, no write for this exchange.
	noStore bool
	// forceRevalidate makes a fresh entry go through a conditional revalidation
	// before it may be served.
	forceRevalidate bool
}

// requestCachePolicyFrom implements RFC 9111 request directives:
//
//   - Cache-Control: no-store skips both lookup and write;
//   - Cache-Control: no-cache or max-age=0 forces revalidation;
//   - Pragma: no-cache is the legacy equivalent, but only when the request
//     carries no Cache-Control at all.
//
// X-MDB-Relay is response control and is never treated as a request cache
// directive; inbound copies are sanitised before this runs.
func requestCachePolicyFrom(r *http.Request) requestCachePolicy {
	values := r.Header.Values("Cache-Control")
	policy := requestCachePolicy{
		noStore:         cacheControlHas(values, "no-store"),
		forceRevalidate: cacheControlHas(values, "no-cache"),
	}
	if seconds, ok := cacheControlSeconds(values, "max-age"); ok && seconds == 0 {
		policy.forceRevalidate = true
	}
	if len(values) == 0 && cacheControlHas(r.Header.Values("Pragma"), "no-cache") {
		policy.forceRevalidate = true
	}
	return policy
}

// freshnessExpiry derives the moment a stored response stops being servable
// without revalidation. A shared cache prefers s-maxage, then max-age, then
// Expires; ttl is a fallback only when the upstream stated none. no-cache
// always makes the entry stale immediately, while still allowing it to be
// stored.
func freshnessExpiry(h http.Header, directives RelayDirectives, now time.Time) time.Time {
	cacheControl := h.Values("Cache-Control")
	if cacheControlHas(cacheControl, "no-cache") {
		return now
	}
	if seconds, ok := cacheControlSeconds(cacheControl, "s-maxage"); ok {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if seconds, ok := cacheControlSeconds(cacheControl, "max-age"); ok {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if raw := strings.TrimSpace(h.Get("Expires")); raw != "" {
		if raw == "0" {
			return now
		}
		if expires, err := http.ParseTime(raw); err == nil {
			return expires
		}
	}
	if directives.TTLSet {
		return now.Add(directives.TTL)
	}
	return now
}

func varyNames(values []string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, value := range values {
		for _, name := range strings.Split(value, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

func varyContainsStar(values []string) bool {
	for _, name := range varyNames(values) {
		if name == "*" {
			return true
		}
	}
	return false
}

func ageSeconds(storedAt, now time.Time) int64 {
	if storedAt.IsZero() {
		return 0
	}
	age := int64(now.Sub(storedAt) / time.Second)
	if age < 0 {
		return 0
	}
	return age
}

type precondition int

const (
	preconditionProceed precondition = iota
	preconditionNotModified
	preconditionFailed
)

// evaluatePreconditions applies RFC 9110 conditional request semantics against
// a stored representation, without any upstream request.
func evaluatePreconditions(r *http.Request, responseHeaders http.Header) precondition {
	etag := strings.TrimSpace(responseHeaders.Get("ETag"))
	lastModified := strings.TrimSpace(responseHeaders.Get("Last-Modified"))

	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	hasIfMatch := r.Header.Values("If-Match") != nil
	if hasIfMatch {
		if ifMatch == "" {
			return preconditionProceed
		}
		if ifMatch == "*" {
			// The representation exists; * always matches.
		} else if !etagListMatches(ifMatch, etag, true) {
			return preconditionFailed
		}
	} else if value := strings.TrimSpace(r.Header.Get("If-Unmodified-Since")); value != "" && lastModified != "" {
		if since, err := http.ParseTime(value); err == nil {
			if modified, err := http.ParseTime(lastModified); err == nil && modified.After(since) {
				return preconditionFailed
			}
		}
	}

	ifNoneMatch := strings.TrimSpace(r.Header.Get("If-None-Match"))
	if r.Header.Values("If-None-Match") != nil {
		if ifNoneMatch == "*" || etagListMatches(ifNoneMatch, etag, false) {
			return preconditionNotModified
		}
		return preconditionProceed
	}
	if value := strings.TrimSpace(r.Header.Get("If-Modified-Since")); value != "" && lastModified != "" {
		if since, err := http.ParseTime(value); err == nil {
			if modified, err := http.ParseTime(lastModified); err == nil && !modified.After(since) {
				return preconditionNotModified
			}
		}
	}
	return preconditionProceed
}

// etagListMatches compares an If-Match / If-None-Match field value against one
// entity tag. If-None-Match uses weak comparison; If-Match is strong and a weak
// stored or candidate tag never matches.
func etagListMatches(list, current string, strong bool) bool {
	if current == "" {
		return false
	}
	for _, candidate := range splitETagList(list) {
		if candidate == "" {
			continue
		}
		if strong {
			if isWeakETag(candidate) || isWeakETag(current) {
				continue
			}
			if candidate == current {
				return true
			}
			continue
		}
		if opaqueETag(candidate) == opaqueETag(current) {
			return true
		}
	}
	return false
}

func splitETagList(value string) []string {
	var out []string
	start := 0
	inQuotes := false
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '"':
			inQuotes = !inQuotes
		case ',':
			if !inQuotes {
				out = append(out, strings.TrimSpace(value[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(value[start:]))
	return out
}

func isWeakETag(value string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "w/")
}

func opaqueETag(value string) string {
	value = strings.TrimSpace(value)
	if isWeakETag(value) {
		value = strings.TrimSpace(value[2:])
	}
	return value
}

// ifRangeMatches reports whether a Range request's If-Range validator agrees
// with the stored representation. Weak ETags never match; dates must equal
// Last-Modified to the second.
func ifRangeMatches(value string, h http.Header) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	if strings.HasPrefix(value, `"`) || isWeakETag(value) {
		etag := strings.TrimSpace(h.Get("ETag"))
		if isWeakETag(value) || isWeakETag(etag) {
			return false
		}
		return value == etag && etag != ""
	}
	lastModified := strings.TrimSpace(h.Get("Last-Modified"))
	if lastModified == "" {
		return false
	}
	want, err := http.ParseTime(value)
	if err != nil {
		return false
	}
	have, err := http.ParseTime(lastModified)
	if err != nil {
		return false
	}
	return want.Equal(have)
}

func stripConditionalHeaders(h http.Header) {
	for _, name := range []string{
		"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range",
	} {
		h.Del(name)
	}
}
