// Package hashstore is a content-addressed store for immutable bodies. One body
// is written once, keyed by its real SHA-256, and is readable through either its
// SHA-1 or SHA-256 address. Capacity is managed by a byte-weighted LRU over the
// logical body bytes, bounded by CACHE_MAX_SIZE (the LRU budget, which is also
// the largest single body); the bbolt index keeps the accounting, the aliases and
// the LRU order across restarts.
//
// Bodies live either in a local directory (CACHE_DIR/data) or in a shared
// S3-compatible bucket (RustFS). Everything else — temp files, the index — is
// always local. A shared bucket is opportunistic reuse only: any relay may
// delete any object, so reads that lose the race are a cache MISS, not an error.
//
// The same local index also holds response-variant metadata: a URL plus the
// Vary contract and matched request headers, naming one body. It is deliberately
// local, never written to the shared bucket, and is removed in the same
// transaction that removes the body it names.
package hashstore

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/google/go-lru"
	"github.com/mirror-db/mirror-db/tools/relay/diskspace"
)

var (
	// ErrNotFound means the body is definitely absent. Callers treat it as a
	// cache MISS and fall back to the upstream. A transient or permission error
	// is never reported as ErrNotFound.
	ErrNotFound = errors.New("hashstore: content not found")
	// ErrUnreadable means the body exists but cannot be served (permissions,
	// I/O error, something else in its place). It is not a cache MISS.
	ErrUnreadable = errors.New("hashstore: body is unreadable")
	// ErrOversized means the body is larger than CACHE_MAX_SIZE and was not
	// stored. It never evicts anything else.
	ErrOversized = errors.New("hashstore: content exceeds max object size")
	// ErrInvalidDigest means the algorithm or hex length was invalid.
	ErrInvalidDigest = errors.New("hashstore: invalid digest")
	// ErrDigestMismatch means an adopted body's real digest did not match the
	// caller's expectation. Nothing was published and no existing body was
	// touched.
	ErrDigestMismatch = errors.New("hashstore: adopted content digest mismatch")
	// ErrInvalidAdopt means the adopt source is not a completed regular file of
	// the declared size inside the store's temp directory. Nothing was
	// published.
	ErrInvalidAdopt = errors.New("hashstore: invalid adopt source")
	// ErrInvalidRange means a range offset is negative or past the body's end.
	ErrInvalidRange = errors.New("hashstore: invalid range offset")
	// ErrClosed means the store is shutting down.
	ErrClosed = errors.New("hashstore: store is closed")
)

const (
	dataDirName   = "data"
	tmpDirName    = ".tmp"
	indexFileName = ".index.db"

	cleanupTimeout = 30 * time.Second
)

// Content identifies one stored body.
type Content struct {
	SHA1   string
	SHA256 string
	Size   int64
}

// Stats is a point-in-time view of the store's accounting. Capacity is the LRU
// budget (CACHE_MAX_SIZE), which is also the largest single body the store will
// keep.
type Stats struct {
	Objects  int
	Bytes    int64
	Capacity int64
}

// Store is one relay's view of the content store.
type Store struct {
	tmpDir  string
	maxSize int64
	backend backend
	index   *index
	// space guards local writes with the configured free-space watermark. It is
	// disabled (watermark 0) unless CACHE_DISK_WATER_MARK is set; reads and
	// deletions never consult it.
	space *diskspace.Checker

	// mu guards the index, the LRU and the two together: a record and its
	// in-memory weight must never disagree.
	mu  sync.Mutex
	lru lru.Cache[objectMeta]

	// publishing counts digests whose body is already visible in storage but not
	// yet in the index. A reconcile pass must not treat them as orphans or
	// pending deletions. A count, not a set: two concurrent PUTs of the same body
	// must both finish before the protection is dropped.
	publishing map[string]int

	// testAfterPublish, testCommitError and testDigestRead are test seams and nil
	// in production. The first runs after a body is published and before its
	// index commit, so a test can hold a publish inside the reconcile-protected
	// window; it runs without s.mu, but must not call back into the store. The
	// second can fail a commit to model an index write failure; it runs with s.mu
	// held, so it must not call the store either. The third marks a full reread
	// performed only to compute digests, so a test can distinguish Adopt from
	// AdoptTrusted without measuring I/O.
	testAfterPublish func()
	testCommitError  func() error
	testDigestRead   func()

	// opMu gates public operations against Close. Operations hold a read lock
	// and re-check closed; Close flips closed first, then takes the write lock
	// so it returns only after every in-flight operation has finished.
	opMu      sync.RWMutex
	closeOnce sync.Once
	closeErr  error

	wg     sync.WaitGroup
	closed atomic.Bool
}

// Open builds a store from cfg. A nil S3 section keeps bodies local; a non-nil
// one puts them in the bucket and leaves only temp files and the index on disk.
func Open(cfg Config) (*Store, error) {
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	b, err := newBackend(cfg)
	if err != nil {
		return nil, err
	}
	return openWith(cfg, b, true)
}

// OpenOffline is Open without the startup reconcile. Offline commands such as
// `relay prune` need the raw state they are about to clean and count the
// deletions themselves.
func OpenOffline(cfg Config) (*Store, error) {
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	b, err := newBackend(cfg)
	if err != nil {
		return nil, err
	}
	return openWith(cfg, b, false)
}

func newBackend(cfg Config) (backend, error) {
	if cfg.S3 == nil {
		return newLocalBackend(filepath.Join(cfg.Dir, dataDirName)), nil
	}
	api, err := newS3Client(*cfg.S3)
	if err != nil {
		return nil, err
	}
	return newS3Backend(api, *cfg.S3), nil
}

// open wires a store around an already constructed backend. It is separate from
// Open so tests can inject a fake object store.
func open(cfg Config, b backend) (*Store, error) { return openWith(cfg, b, true) }

func openWith(cfg Config, b backend, reconcile bool) (*Store, error) {
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	tmpDir := filepath.Join(cfg.Dir, tmpDirName)
	for _, dir := range []string{cfg.Dir, tmpDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	// Take the bbolt lock first. Only then is this process the only user of the
	// directory, so finishing a previous download's .tmp leftovers is safe.
	// Cleaning before the lock let a mistyped `relay prune` wipe a running
	// relay's in-flight temp files and *then* fail to open the index.
	ix, err := openIndex(filepath.Join(cfg.Dir, indexFileName))
	if err != nil {
		return nil, err
	}
	cleanTempDir(tmpDir)

	s := &Store{
		tmpDir:     tmpDir,
		maxSize:    cfg.MaxSize,
		backend:    b,
		index:      ix,
		space:      diskspace.New(cfg.Dir, cfg.DiskWaterMark, cfg.diskOptions...),
		publishing: make(map[string]int),
		// The LRU budget is CACHE_MAX_SIZE, not the available capacity: the
		// store may only occupy its share of the disk.
		lru: lru.New[objectMeta](uint64(cfg.MaxSize),
			lru.WithWeigher(func(_ string, m objectMeta) uint64 { return uint64(m.Size) }),
			// Capacity is managed strictly by bytes; runtime memory pressure must
			// never evict a body behind the LRU's back.
			lru.WithPressureFunc(func() float64 { return 0 }),
		),
	}
	// Test-only seam; nil in production and copied here so the store never reads
	// a config struct again.
	s.testAfterPublish = cfg.TestAfterPublish

	records, err := ix.records()
	if err != nil {
		_ = ix.Close()
		return nil, fmt.Errorf("rebuild index: %w", err)
	}
	// Oldest access first, so the final LRU order matches the persisted one.
	for _, m := range records {
		evicted, err := s.lru.Put(m.SHA256, m)
		if err != nil {
			// A record that no longer fits the capacity (it was lowered) is
			// dropped, and its body with it, rather than kept unaccounted.
			s.lru.Delete(m.SHA256)
			s.removeAllLocked(context.Background(), []objectMeta{m})
			continue
		}
		s.removeAllLocked(context.Background(), evicted)
	}

	if reconcile {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if err := s.Reconcile(ctx); err != nil && !errors.Is(err, ErrClosed) {
				log.Printf("hashstore: startup reconcile: %v", err)
			}
		}()
	}
	return s, nil
}

func cleanTempDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// Put stores r under its content digests and returns them. size may be -1 when
// unknown; the body is then read up to CACHE_MAX_SIZE+1 bytes to detect an
// oversized object without pulling all of it. An oversized body is not stored
// and does not evict anything.
func (s *Store) Put(ctx context.Context, r io.Reader, size int64) (Content, error) {
	if !s.enter() {
		return Content{}, ErrClosed
	}
	defer s.leave()

	if size > s.maxSize {
		return Content{}, fmt.Errorf("%w: %d bytes > %d", ErrOversized, size, s.maxSize)
	}
	if err := ctx.Err(); err != nil {
		return Content{}, err
	}
	// Checked before the temp file exists: a low watermark rejects the Put
	// without leaving anything behind. The guard covers the local temp file
	// even when bodies themselves live in a shared bucket.
	if err := s.space.Check(); err != nil {
		return Content{}, err
	}

	tmp, err := os.CreateTemp(s.tmpDir, "put-*")
	if err != nil {
		return Content{}, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	// Read at most one byte past the limit: enough to recognise an oversized
	// body without pulling all of it. The guard keeps MaxSize+1 from wrapping
	// when the budget is MaxInt64.
	limit := s.maxSize
	if limit != math.MaxInt64 {
		limit++
	}
	h1 := sha1.New()
	h256 := sha256.New()
	n, err := io.Copy(io.MultiWriter(waterWriter{w: tmp, space: s.space}, h1, h256), io.LimitReader(r, limit))
	if err != nil {
		_ = tmp.Close()
		return Content{}, err
	}
	if n > s.maxSize {
		_ = tmp.Close()
		return Content{}, fmt.Errorf("%w: body exceeds %d bytes", ErrOversized, s.maxSize)
	}
	if err := tmp.Close(); err != nil {
		return Content{}, err
	}
	if err := ctx.Err(); err != nil {
		return Content{}, err
	}

	c := Content{
		SHA1:   hex.EncodeToString(h1.Sum(nil)),
		SHA256: hex.EncodeToString(h256.Sum(nil)),
		Size:   n,
	}
	// The body becomes visible before its index entry exists: mark it so a
	// concurrent reconcile does not delete it as an orphan.
	s.beginPublish(c.SHA256)
	defer s.endPublish(c.SHA256)

	if err := s.backend.put(ctx, c, tmpPath); err != nil {
		return Content{}, err
	}
	if s.testAfterPublish != nil {
		s.testAfterPublish()
	}
	if err := s.commit(ctx, c); err != nil {
		s.rollbackSharedPublish(ctx, c)
		return Content{}, err
	}
	return c, nil
}

// Adopt publishes a body that was already written in full into the store's temp
// directory, without copying it into a second temp file. size is the caller's
// claim and must be exact; the file is read once to compute both digests, so
// neither the claim nor the path is trusted.
//
// expectAlgorithm may be empty, meaning no expectation, and then expectDigest
// must be empty too. When set (sha1 or sha256), the digest is validated and the
// body's real digest must match it; a mismatch is ErrDigestMismatch and nothing
// is published.
//
// A local backend renames the file into place, so path is gone on success; a
// shared backend uploads it and leaves path for the caller to remove. On failure
// the source is left untouched for the caller and nothing reaches storage, the
// index or the LRU.
func (s *Store) Adopt(ctx context.Context, path string, size int64, expectAlgorithm Algorithm, expectDigest string) (Content, error) {
	if !s.enter() {
		return Content{}, ErrClosed
	}
	defer s.leave()

	algo, wantDigest, err := normalizeAdoptExpectation(expectAlgorithm, expectDigest)
	if err != nil {
		return Content{}, err
	}
	if size < 0 {
		return Content{}, fmt.Errorf("%w: negative size %d", ErrInvalidAdopt, size)
	}
	if size > s.maxSize {
		return Content{}, fmt.Errorf("%w: %d bytes > %d", ErrOversized, size, s.maxSize)
	}
	if err := ctx.Err(); err != nil {
		return Content{}, err
	}
	// The bytes already exist, but the watermark still decides whether the store
	// may take the body in. Checked before reading or publishing so a low
	// watermark leaves no side effect. No temp file is created: the source
	// already is one, and it must not be copied.
	if err := s.space.Check(); err != nil {
		return Content{}, err
	}

	src, err := s.adoptSource(path, size)
	if err != nil {
		return Content{}, err
	}

	if s.testDigestRead != nil {
		s.testDigestRead()
	}
	f, err := os.Open(src)
	if err != nil {
		return Content{}, err
	}
	// Read one byte past the declared size, enough to notice a file that grew
	// after the stat. The guard keeps MaxSize+1 from wrapping.
	limit := size
	if limit != math.MaxInt64 {
		limit++
	}
	h1 := sha1.New()
	h256 := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(h1, h256), io.LimitReader(f, limit))
	closeErr := f.Close()
	if copyErr != nil {
		return Content{}, copyErr
	}
	if closeErr != nil {
		return Content{}, closeErr
	}
	if n != size {
		return Content{}, fmt.Errorf("%w: %s read %d bytes, declared %d", ErrInvalidAdopt, path, n, size)
	}
	if err := ctx.Err(); err != nil {
		return Content{}, err
	}

	c := Content{
		SHA1:   hex.EncodeToString(h1.Sum(nil)),
		SHA256: hex.EncodeToString(h256.Sum(nil)),
		Size:   n,
	}
	if algo != "" {
		got := c.SHA256
		if algo == SHA1 {
			got = c.SHA1
		}
		if got != wantDigest {
			return Content{}, fmt.Errorf("%w: %s is %s, expected %s", ErrDigestMismatch, algo, got, wantDigest)
		}
	}
	return s.publishAdopted(ctx, src, c)
}

// AdoptTrusted publishes a body whose digests the caller already computed,
// without reading the file again. It is the trusted counterpart of Adopt: the
// caller hashed these exact bytes while it wrote them (the active cache's
// PrefixSinks), so the store skips its own verification read.
//
// The trust boundary is explicit. A wrong Content is committed under a false
// address and poisons the local index and a shared bucket; reconcile, LRU and
// prune then treat the lie as a real body. Only call it from a caller that has
// established the real SHA-1 and SHA-256 of exactly the bytes at path; anything
// else must use Adopt.
//
// c.SHA1 and c.SHA256 are validated and normalised (hex alphabet and the
// algorithm's fixed length), and c.Size must be non-negative and within
// CACHE_MAX_SIZE. path must still be a completed regular file inside the store's
// temp directory at exactly c.Size. That checks the path, the file mode and the
// length only: an equal-length rewrite of the same file is not detectable here,
// which is part of what the trusted contract means. expectAlgorithm may be
// empty; when set, the matching digest in c must equal it or nothing is published
// (ErrDigestMismatch) and an existing body is untouched.
//
// Everything after validation is the same publish tail Adopt uses: the publish
// window, the backend put, the index commit, LRU accounting and the shared
// rollback. Source handling is identical too — a local backend renames path into
// place, a shared backend uploads and leaves path for the caller.
func (s *Store) AdoptTrusted(ctx context.Context, path string, c Content, expectAlgorithm Algorithm, expectDigest string) (Content, error) {
	if !s.enter() {
		return Content{}, ErrClosed
	}
	defer s.leave()

	algo, wantDigest, err := normalizeAdoptExpectation(expectAlgorithm, expectDigest)
	if err != nil {
		return Content{}, err
	}
	sha256Hex, err := NormalizeDigest(SHA256, c.SHA256)
	if err != nil {
		return Content{}, err
	}
	sha1Hex, err := NormalizeDigest(SHA1, c.SHA1)
	if err != nil {
		return Content{}, err
	}
	if c.Size < 0 {
		return Content{}, fmt.Errorf("%w: negative size %d", ErrInvalidAdopt, c.Size)
	}
	if c.Size > s.maxSize {
		return Content{}, fmt.Errorf("%w: %d bytes > %d", ErrOversized, c.Size, s.maxSize)
	}
	if err := ctx.Err(); err != nil {
		return Content{}, err
	}
	// Checked before the source is inspected: a low watermark leaves no side
	// effect, exactly like Adopt.
	if err := s.space.Check(); err != nil {
		return Content{}, err
	}

	src, err := s.adoptSource(path, c.Size)
	if err != nil {
		return Content{}, err
	}
	if algo != "" {
		got := sha256Hex
		if algo == SHA1 {
			got = sha1Hex
		}
		if got != wantDigest {
			return Content{}, fmt.Errorf("%w: %s is %s, expected %s", ErrDigestMismatch, algo, got, wantDigest)
		}
	}
	return s.publishAdopted(ctx, src, Content{SHA1: sha1Hex, SHA256: sha256Hex, Size: c.Size})
}

// publishAdopted is the one publish sequence behind Adopt and AdoptTrusted: the
// caller has already resolved the source and the real Content. The body becomes
// visible before its index record exists, so the publish window protects it from
// a concurrent reconcile or eviction, and a failed commit rolls a shared upload
// back.
func (s *Store) publishAdopted(ctx context.Context, src string, c Content) (Content, error) {
	s.beginPublish(c.SHA256)
	defer s.endPublish(c.SHA256)

	if err := s.backend.put(ctx, c, src); err != nil {
		return Content{}, err
	}
	if s.testAfterPublish != nil {
		s.testAfterPublish()
	}
	if err := s.commit(ctx, c); err != nil {
		s.rollbackSharedPublish(ctx, c)
		return Content{}, err
	}
	return c, nil
}

// adoptSource validates that path is a completed regular file inside the store's
// temp directory and that its real size matches the claim.
func (s *Store) adoptSource(path string, size int64) (string, error) {
	tmpAbs, err := filepath.Abs(s.tmpDir)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidAdopt, err)
	}
	rel, err := filepath.Rel(tmpAbs, abs)
	if err != nil || rel == "." || rel == ".." ||
		filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s is outside %s", ErrInvalidAdopt, path, s.tmpDir)
	}
	// Lstat, not Stat: a symlink is not a regular file. Renaming one into the
	// body tree would publish a link instead of the bytes it points at.
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidAdopt, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s is not a regular file", ErrInvalidAdopt, path)
	}
	if info.Size() != size {
		return "", fmt.Errorf("%w: %s holds %d bytes, declared %d", ErrInvalidAdopt, path, info.Size(), size)
	}
	return abs, nil
}

// normalizeAdoptExpectation validates the optional expected digest. An empty
// algorithm means no expectation and requires an empty digest.
func normalizeAdoptExpectation(algo Algorithm, digest string) (Algorithm, string, error) {
	if strings.TrimSpace(string(algo)) == "" {
		if strings.TrimSpace(digest) != "" {
			return "", "", fmt.Errorf("%w: expected digest %q without an algorithm", ErrInvalidDigest, digest)
		}
		return "", "", nil
	}
	parsed, err := ParseAlgorithm(string(algo))
	if err != nil {
		return "", "", err
	}
	normalized, err := NormalizeDigest(parsed, digest)
	if err != nil {
		return "", "", err
	}
	return parsed, normalized, nil
}

// rollbackSharedPublish removes a body that reached a shared bucket but could
// not be indexed. A shared bucket is never LISTed, so such a body would be
// unreachable garbage. Local orphans are reconciled away instead.
//
// The decision and the delete run under s.mu, because another publisher of the
// same digest may have uploaded the same keys and be about to commit them. The
// caller still holds its own publish guard here, so a count above one means a
// peer publisher exists: it either committed (the record check catches that) or
// it is still in flight (the count catches that) and owns the body now. Only the
// last active publisher with no record may delete, and holding the mutex keeps a
// new publisher from uploading between that decision and the delete.
func (s *Store) rollbackSharedPublish(ctx context.Context, c Content) {
	if !s.backend.shared() {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, found, err := s.index.get(c.SHA256); err != nil {
		// Cannot prove the body is unowned; leaving it is safe, and prune will
		// find it later, whereas deleting a peer's indexed body is not.
		log.Printf("hashstore: roll back %s: inspect index: %v", c.SHA256, err)
		return
	} else if found {
		return
	}
	if s.publishing[c.SHA256] > 1 {
		return
	}

	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := s.backend.delete(delCtx, c.SHA256, c.SHA1); err != nil {
		log.Printf("hashstore: roll back %s: %v", c.SHA256, err)
	}
}

// waterWriter rechecks the disk watermark as bytes flow into the temp file.
// The check is throttled by the Checker, so a large body does not statfs once
// per write; a low watermark aborts the copy, and Put's deferred remove drops
// the partial temp file.
type waterWriter struct {
	w     io.Writer
	space *diskspace.Checker
}

func (w waterWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 {
		if spaceErr := w.space.Account(int64(n)); spaceErr != nil {
			return n, spaceErr
		}
	}
	return n, err
}

func (s *Store) beginPublish(sha256Hex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishing[sha256Hex]++
}

func (s *Store) endPublish(sha256Hex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch n := s.publishing[sha256Hex]; {
	case n <= 1:
		delete(s.publishing, sha256Hex)
	default:
		s.publishing[sha256Hex] = n - 1
	}
}

// commit makes a published body visible: the durable record first, then the LRU
// (which decides evictions), then the evicted bodies are removed.
func (s *Store) commit(ctx context.Context, c Content) error {
	meta := objectMeta{SHA256: c.SHA256, SHA1: c.SHA1, Size: c.Size}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.testCommitError != nil {
		if err := s.testCommitError(); err != nil {
			return err
		}
	}
	// Index first: an LRU eviction is unrecoverable for this process, and a body
	// may only be dropped once the record that replaces it is durable. A failure
	// here must leave the LRU untouched, not half-evicted with no pending delete.
	// index.put also clears any queued delete for this digest, so a re-PUT is not
	// deleted by a later reconcile.
	stored, err := s.index.put(meta)
	if err != nil {
		return err
	}
	evicted, err := s.lru.Put(stored.SHA256, stored)
	if err != nil {
		// Unreachable while size <= MaxSize == LRU budget, but never leave the
		// index claiming a body the LRU refused to account for.
		if rmErr := s.index.remove(stored); rmErr != nil {
			log.Printf("hashstore: roll back index %s: %v", stored.SHA256, rmErr)
		}
		return fmt.Errorf("cache accounting: %w", err)
	}
	s.removeAllLocked(ctx, evicted)
	return nil
}

// Get returns a reader for the body addressed by algo/digest. The reader is
// streamed from storage; the object may be deleted by another relay while it is
// being read.
func (s *Store) Get(ctx context.Context, algo Algorithm, digest string) (io.ReadCloser, int64, error) {
	if !s.enter() {
		return nil, 0, ErrClosed
	}
	defer s.leave()

	sha256, sha1, ok, err := s.resolve(ctx, algo, digest)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, ErrNotFound
	}
	rc, size, err := s.backend.open(ctx, sha256)
	if err != nil {
		// Only a definite absence invalidates the index entry. A transient
		// storage error (or a cancelled client) must leave the entry alone:
		// otherwise one bad read silently halves the cache.
		if errors.Is(err, ErrNotFound) && ctx.Err() == nil {
			s.forget(sha256)
		}
		return nil, 0, err
	}
	if err := s.touch(ctx, sha256, sha1, size); err != nil {
		log.Printf("hashstore: promote %s: %v", sha256, err)
	}
	return rc, size, nil
}

// Range returns a reader for the body from offset to its end, plus the body's
// total size. It is the building block for a server's Content-Range and
// multi-range handling: the caller slices or limits the stream itself.
//
// offset == size yields an empty read; a negative offset or one past the end is
// ErrInvalidRange. A hit promotes recency exactly like Get; Stat does not.
func (s *Store) Range(ctx context.Context, algo Algorithm, digest string, offset int64) (io.ReadCloser, int64, error) {
	if !s.enter() {
		return nil, 0, ErrClosed
	}
	defer s.leave()

	if offset < 0 {
		return nil, 0, fmt.Errorf("%w: offset %d", ErrInvalidRange, offset)
	}
	sha256, sha1, ok, err := s.resolve(ctx, algo, digest)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, ErrNotFound
	}
	rc, total, err := s.backend.openRange(ctx, sha256, offset)
	if err != nil {
		if errors.Is(err, ErrNotFound) && ctx.Err() == nil {
			s.forget(sha256)
		}
		return nil, 0, err
	}
	if err := s.touch(ctx, sha256, sha1, total); err != nil {
		log.Printf("hashstore: promote %s: %v", sha256, err)
	}
	return rc, total, nil
}

// Resolve maps either address for a body onto its canonical SHA-256 and known
// SHA-1 alias, without touching recency. It is a lookup, not an existence check:
// a shared backend can know an address whose body another relay has already
// removed, so callers still use Stat or Get before serving.
func (s *Store) Resolve(ctx context.Context, algo Algorithm, digest string) (sha256Hex, sha1Hex string, ok bool, err error) {
	if !s.enter() {
		return "", "", false, ErrClosed
	}
	defer s.leave()

	if err := ctx.Err(); err != nil {
		return "", "", false, err
	}
	return s.resolve(ctx, algo, digest)
}

// Stat reports whether a body exists and its size, without changing recency.
func (s *Store) Stat(ctx context.Context, algo Algorithm, digest string) (int64, bool, error) {
	if !s.enter() {
		return 0, false, ErrClosed
	}
	defer s.leave()

	sha256, _, ok, err := s.resolve(ctx, algo, digest)
	if err != nil {
		return 0, false, err
	}
	if !ok {
		return 0, false, nil
	}
	size, exists, err := s.backend.stat(ctx, sha256)
	if err != nil {
		return 0, false, err
	}
	if !exists {
		if ctx.Err() == nil {
			s.forget(sha256)
		}
		return 0, false, nil
	}
	return size, true, nil
}

// Delete removes a body through either digest. A failed backend delete is
// queued in the index and retried by Reconcile.
func (s *Store) Delete(ctx context.Context, algo Algorithm, digest string) error {
	if !s.enter() {
		return ErrClosed
	}
	defer s.leave()

	sha256, sha1, ok, err := s.resolve(ctx, algo, digest)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if meta, found, err := s.index.get(sha256); err == nil && found {
		sha1 = meta.SHA1
	}
	s.lru.Delete(sha256)
	s.removeAllLocked(ctx, []objectMeta{{SHA256: sha256, SHA1: sha1}})
	return nil
}

// Stats reports current accounting.
func (s *Store) Stats() Stats {
	st := s.lru.Stats()
	return Stats{
		Objects:  st.Len,
		Bytes:    int64(st.CurrentSize),
		Capacity: s.maxSize,
	}
}

// Reconcile retries queued deletions and, for local storage, repairs the index
// against what is on disk: missing bodies are forgotten, orphan bodies (a crash
// between publish and index write) are removed. Shared storage is never listed.
func (s *Store) Reconcile(ctx context.Context) error {
	if !s.enter() {
		return ErrClosed
	}
	defer s.leave()

	s.mu.Lock()
	defer s.mu.Unlock()

	// A cleanup is not the client's transaction: a disconnect must not skip a
	// queued delete.
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	pending, err := s.index.pendingDeletes()
	if err != nil {
		return err
	}
	for _, m := range pending {
		if s.publishing[m.SHA256] > 0 {
			// A PUT of this digest is between publishing its body and writing
			// its index entry; deleting now would remove the fresh body.
			continue
		}
		if err := s.backend.delete(delCtx, m.SHA256, m.SHA1); err != nil {
			log.Printf("hashstore: retry delete %s: %v", m.SHA256, err)
			continue
		}
		if err := s.index.clearPending(m.SHA256); err != nil {
			log.Printf("hashstore: clear pending %s: %v", m.SHA256, err)
		}
	}

	present, err := s.backend.bodies(ctx)
	if err != nil {
		return err
	}
	if present == nil {
		return nil
	}

	records, err := s.index.records()
	if err != nil {
		return err
	}
	for _, m := range records {
		if _, ok := present[m.SHA256]; !ok {
			s.lru.Delete(m.SHA256)
			if err := s.index.remove(m); err != nil {
				log.Printf("hashstore: drop missing %s: %v", m.SHA256, err)
			}
			continue
		}
		delete(present, m.SHA256)
	}
	for sha256 := range present {
		if s.publishing[sha256] > 0 {
			// A PUT is between publishing its body and writing its index entry.
			continue
		}
		// A variant can only name a body this instance knows about; an orphan
		// body has no record, so any metadata naming it is dangling. Drop the
		// metadata before the body, so a failed object delete leaves an
		// unreferenced orphan for the next pass, never a variant pointing at a
		// file that is about to disappear.
		if err := s.index.removeVariantsForDigest(sha256); err != nil {
			log.Printf("hashstore: drop variants of orphan %s: %v", sha256, err)
			continue
		}
		if err := s.backend.delete(delCtx, sha256, ""); err != nil {
			log.Printf("hashstore: remove orphan %s: %v", sha256, err)
		}
	}
	return nil
}

// enter starts a public operation unless the store is closing. leave must be
// called when it returns true.
func (s *Store) enter() bool {
	s.opMu.RLock()
	if s.closed.Load() {
		s.opMu.RUnlock()
		return false
	}
	return true
}

func (s *Store) leave() { s.opMu.RUnlock() }

// Close stops background work and returns only after every in-flight operation
// has finished, then closes the index. It is idempotent and safe to call from
// several goroutines.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		// Set first: enter() then refuses new operations. The write lock makes
		// Close wait for the ones already running, so nothing can publish a body
		// or touch the index after Close returns.
		s.closed.Store(true)
		s.wg.Wait()
		s.opMu.Lock()
		defer s.opMu.Unlock()
		s.closeErr = s.index.Close()
	})
	return s.closeErr
}

// resolve maps an address to its canonical SHA-256 and known SHA-1 alias. A
// local store answers from the index; a shared store also reads the digest
// pointers another instance wrote.
func (s *Store) resolve(ctx context.Context, algo Algorithm, digest string) (sha256Hex, sha1Hex string, ok bool, err error) {
	digest, err = NormalizeDigest(algo, digest)
	if err != nil {
		return "", "", false, err
	}

	s.mu.Lock()
	switch algo {
	case SHA256:
		if meta, found, getErr := s.index.get(digest); getErr != nil {
			s.mu.Unlock()
			return "", "", false, getErr
		} else if found {
			s.mu.Unlock()
			return meta.SHA256, meta.SHA1, true, nil
		}
	case SHA1:
		if primary, found, getErr := s.index.lookupSHA1(digest); getErr != nil {
			s.mu.Unlock()
			return "", "", false, getErr
		} else if found {
			s.mu.Unlock()
			return primary, digest, true, nil
		}
	}
	s.mu.Unlock()

	if !s.backend.shared() {
		return "", "", false, nil
	}

	if other, found := s.backend.alias(ctx, algo, digest); found {
		if algo == SHA1 {
			return other, digest, true, nil
		}
		return digest, other, true, nil
	}
	if algo == SHA256 {
		// The body is keyed by SHA-256; the pointer only supplies the SHA-1
		// alias, so a missing pointer does not rule out the body.
		return digest, "", true, nil
	}
	return "", "", false, nil
}

// touch promotes an indexed body, or adopts a body that exists in shared storage
// but not in this instance's index.
func (s *Store) touch(ctx context.Context, sha256Hex, sha1Hex string, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	meta, found, err := s.index.touch(sha256Hex)
	if err != nil {
		return err
	}
	if found {
		if _, ok := s.lru.Get(sha256Hex); !ok {
			evicted, err := s.lru.Put(meta.SHA256, meta)
			if err != nil {
				return err
			}
			s.removeAllLocked(ctx, evicted)
		}
		return nil
	}

	if sha1Hex == "" {
		// Without the SHA-1 alias this instance cannot account for the body, so
		// it is served but not adopted.
		return nil
	}
	meta = objectMeta{SHA256: sha256Hex, SHA1: sha1Hex, Size: size}
	evicted, err := s.lru.Put(meta.SHA256, meta)
	if err != nil {
		return err
	}
	if _, err := s.index.put(meta); err != nil {
		s.lru.Delete(meta.SHA256)
		return err
	}
	s.removeAllLocked(ctx, evicted)
	return nil
}

// forget drops an index entry whose body is gone.
func (s *Store) forget(sha256Hex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetLocked(sha256Hex)
}

func (s *Store) forgetLocked(sha256Hex string) {
	s.lru.Delete(sha256Hex)
	meta, found, err := s.index.get(sha256Hex)
	if err != nil || !found {
		return
	}
	if err := s.index.remove(meta); err != nil {
		log.Printf("hashstore: drop index entry %s: %v", sha256Hex, err)
	}
}

// removeAllLocked deletes evicted bodies. The pending record is written in the
// same transaction that removes the index entry, so a failed object delete can
// never be lost; a successful delete clears it.
func (s *Store) removeAllLocked(ctx context.Context, metas []objectMeta) {
	if len(metas) == 0 {
		return
	}
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	for _, m := range metas {
		if s.publishing[m.SHA256] > 0 {
			// A publisher of this digest already has its bytes in place (a
			// backend.put that renamed them, or an upload) but has not committed
			// its record yet. Deleting now would strip the body its commit is
			// about to name, leaving an index and variants pointing at nothing,
			// and Reconcile could not repair that. Skip the whole removal: the
			// durable record and body stay, and the publisher's own commit
			// re-inserts the LRU entry this eviction dropped. Until then the
			// index still describes a readable body; a Get promotes it again, so
			// the only cost is a temporarily under-counted LRU budget.
			continue
		}
		if err := s.index.removeAndQueue(m); err != nil {
			log.Printf("hashstore: dequeue %s: %v", m.SHA256, err)
			continue
		}
		if err := s.backend.delete(delCtx, m.SHA256, m.SHA1); err != nil {
			log.Printf("hashstore: delete %s failed, queued for retry: %v", m.SHA256, err)
			continue
		}
		if err := s.index.clearPending(m.SHA256); err != nil {
			log.Printf("hashstore: clear pending %s: %v", m.SHA256, err)
		}
	}
}
