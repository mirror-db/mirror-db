package server

import (
	"net/http"
	"net/http/httptest"
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
