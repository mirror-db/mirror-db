package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// manifestURL is the endpoint the manifest is fetched from. The configured
// upstream may carry a base path, which must be preserved.
func (s *Server) manifestURL() string {
	return strings.TrimRight(s.upstream.String(), "/") + "/api/mirrors"
}

// manifestState is one immutable snapshot of everything derived from the
// upstream manifest: mirrors, compiled policies, the keepHTTP map and the ETag
// they were loaded with. Publishing it through a single atomic pointer means a
// request never sees a half-updated routing table.
type manifestState struct {
	mirrors  []Mirror
	policies []compiledPolicy
	keepHTTP map[string]bool
	etag     string
}

// newManifestState validates a decoded manifest and derives the routing maps.
// Nothing is published here: a rejected update leaves the previous snapshot
// active.
func newManifestState(r mirrorsResponse, etag string) (*manifestState, error) {
	policies, err := compilePolicies(r.Policies)
	if err != nil {
		return nil, err
	}

	httpOK := make(map[string]bool)
	for _, m := range r.Mirrors {
		if !m.KeepHTTP {
			continue
		}
		if m.Host != "" {
			httpOK[m.Host] = true
		}
		if m.Path != "" {
			httpOK[m.Path] = true
		}
	}

	return &manifestState{
		mirrors:  r.Mirrors,
		policies: policies,
		keepHTTP: httpOK,
		etag:     etag,
	}, nil
}

// syncManifest revalidates the manifest once.
//
// The first load is a GET. Afterwards a HEAD with If-None-Match costs headers
// instead of a body; only a changed or missing ETag triggers another GET. A
// failed or invalid refresh never replaces the last known good snapshot.
func (s *Server) syncManifest() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prev := s.manifest.Load()
	if prev == nil || prev.etag == "" {
		// No validator to send: a first load, or an upstream that predates ETags.
		return s.fetchManifest(ctx)
	}

	resp, err := s.manifestRequest(ctx, http.MethodHead, prev.etag)
	if err != nil {
		return err
	}
	// HEAD must not carry a body, but consume anything a proxy adds so the
	// connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		return nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return fmt.Errorf("manifest HEAD returned %s", resp.Status)
	}

	if etag := resp.Header.Get("ETag"); etag != "" && etag == prev.etag {
		return nil
	}
	return s.fetchManifest(ctx)
}

// fetchManifest downloads, validates and atomically publishes the manifest.
func (s *Server) fetchManifest(ctx context.Context) error {
	resp, err := s.manifestRequest(ctx, http.MethodGet, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("manifest GET returned %s", resp.Status)
	}

	var result mirrorsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}

	next, err := newManifestState(result, resp.Header.Get("ETag"))
	if err != nil {
		return err
	}

	s.manifest.Store(next)
	s.ready.Store(true)
	s.loaded.Store(true)
	log.Printf("loaded %d mirrors, %d policies from upstream (etag %q)",
		len(next.mirrors), len(next.policies), next.etag)
	return nil
}

func (s *Server) manifestRequest(ctx context.Context, method, etag string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.manifestURL(), nil)
	if err != nil {
		return nil, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	return s.client.Do(req)
}
