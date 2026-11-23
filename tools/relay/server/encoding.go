package server

import (
	"math"
	"net/http"
	"strconv"
	"strings"
)

// ContentCoding is one of the content codings the relay can store and transcode.
type ContentCoding string

const (
	CodingIdentity ContentCoding = "identity"
	CodingGzip     ContentCoding = "gzip"
	CodingDeflate  ContentCoding = "deflate"
	CodingBrotli   ContentCoding = "br"
	CodingZstd     ContentCoding = "zstd"
)

// supportedCodings is the tie-break order after the stored coding: zstd, br,
// gzip, deflate, then identity. Identity is kept last because an explicitly
// accepted non-identity coding normally beats an identity that is acceptable
// only by default.
var supportedCodings = []ContentCoding{
	CodingZstd,
	CodingBrotli,
	CodingGzip,
	CodingDeflate,
	CodingIdentity,
}

// ParseContentCoding parses a Content-Encoding value case-insensitively. Only
// the codings the relay can decode are accepted; an absent or unsupported
// coding reports false.
func ParseContentCoding(value string) (ContentCoding, bool) {
	switch ContentCoding(strings.ToLower(strings.TrimSpace(value))) {
	case CodingIdentity:
		return CodingIdentity, true
	case CodingGzip:
		return CodingGzip, true
	case CodingDeflate:
		return CodingDeflate, true
	case CodingBrotli:
		return CodingBrotli, true
	case CodingZstd:
		return CodingZstd, true
	default:
		return "", false
	}
}

// ContentCodingFromHeaders returns the coding named by Content-Encoding,
// treating an absent (or empty) header as identity. Every Content-Encoding field
// line is joined before parsing: a body with more than one coding layer reports
// false, whether the layers are comma-separated in one line or spread over
// several lines. The relay never decodes just one layer and pretends the result
// was identity. An unsupported value also reports false; the caller must then
// bypass transcoding rather than guess.
func ContentCodingFromHeaders(h http.Header) (ContentCoding, bool) {
	values := h.Values("Content-Encoding")
	if len(values) == 0 {
		return CodingIdentity, true
	}

	joined := strings.Join(values, ",")
	var coding string
	for _, token := range strings.Split(joined, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if coding != "" {
			return "", false
		}
		coding = token
	}
	if coding == "" {
		return CodingIdentity, true
	}
	return ParseContentCoding(coding)
}

// Supported reports whether the relay can decode and encode this coding.
func (c ContentCoding) Supported() bool {
	switch c {
	case CodingIdentity, CodingGzip, CodingDeflate, CodingBrotli, CodingZstd:
		return true
	default:
		return false
	}
}

// AcceptEncoding is a parsed Accept-Encoding preference. The zero value (or a
// nil value list passed to ParseAcceptEncoding) means the header was absent and
// every coding is acceptable.
type AcceptEncoding struct {
	absent      bool
	empty       bool
	explicit    map[ContentCoding]float64
	wildcard    float64
	hasWildcard bool
}

// ParseAcceptEncoding parses one or more Accept-Encoding field values.
//
// A nil/empty value list means the header was absent: every coding is
// acceptable and the stored coding is preferred. A present but empty field
// value ("") means no coding is acceptable, per RFC 9110.
//
// Duplicate codings keep the smallest quality: a client that says
// `gzip;q=1, gzip;q=0` has rejected gzip, and a relay must not override that.
// An invalid quality is treated as q=0 for the coding that named it. Unknown
// coding names and parameters other than q are ignored.
func ParseAcceptEncoding(values []string) AcceptEncoding {
	a := AcceptEncoding{explicit: make(map[ContentCoding]float64)}
	if len(values) == 0 {
		a.absent = true
		return a
	}

	seenToken := false
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			seenToken = true

			parts := strings.Split(token, ";")
			name := strings.ToLower(strings.TrimSpace(parts[0]))
			q := parseQValue(parts[1:])

			switch name {
			case "*":
				if !a.hasWildcard || q < a.wildcard {
					a.wildcard = q
				}
				a.hasWildcard = true
			default:
				coding, ok := ParseContentCoding(name)
				if !ok {
					continue
				}
				if existing, ok := a.explicit[coding]; !ok || q < existing {
					a.explicit[coding] = q
				}
			}
		}
	}
	if !seenToken {
		a.empty = true
	}
	return a
}

// parseQValue extracts the q parameter from the semicolon-separated parameters
// of one Accept-Encoding token. Missing q defaults to 1; a malformed or
// out-of-range q is q=0, and repeated q parameters keep the smallest value.
func parseQValue(params []string) float64 {
	q := 1.0
	qSeen := false
	for _, param := range params {
		name, value, ok := strings.Cut(param, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}
		parsed, valid := parseQ(strings.TrimSpace(value))
		if !valid {
			parsed = 0
		}
		if !qSeen || parsed < q {
			q = parsed
		}
		qSeen = true
	}
	return q
}

func parseQ(value string) (float64, bool) {
	if value == "" {
		return 0, false
	}
	q, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(q) || math.IsInf(q, 0) || q < 0 || q > 1 {
		return 0, false
	}
	return q, true
}

// AcceptEncodingFromRequest parses a request's Accept-Encoding header.
func AcceptEncodingFromRequest(r *http.Request) AcceptEncoding {
	if r == nil {
		return ParseAcceptEncoding(nil)
	}
	return ParseAcceptEncoding(r.Header.Values("Accept-Encoding"))
}

// Absent reports whether the request carried no Accept-Encoding at all. The
// zero value counts as absent, so an uninitialised preference accepts every
// coding and keeps the stored one.
func (a AcceptEncoding) Absent() bool {
	return a.absent || (a.explicit == nil && !a.empty && !a.hasWildcard)
}

// Quality returns the effective quality of a coding. Identity is acceptable by
// default unless identity;q=0 or *;q=0 excludes it. An absent header gives every
// coding q=1.
func (a AcceptEncoding) Quality(c ContentCoding) float64 {
	if a.Absent() {
		return 1
	}
	if a.empty {
		return 0
	}
	if q, ok := a.explicit[c]; ok {
		return q
	}
	if c == CodingIdentity {
		if a.hasWildcard && a.wildcard == 0 {
			return 0
		}
		return 1
	}
	if a.hasWildcard {
		return a.wildcard
	}
	return 0
}

// Accepts reports whether the client accepts the coding with a non-zero
// quality.
func (a AcceptEncoding) Accepts(c ContentCoding) bool {
	return a.Quality(c) > 0
}

// Negotiate chooses the coding to deliver for a body currently stored as
// stored. It returns the target coding, whether the body has to be transcoded,
// and false when no acceptable coding exists (the caller must then answer from
// upstream). stored must be one of the supported codings; an empty or unknown
// value returns false rather than being treated as identity.
//
// The highest quality wins. On a tie an explicitly listed coding beats an
// identity that is only acceptable by default; otherwise the stored coding wins,
// then zstd, br, gzip and deflate. An absent Accept-Encoding always keeps the
// stored coding.
func (a AcceptEncoding) Negotiate(stored ContentCoding) (target ContentCoding, transcode bool, ok bool) {
	// stored is the coding the cached bytes are actually in. An empty or
	// unknown value is never coerced to identity: a caller that forgot to read
	// Content-Encoding would otherwise deliver undecoded bytes with the header
	// removed. Refuse to negotiate instead.
	if !stored.Supported() {
		return "", false, false
	}
	if a.Absent() {
		return stored, false, true
	}

	bestQ := 0.0
	var best []ContentCoding
	for _, coding := range supportedCodings {
		q := a.Quality(coding)
		if q <= 0 {
			continue
		}
		switch {
		case q > bestQ+1e-9:
			bestQ = q
			best = append(best[:0], coding)
		case math.Abs(q-bestQ) <= 1e-9:
			best = append(best, coding)
		}
	}
	if len(best) == 0 {
		return "", false, false
	}

	// Explicitly listed non-identity codings beat an identity that is only
	// acceptable because the default rules make it so.
	if _, identityExplicit := a.explicit[CodingIdentity]; !identityExplicit {
		for _, coding := range best {
			if coding == CodingIdentity {
				continue
			}
			if q, listed := a.explicit[coding]; listed && math.Abs(q-bestQ) <= 1e-9 {
				best = removeCoding(best, CodingIdentity)
				break
			}
		}
	}

	if containsCoding(best, stored) {
		return stored, false, true
	}
	for _, coding := range supportedCodings {
		if coding == CodingIdentity {
			continue
		}
		if containsCoding(best, coding) {
			return coding, true, true
		}
	}
	if containsCoding(best, CodingIdentity) {
		return CodingIdentity, true, true
	}
	return "", false, false
}

func containsCoding(codings []ContentCoding, want ContentCoding) bool {
	for _, coding := range codings {
		if coding == want {
			return true
		}
	}
	return false
}

func removeCoding(codings []ContentCoding, drop ContentCoding) []ContentCoding {
	out := codings[:0]
	for _, coding := range codings {
		if coding != drop {
			out = append(out, coding)
		}
	}
	return out
}

// CacheControlNoTransform reports whether a Cache-Control header carries the
// no-transform directive. Matching is case-insensitive; any parameters after
// the directive name are ignored.
func CacheControlNoTransform(values []string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			name, _, _ := strings.Cut(strings.TrimSpace(token), "=")
			if strings.EqualFold(strings.TrimSpace(name), "no-transform") {
				return true
			}
		}
	}
	return false
}

// HasContentDigest reports whether a response carries a header that describes
// the stored bytes by digest. Such a body must never be transcoded, because the
// new bytes would contradict the untouched digest.
func HasContentDigest(h http.Header) bool {
	for _, name := range []string{"Content-MD5", "Digest", "Docker-Content-Digest"} {
		if strings.TrimSpace(h.Get(name)) != "" {
			return true
		}
	}
	return false
}

// IsOCIDigestPath reports whether a request path addresses an OCI object by
// digest (`/blobs/sha256:…`, `/manifests/sha256:…`). Digest-addressed content
// must not be transcoded.
func IsOCIDigestPath(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		i := strings.LastIndexByte(segment, ':')
		if i <= 0 || i == len(segment)-1 {
			continue
		}
		algorithm := strings.ToLower(segment[:i])
		if j := strings.LastIndexByte(algorithm, '@'); j >= 0 {
			algorithm = algorithm[j+1:]
		}
		digest := segment[i+1:]
		switch algorithm {
		case "sha256":
			if len(digest) == 64 && isHex(digest) {
				return true
			}
		case "sha512":
			if len(digest) == 128 && isHex(digest) {
				return true
			}
		}
	}
	return false
}

func isHex(value string) bool {
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// TranscodingForbidden reports whether a stored response must be answered from
// upstream instead of transcoded: Cache-Control: no-transform, a content-digest
// header, or an OCI path addressed by digest.
func TranscodingForbidden(h http.Header, requestPath string) bool {
	return CacheControlNoTransform(h.Values("Cache-Control")) ||
		HasContentDigest(h) ||
		IsOCIDigestPath(requestPath)
}

// ApplyTranscodedResponseHeaders rewrites a stored response header set for a
// transcoded delivery to target.
//
// Headers describing the stored bytes are removed (Content-Length, Content-MD5,
// Digest, Docker-Content-Digest), a strong ETag is demoted to weak, and
// Content-Encoding is set to the target (or removed for identity). Everything
// else is left alone.
//
// This function only edits headers. A HEAD delivery calls it and sends no body;
// it never invokes Transcode and therefore produces no bytes.
func ApplyTranscodedResponseHeaders(h http.Header, target ContentCoding) {
	if h == nil {
		return
	}
	h.Del("Content-Length")
	h.Del("Content-MD5")
	h.Del("Digest")
	h.Del("Docker-Content-Digest")
	demoteETag(h)

	if target == "" || target == CodingIdentity {
		h.Del("Content-Encoding")
		return
	}
	h.Set("Content-Encoding", string(target))
}

// demoteETag turns every strong ETag into its weak form. Already weak tags are
// left byte-for-byte identical.
func demoteETag(h http.Header) {
	values := h.Values("ETag")
	if len(values) == 0 {
		return
	}
	h.Del("ETag")
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(value), "w/") {
			value = "W/" + value
		}
		h.Add("ETag", value)
	}
}
