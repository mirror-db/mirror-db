package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseRelayDirectives(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		want   RelayDirectives
	}{
		{"absent", nil, RelayDirectives{}},
		{"empty", []string{""}, RelayDirectives{}},
		{"no-store", []string{"no-store"}, RelayDirectives{NoStore: true}},
		{"no-store case-insensitive", []string{"NO-STORE"}, RelayDirectives{NoStore: true}},
		{"no-vary", []string{"no-vary"}, RelayDirectives{NoVary: true}},
		{"prefetch", []string{"prefetch"}, RelayDirectives{Prefetch: true}},
		{"prefetch case-insensitive", []string{"PREFETCH"}, RelayDirectives{Prefetch: true}},
		{"prefetch with argument", []string{"prefetch=1"}, RelayDirectives{Prefetch: true}},
		{"both", []string{"no-store, no-vary"}, RelayDirectives{NoStore: true, NoVary: true}},
		{"whitespace", []string{"  no-store ,  no-vary  "}, RelayDirectives{NoStore: true, NoVary: true}},
		{"unknown ignored", []string{"fresh, no-vary, stale-while-revalidate=60"}, RelayDirectives{NoVary: true}},
		{"ttl", []string{"ttl=600"}, RelayDirectives{TTL: 10 * time.Minute, TTLSet: true}},
		{"ttl zero", []string{"ttl=0"}, RelayDirectives{TTL: 0, TTLSet: true}},
		{"ttl whitespace", []string{" ttl = 60 "}, RelayDirectives{TTL: time.Minute, TTLSet: true}},
		{"ttl leading zero", []string{"ttl=005"}, RelayDirectives{TTL: 5 * time.Second, TTLSet: true}},
		{"ttl negative", []string{"ttl=-1"}, RelayDirectives{}},
		{"ttl signed", []string{"ttl=+5"}, RelayDirectives{}},
		{"ttl fraction", []string{"ttl=1.5"}, RelayDirectives{}},
		{"ttl unit", []string{"ttl=60s"}, RelayDirectives{}},
		{"ttl empty", []string{"ttl="}, RelayDirectives{}},
		{"ttl bare", []string{"ttl"}, RelayDirectives{}},
		{"ttl garbage", []string{"ttl=abc"}, RelayDirectives{}},
		{"ttl extra equals", []string{"ttl=10=20"}, RelayDirectives{}},
		{"ttl overflow", []string{"ttl=999999999999999999999"}, RelayDirectives{}},
		{"ttl too large for duration", []string{"ttl=9223372037"}, RelayDirectives{}},
		{"duplicate booleans", []string{"no-store, no-store"}, RelayDirectives{NoStore: true}},
		{"duplicate ttl keeps smallest", []string{"ttl=30, ttl=10"}, RelayDirectives{TTL: 10 * time.Second, TTLSet: true}},
		{"duplicate ttl reverse", []string{"ttl=10, ttl=30"}, RelayDirectives{TTL: 10 * time.Second, TTLSet: true}},
		{"invalid ttl then valid", []string{"ttl=nope, ttl=30"}, RelayDirectives{TTL: 30 * time.Second, TTLSet: true}},
		{"valid ttl then invalid", []string{"ttl=30, ttl=nope"}, RelayDirectives{TTL: 30 * time.Second, TTLSet: true}},
		{"multiple header lines", []string{"no-store, ttl=60", "no-vary"}, RelayDirectives{
			NoStore: true, NoVary: true, TTL: time.Minute, TTLSet: true,
		}},
		// A boolean directive is not cancelled by an unexpected argument; the
		// keyword is what matters, and honouring it is the safe reading.
		{"boolean with argument", []string{"no-store=1, no-vary=true"}, RelayDirectives{NoStore: true, NoVary: true}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseRelayDirectives(c.values)
			if got != c.want {
				t.Fatalf("ParseRelayDirectives(%q) = %+v, want %+v", c.values, got, c.want)
			}
		})
	}
}

// TestParseRelayDirectivesImmutable covers the response-declared immutability
// keywords, including every way a digest can be ignored or cancelled.
func TestParseRelayDirectivesImmutable(t *testing.T) {
	const (
		sha256  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		sha256b = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
		sha1    = "0123456789abcdef0123456789abcdef01234567"
		sha1b   = "fedcba9876543210fedcba9876543210fedcba98"
	)

	cases := []struct {
		name   string
		values []string
		want   RelayDirectives
	}{
		{"immutable alone", []string{"immutable"}, RelayDirectives{Immutable: true}},
		{"immutable case-insensitive", []string{"IMMUTABLE"}, RelayDirectives{Immutable: true}},
		{"immutable with argument", []string{"immutable=1"}, RelayDirectives{Immutable: true}},
		{"sha256 alone does not imply immutable", []string{"sha256=" + sha256}, RelayDirectives{SHA256: sha256}},
		{"sha1 alone does not imply immutable", []string{"sha1=" + sha1}, RelayDirectives{SHA1: sha1}},
		{"both digests", []string{"immutable, sha256=" + sha256 + ", sha1=" + sha1}, RelayDirectives{
			Immutable: true, SHA256: sha256, SHA1: sha1,
		}},
		{"uppercase digest is normalised", []string{"sha256=" + strings.ToUpper(sha256)}, RelayDirectives{SHA256: sha256}},
		{"whitespace around digest", []string{" sha256 = " + sha256 + " "}, RelayDirectives{SHA256: sha256}},
		{"same digest repeated", []string{"sha256=" + sha256, "sha256=" + sha256}, RelayDirectives{SHA256: sha256}},
		{"conflicting sha256 clears it", []string{"immutable, sha256=" + sha256 + ", sha256=" + sha256b}, RelayDirectives{Immutable: true}},
		{"conflict then repeat stays cleared", []string{"sha256=" + sha256 + ", sha256=" + sha256b + ", sha256=" + sha256}, RelayDirectives{}},
		{"conflicting sha1 clears only sha1", []string{"sha256=" + sha256 + ", sha1=" + sha1 + ", sha1=" + sha1b}, RelayDirectives{SHA256: sha256}},
		{"invalid sha256 ignored", []string{"sha256=abc"}, RelayDirectives{}},
		{"invalid then valid sha256", []string{"sha256=abc, sha256=" + sha256}, RelayDirectives{SHA256: sha256}},
		{"valid then invalid sha256 keeps value", []string{"sha256=" + sha256 + ", sha256=zz"}, RelayDirectives{SHA256: sha256}},
		{"wrong length sha256 ignored", []string{"sha256=" + sha1}, RelayDirectives{}},
		{"wrong length sha1 ignored", []string{"sha1=" + sha256}, RelayDirectives{}},
		{"empty digest ignored", []string{"sha256=", "sha1"}, RelayDirectives{}},
		{"digest with algorithm prefix ignored", []string{"sha256=sha256:" + sha256}, RelayDirectives{}},
		{"digest with extra equals ignored", []string{"sha256=" + sha256 + "=x"}, RelayDirectives{}},
		{"digest alongside other keywords", []string{"no-store, immutable, sha1=" + sha1 + ", ttl=60"}, RelayDirectives{
			NoStore: true, Immutable: true, SHA1: sha1, TTL: time.Minute, TTLSet: true,
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseRelayDirectives(c.values); got != c.want {
				t.Fatalf("ParseRelayDirectives(%q) = %+v, want %+v", c.values, got, c.want)
			}
		})
	}
}

func TestRelayDirectivesExpectation(t *testing.T) {
	const (
		sha256  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		sha256b = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
		sha1    = "0123456789abcdef0123456789abcdef01234567"
	)
	cases := []struct {
		name     string
		values   []string
		wantAlgo string
		want     string
	}{
		{"none", nil, "", ""},
		{"sha256 only", []string{"sha256=" + sha256}, "sha256", sha256},
		{"sha1 only", []string{"sha1=" + sha1}, "sha1", sha1},
		{"sha256 wins", []string{"sha1=" + sha1 + ", sha256=" + sha256}, "sha256", sha256},
		{"sha256 wins regardless of order", []string{"sha256=" + sha256 + ", sha1=" + sha1}, "sha256", sha256},
		{"cancelled sha256 falls back to sha1", []string{"sha256=" + sha256 + ", sha256=" + sha256b + ", sha1=" + sha1}, "sha1", sha1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			algo, digest := ParseRelayDirectives(c.values).Expectation()
			if string(algo) != c.wantAlgo || digest != c.want {
				t.Fatalf("Expectation() = %q %q, want %q %q", algo, digest, c.wantAlgo, c.want)
			}
		})
	}
}

func TestRelayDirectivesFromHeader(t *testing.T) {
	h := http.Header{}
	h.Add(RelayDirectiveHeader, "no-store")
	h.Add(RelayDirectiveHeader, "ttl=15")
	got := RelayDirectivesFromHeader(h)
	want := RelayDirectives{NoStore: true, TTL: 15 * time.Second, TTLSet: true}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSanitizeInboundRelayDirective(t *testing.T) {
	t.Run("drops client-supplied directive", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/latest", nil)
		r.Header.Set(RelayDirectiveHeader, "no-store")
		SanitizeInboundRelayDirective(r)
		if got := r.Header.Get(RelayDirectiveHeader); got != "" {
			t.Fatalf("X-MDB-Relay survived: %q", got)
		}
	})

	t.Run("keeps relay-supplied directive", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(RelayDirectiveHeader, "no-vary")
		r.Header.Set(RelayHostHeader, "relay.example.com")
		SanitizeInboundRelayDirective(r)
		if got := r.Header.Get(RelayDirectiveHeader); got != "no-vary" {
			t.Fatalf("X-MDB-Relay = %q, want no-vary", got)
		}
	})

	t.Run("empty relay host is not a relay", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(RelayDirectiveHeader, "no-store")
		r.Header.Set(RelayHostHeader, "")
		SanitizeInboundRelayDirective(r)
		if got := r.Header.Get(RelayDirectiveHeader); got != "" {
			t.Fatalf("X-MDB-Relay survived: %q", got)
		}
	})

	t.Run("nil request", func(t *testing.T) {
		SanitizeInboundRelayDirective(nil)
	})
}

// TestWithPersistentLifetime keeps orthogonal directives and replaces only the
// lifetime. It never invents storage: the store rules already refused the
// prohibitions before it runs, and any that remain are removed here.
func TestWithPersistentLifetime(t *testing.T) {
	h := http.Header{}
	h.Set("Cache-Control", "public, max-age=60, no-transform, s-maxage=30, no-cache")
	h.Set("Expires", "Mon, 02 Jan 2006 15:04:05 GMT")
	withPersistentLifetime(h)

	got := h.Get("Cache-Control")
	if h.Get("Expires") != "" {
		t.Fatalf("Expires survived: %q", h.Get("Expires"))
	}
	for _, want := range []string{"public", "no-transform", "max-age=31536000"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Cache-Control %q lost %q", got, want)
		}
	}
	for _, unwanted := range []string{"max-age=60", "s-maxage", "no-cache"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("Cache-Control %q kept %q", got, unwanted)
		}
	}
}
