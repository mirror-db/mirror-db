package server

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// Normalised APT metadata index (M3).
//
// The Worker publishes a schema-1 JSON document at <mount>/.mdb/apt-index.json.
// The relay keeps only an in-memory, atomically replaced snapshot per apt-tagged
// mirror. It never builds client cache variants for mutable Release/Packages
// objects: the index is a separate, Worker-produced generation.
const (
	aptIndexSchemaVersion = 1
	// maxAPTIndexBytes bounds one index document. 50k files with a few hashes
	// each stay far below this; the limit only prevents an unbounded body from
	// a broken upstream.
	maxAPTIndexBytes = 32 << 20
	// defaultAPTIndexTTL keeps an already-expired generation from becoming a
	// tight refresh loop. The Worker normally states a real expiry.
	defaultAPTIndexTTL = time.Hour
)

// aptIndex is one immutable, validated generation.
type aptIndex struct {
	Schema      int
	Mirror      string
	GeneratedAt time.Time
	ExpiresAt   time.Time
	Suites      []aptSuite
	Files       []aptFile
	byPath      map[string]aptFile
}

type aptSuite struct {
	Suite       string
	ReleasePath string
	Date        string
	ValidUntil  string
	SHA256      string
	SHA1        string
}

type aptFile struct {
	Path   string
	Size   int64
	SHA256 string
	SHA1   string
	Kind   string
}

// aptIndexTable is the per-mirror snapshot set. A snapshot pointer is never
// mutated after publication, so readers need only the read lock.
type aptIndexTable struct {
	mu       sync.RWMutex
	byMirror map[string]*aptGeneration
}

// aptGeneration is one published index generation plus its effective delivery
// deadline. A 304 may extend the deadline of the current generation without
// changing its content, so the deadline lives beside the immutable index rather
// than inside it.
type aptGeneration struct {
	idx *aptIndex
	// until is when this generation stops being authoritative for direct
	// delivery. It starts at the document's expires_at; a 304 that states
	// freshness may extend it, never shorten it.
	until time.Time
}

// set publishes a new generation. Its deadline is the document's own expiry.
func (t *aptIndexTable) set(mirror string, idx *aptIndex) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byMirror == nil {
		t.byMirror = make(map[string]*aptGeneration)
	}
	t.byMirror[mirror] = &aptGeneration{idx: idx, until: idx.ExpiresAt}
}

// extend moves the current generation's deadline to until when that is later.
// It never shortens, and it does nothing without a published generation.
func (t *aptIndexTable) extend(mirror string, until time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	current := t.byMirror[mirror]
	if current == nil || !until.After(current.until) {
		return
	}
	t.byMirror[mirror] = &aptGeneration{idx: current.idx, until: until}
}

func (t *aptIndexTable) generation(mirror string) *aptGeneration {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byMirror[mirror]
}

func (t *aptIndexTable) get(mirror string) *aptIndex {
	gen := t.generation(mirror)
	if gen == nil {
		return nil
	}
	return gen.idx
}

type aptIndexJSON struct {
	Schema      *int            `json:"schema"`
	Mirror      *string         `json:"mirror"`
	GeneratedAt *string         `json:"generated_at"`
	ExpiresAt   *string         `json:"expires_at"`
	Suites      *[]aptSuiteJSON `json:"suites"`
	Files       *[]aptFileJSON  `json:"files"`
}

type aptSuiteJSON struct {
	Suite       *string `json:"suite"`
	ReleasePath *string `json:"release_path"`
	Date        *string `json:"date"`
	ValidUntil  *string `json:"valid_until"`
	SHA256      *string `json:"sha256"`
	SHA1        *string `json:"sha1"`
}

type aptFileJSON struct {
	Path   *string `json:"path"`
	Size   *int64  `json:"size"`
	SHA256 *string `json:"sha256"`
	SHA1   *string `json:"sha1"`
	Kind   *string `json:"kind"`
}

// parseAPTIndex validates the whole document. A single bad field rejects the
// generation; the previous snapshot (if any) stays active and is never used for
// direct delivery once stale.
func parseAPTIndex(data []byte, wantMirror string) (*aptIndex, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var raw aptIndexJSON
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("apt index: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, err
	}

	if raw.Schema == nil {
		return nil, errors.New("apt index: schema is required")
	}
	if *raw.Schema != aptIndexSchemaVersion {
		return nil, fmt.Errorf("apt index: unsupported schema %d", *raw.Schema)
	}
	if raw.Mirror == nil || strings.TrimSpace(*raw.Mirror) == "" {
		return nil, errors.New("apt index: mirror is required")
	}
	if wantMirror != "" && *raw.Mirror != wantMirror {
		return nil, fmt.Errorf("apt index: mirror %q does not match %q", *raw.Mirror, wantMirror)
	}
	if raw.GeneratedAt == nil || raw.ExpiresAt == nil {
		return nil, errors.New("apt index: generated_at and expires_at are required")
	}
	generated, err := time.Parse(time.RFC3339, *raw.GeneratedAt)
	if err != nil {
		return nil, fmt.Errorf("apt index: generated_at: %w", err)
	}
	expires, err := time.Parse(time.RFC3339, *raw.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("apt index: expires_at: %w", err)
	}
	if !expires.After(generated) {
		return nil, errors.New("apt index: expires_at must be after generated_at")
	}
	if raw.Suites == nil || raw.Files == nil {
		return nil, errors.New("apt index: suites and files are required")
	}

	idx := &aptIndex{
		Schema:      *raw.Schema,
		Mirror:      *raw.Mirror,
		GeneratedAt: generated,
		ExpiresAt:   expires,
		byPath:      make(map[string]aptFile, len(*raw.Files)),
	}
	for i, s := range *raw.Suites {
		suite, err := parseAPTSuite(s)
		if err != nil {
			return nil, fmt.Errorf("apt index: suite %d: %w", i, err)
		}
		idx.Suites = append(idx.Suites, suite)
	}
	for i, f := range *raw.Files {
		file, err := parseAPTFile(f)
		if err != nil {
			return nil, fmt.Errorf("apt index: file %d: %w", i, err)
		}
		if previous, ok := idx.byPath[file.Path]; ok {
			if previous != file {
				return nil, fmt.Errorf("apt index: conflicting entries for %q", file.Path)
			}
			continue
		}
		idx.byPath[file.Path] = file
		idx.Files = append(idx.Files, file)
	}
	return idx, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("apt index: trailing JSON value")
		}
		return fmt.Errorf("apt index: trailing data: %w", err)
	}
	return nil
}

func parseAPTSuite(s aptSuiteJSON) (aptSuite, error) {
	if s.Suite == nil || strings.TrimSpace(*s.Suite) == "" {
		return aptSuite{}, errors.New("suite is required")
	}
	if s.ReleasePath == nil || !safeIndexPath(*s.ReleasePath) {
		return aptSuite{}, fmt.Errorf("release_path %q is not a safe root-relative path", derefString(s.ReleasePath))
	}
	out := aptSuite{Suite: *s.Suite, ReleasePath: *s.ReleasePath}
	out.Date = derefString(s.Date)
	out.ValidUntil = derefString(s.ValidUntil)
	for label, value := range map[string]string{"date": out.Date, "valid_until": out.ValidUntil} {
		if value == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			return aptSuite{}, fmt.Errorf("%s: %w", label, err)
		}
	}
	var err error
	if out.SHA256, err = optionalDigest(hashstore.SHA256, s.SHA256); err != nil {
		return aptSuite{}, err
	}
	if out.SHA1, err = optionalDigest(hashstore.SHA1, s.SHA1); err != nil {
		return aptSuite{}, err
	}
	return out, nil
}

func parseAPTFile(f aptFileJSON) (aptFile, error) {
	if f.Path == nil || !safeIndexPath(*f.Path) {
		return aptFile{}, fmt.Errorf("path %q is not a safe root-relative path", derefString(f.Path))
	}
	if f.Size == nil {
		return aptFile{}, errors.New("size is required")
	}
	if *f.Size < 0 {
		return aptFile{}, fmt.Errorf("size %d is negative", *f.Size)
	}
	if f.Kind == nil {
		return aptFile{}, errors.New("kind is required")
	}
	kind := *f.Kind
	if kind != "metadata" && kind != "package" {
		return aptFile{}, fmt.Errorf("kind %q is not metadata or package", kind)
	}
	out := aptFile{Path: *f.Path, Size: *f.Size, Kind: kind}
	var err error
	if out.SHA256, err = optionalDigest(hashstore.SHA256, f.SHA256); err != nil {
		return aptFile{}, err
	}
	if out.SHA1, err = optionalDigest(hashstore.SHA1, f.SHA1); err != nil {
		return aptFile{}, err
	}
	if kind == "package" && out.SHA256 == "" && out.SHA1 == "" {
		return aptFile{}, errors.New("package needs sha256 or sha1")
	}
	return out, nil
}

func optionalDigest(algo hashstore.Algorithm, value *string) (string, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return "", nil
	}
	normalized, err := hashstore.NormalizeDigest(algo, *value)
	if err != nil {
		return "", err
	}
	return normalized, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// safeIndexPath accepts only a clean, root-relative path with no traversal.
// The map is keyed by this exact string, so a path that would normalise to a
// different value is rejected rather than silently rewritten.
func safeIndexPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\?#") {
		return false
	}
	if strings.ContainsRune(p, 0) || path.Clean(p) != p || p == "." || p == ".." {
		return false
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// -- background fetch --

func (s *Server) runAPTIndexTask(ctx context.Context, task prefetchTask) prefetchOutcome {
	now := s.currentTime()
	req := s.syntheticRequest(ctx, task.mirror, task.clientPath, task.clientEscapedPath, "", http.MethodGet)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if task.aptEtag != "" {
		req.Header.Set("If-None-Match", task.aptEtag)
	}
	out, err := s.buildUpstreamRequest(ctx, req)
	if err != nil {
		return prefetchOutcome{err: err}
	}
	out = s.withMirrorPolicy(out, task.mirror, task.relPath)

	resp, err := s.roundTrip(out)
	if err != nil {
		return prefetchOutcome{err: err}
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		now = s.currentTime()
		directives := RelayDirectivesFromHeader(resp.Header)
		fresh := freshnessExpiry(resp.Header, directives, now)
		next := fresh
		if gen := s.aptIndexes.generation(task.mirror.Name); gen != nil {
			// A 304 is the upstream confirming the stored document is still
			// current. Its freshness may therefore extend how long that
			// generation stays authoritative for direct delivery, which the
			// document's own expires_at no longer describes.
			if fresh.After(now) && fresh.After(gen.until) {
				s.aptIndexes.extend(task.mirror.Name, fresh)
			}
			if gen.until.After(next) {
				next = gen.until
			}
		}
		if !next.After(now) {
			// No usable freshness: the generation keeps its old deadline (so it
			// is not used for direct delivery) and the retry waits a full default
			// interval instead of spinning on a zero-length one.
			next = now.Add(defaultAPTIndexTTL)
		}
		return prefetchOutcome{next: next, etag: resp.Header.Get("ETag")}
	case http.StatusOK:
		data, err := readLimited(resp.Body, maxAPTIndexBytes)
		if err != nil {
			return prefetchOutcome{err: err}
		}
		idx, err := parseAPTIndex(data, task.mirror.Name)
		if err != nil {
			return prefetchOutcome{err: err}
		}
		// A new document is authoritative for exactly what it states; a previous
		// 304 extension never carries over to it.
		s.aptIndexes.set(task.mirror.Name, idx)
		next := idx.ExpiresAt
		if !next.After(now) {
			next = now.Add(defaultAPTIndexTTL)
		}
		return prefetchOutcome{next: next, etag: resp.Header.Get("ETag"), etagSet: true}
	default:
		_, _ = io.Copy(io.Discard, resp.Body)
		return prefetchOutcome{err: fmt.Errorf("apt index returned %s", resp.Status)}
	}
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("apt index exceeds %d bytes", limit)
	}
	return data, nil
}

// -- hash prediction delivery --

// freshAPTFile returns the index entry covering path, but only while the whole
// generation is fresh. A stale index is not authoritative for any path. The
// generation carries the effective deadline, which a 304 may have extended
// beyond the document's own expires_at.
func (s *Server) freshAPTFile(m Mirror, relPath string, now time.Time) (aptFile, *aptGeneration, bool) {
	gen := s.aptIndexes.generation(m.Name)
	if gen == nil || !gen.until.After(now) {
		return aptFile{}, gen, false
	}
	lookup := strings.TrimPrefix(relPath, "/")
	if unescaped, err := url.PathUnescape(lookup); err == nil {
		lookup = unescaped
	}
	file, ok := gen.idx.byPath[lookup]
	if !ok {
		return aptFile{}, gen, false
	}
	return file, gen, true
}

// variantMatchesExpectedFile reports whether a stored variant's body is the one
// the fresh index names. Only a package entry may invalidate a stored variant:
// metadata paths (Release, Packages, ...) are mutable descriptions of the
// repository, and the index's hash for them describes the fetched metadata
// object, not the client-cached body. An index entry without SHA-256 is checked
// through the store's SHA-1 alias; if that mapping is unavailable the variant
// is left alone rather than guessed away.
func (s *Server) variantMatchesExpectedFile(ctx context.Context, v hashstore.Variant, file aptFile) bool {
	if file.Kind != "package" {
		return true
	}
	if file.SHA256 != "" {
		return strings.EqualFold(v.BodySHA256, file.SHA256)
	}
	if file.SHA1 == "" {
		return true
	}
	sha256, _, ok, err := s.hashStore.Resolve(ctx, hashstore.SHA1, file.SHA1)
	if err != nil || !ok || sha256 == "" {
		return true
	}
	return strings.EqualFold(v.BodySHA256, sha256)
}

// servePredicted delivers a package body already in the content store when the
// current fresh index says this path has that exact hash. It reuses the M2 hit
// path through a temporary variant and never writes variant metadata.
//
// It returns false when the request should take the ordinary miss path: not a
// package, no body, a size mismatch, an unusable address, or a delivery feature
// the M2 hit path cannot satisfy.
func (s *Server) servePredicted(w http.ResponseWriter, r *http.Request, key, path string, file aptFile, gen *aptGeneration) bool {
	if file.Kind != "package" {
		return false
	}
	if gen == nil || !gen.until.After(s.currentTime()) {
		return false
	}
	// The stored body is identity-coded and carries a content digest, so it
	// must not be transcoded. Delivering identity is legal whenever the client
	// accepts it; negotiate against that instead of letting a gzip preference
	// turn a verifiable body into an upstream fetch.
	if !AcceptEncodingFromRequest(r).Accepts(CodingIdentity) {
		return false
	}
	r = r.Clone(r.Context())
	r.Header.Set("Accept-Encoding", "identity")

	ctx := r.Context()
	var (
		size   int64
		sha256 string
		sha1   string
		found  bool
	)
	candidates := []struct {
		algo   hashstore.Algorithm
		digest string
	}{
		{hashstore.SHA256, file.SHA256},
		{hashstore.SHA1, file.SHA1},
	}
	for _, candidate := range candidates {
		if candidate.digest == "" {
			continue
		}
		gotSize, exists, err := s.hashStore.Stat(ctx, candidate.algo, candidate.digest)
		if err != nil {
			log.Printf("relay prefetch: stat %s/%s: %v", candidate.algo, candidate.digest, err)
			continue
		}
		if !exists || gotSize != file.Size {
			if exists {
				log.Printf("relay prefetch: %q stored size %d, index size %d", path, gotSize, file.Size)
			}
			continue
		}
		primary, alias, ok, err := s.hashStore.Resolve(ctx, candidate.algo, candidate.digest)
		if err != nil || !ok || primary == "" {
			if err != nil {
				log.Printf("relay prefetch: resolve %s/%s: %v", candidate.algo, candidate.digest, err)
			}
			continue
		}
		size, sha256, sha1, found = gotSize, primary, alias, true
		break
	}
	if !found {
		return false
	}

	now := s.currentTime()
	v := hashstore.Variant{
		URL:             key,
		NoVary:          true,
		BodySHA256:      sha256,
		BodySize:        size,
		ResponseHeaders: predictedHeaders(path, sha256, sha1, file, gen, now),
		StoredAt:        now,
		ExpiresAt:       gen.until,
	}
	return s.serveHit(w, r, key, path, v)
}

// predictedHeaders builds the only headers a hash-predicted delivery may state.
// Nothing here is copied from an upstream response: the content digest is the
// validator, the content type is derived from the path, and freshness is capped
// by the index generation.
func predictedHeaders(path string, sha256, sha1 string, file aptFile, gen *aptGeneration, now time.Time) http.Header {
	h := make(http.Header)
	h.Set("Content-Type", predictedContentType(path))
	h.Set("Accept-Ranges", "bytes")
	if sha256 != "" {
		h.Set("ETag", `"sha256:`+sha256+`"`)
	} else if sha1 != "" {
		h.Set("ETag", `"sha1:`+sha1+`"`)
	}
	maxAge := int64(gen.until.Sub(now) / time.Second)
	if maxAge < 0 {
		maxAge = 0
	}
	h.Set("Cache-Control", "public, max-age="+strconv.FormatInt(maxAge, 10))
	if digest := digestHeader(file.SHA256, file.SHA1); digest != "" {
		h.Set("Digest", digest)
	}
	return h
}

func predictedContentType(requestPath string) string {
	switch strings.ToLower(path.Ext(requestPath)) {
	case ".deb", ".udeb":
		return "application/vnd.debian.binary-package"
	case ".gz":
		return "application/gzip"
	case ".xz":
		return "application/x-xz"
	case ".bz2":
		return "application/x-bzip2"
	case ".zst":
		return "application/zstd"
	default:
		return "application/octet-stream"
	}
}

// digestHeader emits RFC 3230-style field values for the hashes the index
// states. A digest header also tells the M2 delivery path that transcoding is
// forbidden, so the package body bytes stay exactly what was verified.
func digestHeader(sha256, sha1 string) string {
	var parts []string
	if raw, err := hex.DecodeString(sha256); err == nil && len(raw) == 32 {
		parts = append(parts, "sha-256="+base64.StdEncoding.EncodeToString(raw))
	}
	if raw, err := hex.DecodeString(sha1); err == nil && len(raw) == 20 {
		parts = append(parts, "sha-1="+base64.StdEncoding.EncodeToString(raw))
	}
	return strings.Join(parts, ", ")
}
