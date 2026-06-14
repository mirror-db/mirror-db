package guard

import (
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Guard validates JWT tokens on protected endpoints.
// Trust boundary is the shared secret key; no domain claim needed.
type Guard struct {
	SecretKey []byte
}

// Wrap returns middleware that validates JWT before passing to handler.
func (g *Guard) Wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.validate(w, r) {
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (g *Guard) validate(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	tokenStr := strings.TrimPrefix(auth, "Bearer ")
	if tokenStr == auth || tokenStr == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return false
	}

	claims := &jwt.RegisteredClaims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		return g.SecretKey, nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return false
	}
	if !token.Valid {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return false
	}

	iat, err := claims.GetIssuedAt()
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return false
	}
	if iat.Time.Before(time.Now().Add(-5 * time.Minute)) {
		http.Error(w, "token expired", http.StatusUnauthorized)
		return false
	}

	return true
}

// SignToken creates a JWT for authenticating to another relay's cert-pack.
func SignToken(secretKey []byte) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	return token.SignedString(secretKey)
}
