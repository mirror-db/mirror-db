package server

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

const hashContentPrefix = "/hash-content/"

// handleHashContent serves the public, read-only content addresses:
//
//	GET|HEAD /hash-content/sha1/<40 hex>
//	GET|HEAD /hash-content/sha256/<64 hex>
//
// It is mounted before the proxy catch-all and does not consult the manifest, so
// a browser can fetch a known body even if this relay has no upstream snapshot.
// Anything else is a cache MISS and falls back upstream.
func (s *Server) handleHashContent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	algo, digest, ok := parseHashContentPath(r.URL.Path)
	if !ok {
		http.Error(w, "invalid content address: want /hash-content/{sha1|sha256}/<hex>", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if r.Method == http.MethodHead {
		size, found, err := s.hashStore.Stat(ctx, algo, digest)
		if err != nil && !errors.Is(err, hashstore.ErrNotFound) {
			log.Printf("hash content stat %s/%s: %v", algo, digest, err)
			http.Error(w, "content store error", http.StatusBadGateway)
			return
		}
		if !found {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	body, size, err := s.hashStore.Get(ctx, algo, digest)
	if err != nil {
		if errors.Is(err, hashstore.ErrNotFound) || errors.Is(err, hashstore.ErrInvalidDigest) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		log.Printf("hash content get %s/%s: %v", algo, digest, err)
		http.Error(w, "content store error", http.StatusBadGateway)
		return
	}
	defer body.Close()

	// Content is addressable by digest, so it is immutable. No Range support in
	// M1: a reader always gets the whole body.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil && ctx.Err() == nil {
		log.Printf("hash content stream %s/%s: %v", algo, digest, err)
	}
}

// parseHashContentPath validates everything before it reaches the store: exactly
// two path segments and a digest of the right length for the algorithm.
func parseHashContentPath(path string) (hashstore.Algorithm, string, bool) {
	rest, ok := strings.CutPrefix(path, hashContentPrefix)
	if !ok {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	algo, err := hashstore.ParseAlgorithm(parts[0])
	if err != nil {
		return "", "", false
	}
	digest, err := hashstore.NormalizeDigest(algo, parts[1])
	if err != nil {
		return "", "", false
	}
	return algo, digest, true
}
