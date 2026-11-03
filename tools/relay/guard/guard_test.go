package guard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var key = []byte("test-secret-key")

func serve(t *testing.T, g *Guard, auth string) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/.mirror-relay/cert-pack", nil)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(w, r)
	return w.Code
}

func TestGuardAcceptsSignedToken(t *testing.T) {
	tok, err := SignToken(key)
	if err != nil {
		t.Fatal(err)
	}
	if code := serve(t, &Guard{SecretKey: key}, "Bearer "+tok); code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
}

func TestGuardRejects(t *testing.T) {
	g := &Guard{SecretKey: key}
	now := time.Now()

	mk := func(method jwt.SigningMethod, k any, c jwt.RegisteredClaims) string {
		s, err := jwt.NewWithClaims(method, c).SignedString(k)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return s
	}

	valid := func() jwt.RegisteredClaims {
		return jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		}
	}

	cases := map[string]string{
		"no header":      "",
		"missing bearer": mk(jwt.SigningMethodHS256, key, valid()),
		"expired": "Bearer " + mk(jwt.SigningMethodHS256, key, jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now.Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(now.Add(-time.Hour)),
		}),
		"no exp": "Bearer " + mk(jwt.SigningMethodHS256, key, jwt.RegisteredClaims{
			IssuedAt: jwt.NewNumericDate(now),
		}),
		"iat in the future": "Bearer " + mk(jwt.SigningMethodHS256, key, jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now.Add(time.Hour)),
			ExpiresAt: jwt.NewNumericDate(now.Add(2 * time.Hour)),
		}),
		"wrong key": "Bearer " + mk(jwt.SigningMethodHS256, []byte("other"), valid()),
		"alg none":  "Bearer " + mk(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, valid()),
	}

	for name, auth := range cases {
		if code := serve(t, g, auth); code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, code)
		}
	}
}

// The 401 body must not describe why the token failed.
func TestGuardDoesNotEchoParserErrors(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer not.a.jwt")
	(&Guard{SecretKey: key}).Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(w, r)

	body := strings.ToLower(strings.TrimSpace(w.Body.String()))
	if body == "" {
		return
	}
	for _, leak := range []string{"signature", "segment", "base64", "malformed", "expired"} {
		if strings.Contains(body, leak) {
			t.Errorf("response leaks parser internals %q: %q", leak, body)
		}
	}
}

func TestSignTokenEmptyKey(t *testing.T) {
	if _, err := SignToken(nil); err == nil {
		t.Fatal("expected error for empty secret")
	}
}
