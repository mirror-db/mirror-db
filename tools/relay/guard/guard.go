package guard

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// clockSkew tolerates small drift between relay tiers.
const clockSkew = 30 * time.Second

var (
	errNoToken      = errors.New("missing bearer token")
	errInvalidToken = errors.New("invalid token")
)

// Guard validates JWT tokens on protected endpoints.
// Trust boundary is the shared secret key; no domain claim needed.
type Guard struct {
	SecretKey []byte
}

// Wrap returns middleware that validates JWT before passing to handler.
func (g *Guard) Wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := g.Check(r); err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer`)
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Check validates the bearer token on r. Shared by the middleware and by
// callers that need the verdict without a ResponseWriter.
func (g *Guard) Check(r *http.Request) error {
	auth := r.Header.Get("Authorization")
	tokenStr := strings.TrimPrefix(auth, "Bearer ")
	if tokenStr == auth || tokenStr == "" {
		return errNoToken
	}

	claims := &jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		return g.SecretKey, nil
	},
		// Pin the algorithm: without this a token signed with another alg (or
		// "none") could slip through depending on library defaults.
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(clockSkew),
	)
	if err != nil {
		// Wrapped, not returned raw: callers must not echo parser internals.
		return errInvalidToken
	}

	iat, err := claims.GetIssuedAt()
	if err != nil || iat == nil {
		return errInvalidToken
	}
	// Issued in the future beyond skew → clock tampering or replay.
	if iat.Time.After(time.Now().Add(clockSkew)) {
		return errInvalidToken
	}

	return nil
}

// SignToken creates a short-lived JWT for authenticating to another relay's
// cert-pack endpoint.
func SignToken(secretKey []byte) (string, error) {
	if len(secretKey) == 0 {
		return "", errors.New("empty secret key")
	}
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now.Add(-clockSkew)),
		ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
	})
	return token.SignedString(secretKey)
}
