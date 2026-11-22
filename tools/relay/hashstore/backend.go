package hashstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// objectKind names one of the objects a content body may have in a backend.
type objectKind int

const (
	bodyObject objectKind = iota
	sha1AliasObject
	sha256AliasObject
	// foreignObject is anything else found while listing a shared prefix. It is
	// reported so the caller can count it, never deleted.
	foreignObject
)

// objectRef identifies one stored object.
type objectRef struct {
	Kind   objectKind
	Digest string
}

func (r objectRef) describe() string {
	switch r.Kind {
	case bodyObject:
		return "body " + r.Digest
	case sha1AliasObject:
		return "sha1 alias " + r.Digest
	case sha256AliasObject:
		return "sha256 alias " + r.Digest
	case foreignObject:
		return "unknown object"
	}
	return "unknown object"
}

// backend is the only seam between the two storage kinds: the store owns
// hashing, capacity accounting, LRU and the index; a backend only publishes,
// opens and deletes whole bodies.
type backend interface {
	// put atomically publishes the body currently held in tmpPath. tmpPath is on
	// the store's local filesystem, on the same filesystem as local bodies.
	put(ctx context.Context, c Content, tmpPath string) error
	// open returns a reader for a body. A missing or unreadable body is
	// ErrNotFound.
	open(ctx context.Context, sha256 string) (io.ReadCloser, int64, error)
	// stat reports whether a body exists and how large it is. A definite absence
	// is (0, false, nil); a transient, permission or I/O error is returned and
	// must not be mistaken for a miss.
	stat(ctx context.Context, sha256 string) (int64, bool, error)
	// delete removes a body and, for shared storage, its digest pointers.
	delete(ctx context.Context, sha256, sha1 string) error
	// alias resolves a digest pointer written by another instance. Local
	// storage keeps aliases in the index instead, so it always returns false.
	alias(ctx context.Context, algo Algorithm, digest string) (string, bool)
	// bodies returns the SHA-256 of every published body, for orphan
	// reconciliation. Shared storage never lists the bucket, so it returns nil.
	// Normal reads and the startup reconcile never call listObjects.
	bodies(ctx context.Context) (map[string]struct{}, error)
	// listObjects enumerates every object prune may consider. Shared storage
	// pages through the configured bucket prefix; local storage walks its data
	// directory. This is the only full listing in the store.
	listObjects(ctx context.Context, fn func(objectRef) error) error
	// deleteObject removes exactly one object (a body or a single alias).
	deleteObject(ctx context.Context, ref objectRef) error
	shared() bool
}

// localBackend keeps bodies under <CACHE_DIR>/data, sharded by the first two
// bytes of the SHA-256.
type localBackend struct {
	root string
}

func newLocalBackend(root string) *localBackend { return &localBackend{root: root} }

func (b *localBackend) relPath(sha256 string) string {
	if len(sha256) < 4 {
		return ""
	}
	return filepath.ToSlash(filepath.Join(sha256[0:2], sha256[2:4], sha256))
}

func (b *localBackend) path(sha256 string) (string, error) {
	rel := b.relPath(sha256)
	if rel == "" {
		return "", ErrInvalidDigest
	}
	return filepath.Join(b.root, filepath.FromSlash(rel)), nil
}

func (b *localBackend) put(_ context.Context, c Content, tmpPath string) error {
	dst, err := b.path(c.SHA256)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	// The rename is atomic, but only after the temp file's bytes are durable.
	// Both files are under CACHE_DIR, so no cross-filesystem fallback is needed.
	f, err := os.OpenFile(tmpPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	syncDir(filepath.Dir(dst))
	return nil
}

func (b *localBackend) open(_ context.Context, sha256 string) (io.ReadCloser, int64, error) {
	p, err := b.path(sha256)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		// Only a definite absence is a cache miss. EACCES/EIO/... must surface
		// so the caller keeps its index entry and can answer 502.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	if info.IsDir() {
		_ = f.Close()
		return nil, 0, fmt.Errorf("%w: %s is a directory", ErrUnreadable, p)
	}
	return f, info.Size(), nil
}

func (b *localBackend) stat(_ context.Context, sha256 string) (int64, bool, error) {
	p, err := b.path(sha256)
	if err != nil {
		return 0, false, err
	}
	info, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if info.IsDir() {
		return 0, false, fmt.Errorf("%w: %s is a directory", ErrUnreadable, p)
	}
	return info.Size(), true, nil
}

func (b *localBackend) delete(_ context.Context, sha256, _ string) error {
	p, err := b.path(sha256)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (b *localBackend) alias(context.Context, Algorithm, string) (string, bool) {
	return "", false
}

func (b *localBackend) bodies(_ context.Context) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	err := filepath.WalkDir(b.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		// The data directory holds sharded digest-named files and nothing else.
		// A file that is not named after a digest is not ours to delete.
		base := filepath.Base(p)
		if isDigestName(base) {
			out[base] = struct{}{}
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	return out, err
}

func isDigestName(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (b *localBackend) listObjects(ctx context.Context, fn func(objectRef) error) error {
	present, err := b.bodies(ctx)
	if err != nil {
		return err
	}
	for digest := range present {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(objectRef{Kind: bodyObject, Digest: digest}); err != nil {
			return err
		}
	}
	return nil
}

func (b *localBackend) deleteObject(_ context.Context, ref objectRef) error {
	if ref.Kind != bodyObject {
		// Local storage keeps aliases in the index, not as files.
		return nil
	}
	p, err := b.path(ref.Digest)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (b *localBackend) shared() bool { return false }

// syncDir makes a rename durable. Failure is not fatal for a cache: the body is
// either there after a crash or reconciled away.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
