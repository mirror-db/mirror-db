package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMatchBaseDomain(t *testing.T) {
	s := &Server{baseDomains: []string{"relay.example.com", "relay-alt.example.org"}}

	cases := []struct {
		host, sub, base string
	}{
		{"relay.example.com", "", "relay.example.com"},
		{"dcr.relay.example.com", "dcr", "relay.example.com"},
		{"a.b.relay.example.com", "a.b", "relay.example.com"},
		{"relay-alt.example.org", "", "relay-alt.example.org"},
		{"npm.relay-alt.example.org", "npm", "relay-alt.example.org"},
		{"unknown.example.net", "", ""},
		// Suffix must be label-aligned: a domain that merely ends with the base
		// domain's characters is not a subdomain.
		{"evilrelay.example.com", "", ""},
		{"relay.example.com.evil.net", "", ""},
		{"", "", ""},
		{"com", "", ""},
	}

	for _, c := range cases {
		sub, base := s.matchBaseDomain(c.host)
		if sub != c.sub || base != c.base {
			t.Errorf("matchBaseDomain(%q) = (%q,%q), want (%q,%q)", c.host, sub, base, c.sub, c.base)
		}
	}
}

func TestUpstreamHostFor(t *testing.T) {
	s := &Server{
		upstream:    mustURL(t, "https://mirs.uk"),
		baseDomains: []string{"relay.example.com"},
	}
	cases := []struct{ host, want string }{
		{"dcr.relay.example.com", "dcr.mirs.uk"},
		{"relay.example.com", "mirs.uk"},
		{"other.example.net", "mirs.uk"},
	}
	for _, c := range cases {
		if got := s.upstreamHostFor(c.host); got != c.want {
			t.Errorf("upstreamHostFor(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

func TestRequestHost(t *testing.T) {
	cases := []struct {
		host       string
		xForwarded string
		want       string
	}{
		{"dcr.relay.example.com", "", "dcr.relay.example.com"},
		{"DCR.Relay.Example.COM:443", "", "dcr.relay.example.com"},
		{"10.0.0.1:8080", "dcr.relay.example.com", "dcr.relay.example.com"},
		{"10.0.0.1:8080", "dcr.relay.example.com, proxy.internal", "dcr.relay.example.com"},
		{"dcr.relay.example.com.", "", "dcr.relay.example.com"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = c.host
		if c.xForwarded != "" {
			r.Header.Set("X-Forwarded-Host", c.xForwarded)
		}
		if got := requestHost(r); got != c.want {
			t.Errorf("requestHost(%q, xfh=%q) = %q, want %q", c.host, c.xForwarded, got, c.want)
		}
	}
}

func TestShouldKeepHTTP(t *testing.T) {
	s := &Server{baseDomains: []string{"relay.example.com"}}
	st := &manifestState{keepHTTP: map[string]bool{"apt": true, "dcr": true}}
	s.manifest.Store(st)

	cases := []struct {
		target string
		host   string
		want   bool
	}{
		{"/apt/debian/dists/stable/Release", "relay.example.com", true},
		{"/npm/pkg", "relay.example.com", false},
		{"/v2/library/nginx/manifests/latest", "dcr.relay.example.com", true},
		{"/v2/library/nginx/manifests/latest", "ghcr.relay.example.com", false},
		{"/", "relay.example.com", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, c.target, nil)
		r.Host = c.host
		if got := s.ShouldKeepHTTP(r); got != c.want {
			t.Errorf("ShouldKeepHTTP(%s @ %s) = %v, want %v", c.target, c.host, got, c.want)
		}
	}
}

func TestTokenForCertName(t *testing.T) {
	tokens := map[string]string{
		"a.com":      "tok-a",
		"b.org":      "tok-b",
		"deep.c.net": "tok-c",
	}

	cases := []struct{ name, want string }{
		{"a.com", "tok-a"},
		{"*.a.com", "tok-a"},
		{"x.a.com", "tok-a"},
		{"*.x.a.com", "tok-a"},
		{"b.org", "tok-b"},
		{"*.b.org", "tok-b"},
		{"relay.deep.c.net", "tok-c"},
		{"unknown.net", ""},
		// Must not walk all the way up to a public suffix.
		{"x.y.com", ""},
	}

	for _, c := range cases {
		if got := tokenForCertName(c.name, tokens); got != c.want {
			t.Errorf("tokenForCertName(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}
