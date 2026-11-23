package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RelayDirectiveHeader is the response control header an upstream uses to tell
// the relay how to treat that response. Its value has the shape of
// Cache-Control: comma-separated `keyword` or `keyword=value`.
const RelayDirectiveHeader = "X-MDB-Relay"

// RelayDirectives is the parsed form of X-MDB-Relay. Only the keywords the
// relay currently understands are represented; everything else is ignored.
type RelayDirectives struct {
	// NoStore means the response must not enter the relay cache at all.
	NoStore bool
	// NoVary means the URL path identifies the resource, so Vary must not split
	// it into variants.
	NoVary bool
	// TTL is the fallback freshness used only when the upstream states none.
	// It is meaningful only when TTLSet is true.
	TTL    time.Duration
	TTLSet bool
}

// ParseRelayDirectives parses one or more X-MDB-Relay field values.
//
// Boolean keywords are sticky: once a keyword is seen it stays set, so a later
// duplicate or a malformed token cannot silently cancel it. A malformed ttl is
// ignored rather than guessed, and a duplicate ttl keeps the smallest valid
// value — the safe choice when a response declares more than one fallback
// lifetime.
func ParseRelayDirectives(values []string) RelayDirectives {
	var d RelayDirectives
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}

			name, arg, _ := strings.Cut(token, "=")
			name = strings.ToLower(strings.TrimSpace(name))

			switch name {
			case "no-store":
				d.NoStore = true
			case "no-vary":
				d.NoVary = true
			case "ttl":
				seconds, ok := parseNonNegativeSeconds(strings.TrimSpace(arg))
				if !ok {
					continue
				}
				ttl := time.Duration(seconds) * time.Second
				if !d.TTLSet || ttl < d.TTL {
					d.TTL = ttl
				}
				d.TTLSet = true
			}
		}
	}
	return d
}

// RelayDirectivesFromHeader parses the X-MDB-Relay values out of a response
// header set.
func RelayDirectivesFromHeader(h http.Header) RelayDirectives {
	return ParseRelayDirectives(h.Values(RelayDirectiveHeader))
}

// parseNonNegativeSeconds accepts only plain decimal digits. A sign, fraction,
// unit suffix or overflow is invalid, so no fallback freshness is invented.
func parseNonNegativeSeconds(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	const maxTTLSeconds = int64(1<<63-1) / int64(time.Second)
	if seconds > maxTTLSeconds {
		return 0, false
	}
	return seconds, true
}

// SanitizeInboundRelayDirective drops X-MDB-Relay from a client request that
// did not come through a relay. Only the relay populates the header from an
// upstream response; a bare client must not be able to inject it. A request
// carrying X-MDB-Relay-Host came from a relay and is left untouched.
//
// This only edits request headers. Response headers are never modified here.
func SanitizeInboundRelayDirective(r *http.Request) {
	if r == nil {
		return
	}
	if r.Header.Get(RelayHostHeader) == "" {
		r.Header.Del(RelayDirectiveHeader)
	}
}
