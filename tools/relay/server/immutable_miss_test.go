package server

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"testing"
)

const (
	missSHA1    = "0123456789abcdef0123456789abcdef01234567"
	missSHA1b   = "fedcba9876543210fedcba9876543210fedcba98"
	missSHA256  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	missSHA256b = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	missHex128  = missSHA256 + missSHA256
	missHex41   = missSHA1 + "0"
	missHex63   = missSHA1 + "0123456789abcdef0123456"
)

// TestImmutableMissCandidates pins the operator's expression exactly, including
// the false negatives it necessarily has: no lookaround or boundary was added,
// so the surrounding characters are part of the match.
func TestImmutableMissCandidates(t *testing.T) {
	cases := []struct {
		name string
		path string
		want []string
	}{
		{"sha1 between delimiters", "/blob/" + missSHA1 + ".bin", []string{"sha1:" + missSHA1}},
		{"sha256 between delimiters", "/blob/" + missSHA256 + ".bin", []string{"sha256:" + missSHA256}},
		{"uppercase is normalised", "/blob/" + strings.ToUpper(missSHA1) + "/x", []string{"sha1:" + missSHA1}},
		{"no hex", "/blob/thing.bin", nil},
		// A digest at the start or the end has no surrounding non-alphanumeric
		// character and is therefore not reported.
		{"at path start", missSHA1 + "/blob", nil},
		{"at path end", "/blob/" + missSHA1, nil},
		{"at path end with sha256", "/blob/" + missSHA256, nil},
		// An escaped delimiter ends in a hexadecimal letter, so the rune before
		// the digest is alphanumeric and the candidate is hidden.
		{"escaped delimiter", "/blob%2F" + missSHA1 + "%2Ffile", nil},
		{"escaped space", "/blob%20" + missSHA1 + "%20file", nil},
		// Only exactly 40 or 64 hex fit the expression.
		{"41 hex", "/blob/" + missHex41 + "/x", nil},
		{"63 hex", "/blob/" + missHex63 + "/x", nil},
		// Two digests with no delimiter form one 128 character hex run. The
		// expression cannot start inside an alphanumeric run, so a run longer than
		// 64 characters yields no candidate at all.
		{"128 hex before delimiter", "/blob/" + missHex128 + "/x", nil},
		{"128 hex at path end", "/blob/" + missHex128, nil},
		// Non-overlapping matching means two digests sharing one delimiter report
		// only the first.
		{"adjacent digests share a delimiter", "/blob/" + missSHA1 + "/" + missSHA1b + "/x", []string{"sha1:" + missSHA1}},
		// Multiple separated candidates are all reported, in order.
		{"two candidates", "/a/" + missSHA1 + "/b/" + missSHA256 + "/c", []string{"sha1:" + missSHA1, "sha256:" + missSHA256}},
		// A non-ASCII delimiter is one rune, not one byte, and is still one
		// non-alphanumeric character for the expression.
		{"non-ascii delimiter", "/blob/中" + missSHA1 + "中/x", []string{"sha1:" + missSHA1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := immutableMissCandidates(c.path)
			if len(got) != len(c.want) {
				t.Fatalf("candidates(%q) = %v, want %v", c.path, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("candidates(%q) = %v, want %v", c.path, got, c.want)
				}
			}
		})
	}
}

// captureLog runs fn while collecting everything written through the standard
// logger.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })
	fn()
	log.SetOutput(previous)
	return buf.String()
}

func immutableMissTestServer(t *testing.T, policies []Policy) *Server {
	t.Helper()
	_, upSrv := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s, _ := newCacheServer(t, upSrv.URL, cacheServerOptions{
		clock:    newTestClock(),
		mirrors:  []Mirror{{Name: "files", Path: "files"}},
		policies: policies,
	})
	return s
}

func TestImmutableMissLogSwitch(t *testing.T) {
	path := "/files/blob/" + missSHA256 + ".bin"

	t.Run("disabled by default", func(t *testing.T) {
		s := immutableMissTestServer(t, nil)
		if s.logImmutableMiss {
			t.Fatal("diagnostic is enabled by default")
		}
		out := captureLog(t, func() {
			cacheRequest(s, http.MethodGet, path, nil)
		})
		if strings.Contains(out, "immutable miss") {
			t.Fatalf("disabled diagnostic logged: %q", out)
		}
	})

	t.Run("policy miss is reported", func(t *testing.T) {
		s := immutableMissTestServer(t, []Policy{{
			Name:  "immutable-other",
			Match: PolicyMatch{PathPrefix: "/other/"},
			Rules: PolicyRules{Immutable: stringRule(`^/other/`)},
		}})
		s.logImmutableMiss = true
		out := captureLog(t, func() {
			cacheRequest(s, http.MethodGet, path, nil)
		})
		if !strings.Contains(out, "relay cache immutable miss") {
			t.Fatalf("policy miss was not logged: %q", out)
		}
		if !strings.Contains(out, "mirror=files") {
			t.Fatalf("log line has no mirror: %q", out)
		}
		if !strings.Contains(out, "method=GET") {
			t.Fatalf("log line has no method: %q", out)
		}
		if !strings.Contains(out, "path=/blob/"+missSHA256+".bin") {
			t.Fatalf("log line has no mount-relative path: %q", out)
		}
		if !strings.Contains(out, "candidates=sha256:"+missSHA256) {
			t.Fatalf("log line has no candidate: %q", out)
		}
	})

	t.Run("a matching policy suppresses the line", func(t *testing.T) {
		s := immutableMissTestServer(t, []Policy{{
			Name:  "immutable-blob",
			Match: PolicyMatch{PathPrefix: "/blob/"},
			Rules: PolicyRules{Immutable: stringRule(`^/blob/.+`)},
		}})
		s.logImmutableMiss = true
		out := captureLog(t, func() {
			cacheRequest(s, http.MethodGet, path, nil)
		})
		if strings.Contains(out, "immutable miss") {
			t.Fatalf("policy hit was logged: %q", out)
		}
	})

	t.Run("a declared pattern that misses is a policy miss", func(t *testing.T) {
		// The policy matches the request by prefix but its immutable expression
		// does not name this path, so nothing declared immutability.
		s := immutableMissTestServer(t, []Policy{{
			Name:  "immutable-deep",
			Match: PolicyMatch{PathPrefix: "/blob/"},
			Rules: PolicyRules{Immutable: stringRule(`^/blob/deep/`)},
		}})
		s.logImmutableMiss = true
		out := captureLog(t, func() {
			cacheRequest(s, http.MethodGet, path, nil)
		})
		if !strings.Contains(out, "immutable miss") {
			t.Fatalf("non-matching immutable expression was treated as a hit: %q", out)
		}
	})

	t.Run("passthrough is covered", func(t *testing.T) {
		s := immutableMissTestServer(t, []Policy{{
			Name:  "pass",
			Rules: PolicyRules{Passthrough: boolRule(true)},
		}})
		s.logImmutableMiss = true
		out := captureLog(t, func() {
			cacheRequest(s, http.MethodGet, path, nil)
		})
		if !strings.Contains(out, "immutable miss") {
			t.Fatalf("passthrough request was not reported: %q", out)
		}
	})

	t.Run("unresolved mirror is not reported", func(t *testing.T) {
		s := immutableMissTestServer(t, nil)
		s.logImmutableMiss = true
		out := captureLog(t, func() {
			cacheRequest(s, http.MethodGet, "/unknown/blob/"+missSHA256+".bin", nil)
		})
		if strings.Contains(out, "immutable miss") {
			t.Fatalf("unresolved request was reported: %q", out)
		}
	})

	t.Run("query is never logged", func(t *testing.T) {
		s := immutableMissTestServer(t, nil)
		s.logImmutableMiss = true
		out := captureLog(t, func() {
			cacheRequest(s, http.MethodGet, path+"?token=super-secret", nil)
		})
		var lines []string
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "immutable miss") {
				lines = append(lines, line)
			}
		}
		if len(lines) != 1 {
			t.Fatalf("diagnostic lines = %v, want 1", lines)
		}
		if strings.Contains(lines[0], "super-secret") || strings.Contains(lines[0], "token=") {
			t.Fatalf("log line leaked the query: %q", lines[0])
		}
	})
}
