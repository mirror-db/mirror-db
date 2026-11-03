package certclient

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/mirror-db/mirror-db/tools/relay/guard"
)

// refreshInterval is how long a fetched cert-pack is considered current.
const refreshInterval = time.Hour

// maxPackBytes bounds the cert-pack download; a full certmagic store for a
// handful of wildcard domains is a few hundred KB.
const maxPackBytes = 64 << 20

// CertClient implements certmagic.Storage by fetching a cert-pack tar from an
// upstream relay instance.
//
// It is a READ-ONLY consumer: the parent owns issuance and renewal. Mutating
// operations return certmagic.ErrNotImplemented so certmagic never believes it
// renewed a certificate that the pack will not carry.
type CertClient struct {
	CertPackURL string
	SecretKey   []byte
	Transport   http.RoundTripper // nil → http.DefaultTransport

	mu       sync.Mutex
	files    atomic.Pointer[map[string]fileEntry]
	updateAt atomic.Int64 // UnixMilli of last successful fetch
}

type fileEntry struct {
	content []byte
	modTime time.Time
	isDir   bool
}

var _ certmagic.Storage = (*CertClient)(nil)

func (c *CertClient) isFresh() bool {
	ts := c.updateAt.Load()
	if ts == 0 {
		return false
	}
	return time.Since(time.UnixMilli(ts)) < refreshInterval
}

// Refresh fetches the cert-pack if the cached snapshot is older than one hour.
func (c *CertClient) Refresh() error {
	if c.isFresh() {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.isFresh() {
		return nil
	}

	return c.fetch()
}

func (c *CertClient) fetch() error {
	tokenStr, err := guard.SignToken(c.SecretKey)
	if err != nil {
		return fmt.Errorf("sign token: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.CertPackURL, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenStr)

	client := &http.Client{Transport: c.Transport, Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch cert-pack: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Do not echo the body: it may contain upstream internals.
		return fmt.Errorf("cert-pack returned %s", resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPackBytes))
	if err != nil {
		return fmt.Errorf("read cert-pack: %w", err)
	}

	files, err := untar(data)
	if err != nil {
		return fmt.Errorf("untar cert-pack: %w", err)
	}
	if _, ok := files["certificates"]; !ok && len(files) == 0 {
		return fmt.Errorf("cert-pack is empty")
	}

	c.files.Store(&files)
	c.updateAt.Store(time.Now().UnixMilli())
	return nil
}

// untar parses a cert-pack tar into storage-keyed entries, stripping the
// leading "certmagic/" directory the server adds.
func untar(data []byte) (map[string]fileEntry, error) {
	files := make(map[string]fileEntry)
	tr := tar.NewReader(bytes.NewReader(data))

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		key := path.Clean(strings.TrimPrefix(hdr.Name, "certmagic/"))
		if key == "." || key == "" {
			continue
		}
		// Reject absolute or parent-escaping entries outright.
		if path.IsAbs(key) || strings.HasPrefix(key, "../") {
			return nil, fmt.Errorf("unsafe path in cert-pack: %q", hdr.Name)
		}

		if hdr.Typeflag == tar.TypeDir {
			files[key] = fileEntry{isDir: true, modTime: hdr.ModTime}
			continue
		}

		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[key] = fileEntry{content: content, modTime: hdr.ModTime}
	}
	return files, nil
}

func (c *CertClient) getFiles() map[string]fileEntry {
	if p := c.files.Load(); p != nil {
		return *p
	}
	return nil
}

// --- certmagic.Storage ---

// ErrReadOnly is returned by mutating operations. certmagic surfaces it to the
// caller instead of pretending the write succeeded.
var ErrReadOnly = errors.New("certclient: storage is read-only")

// Mutating operations are not supported: this relay consumes the parent's
// certificates, it never issues its own.
func (c *CertClient) Store(context.Context, string, []byte) error {
	return ErrReadOnly
}

func (c *CertClient) Delete(context.Context, string) error {
	return ErrReadOnly
}

// Lock / Unlock are no-ops that succeed: nothing here is shared state, so
// refusing would only stall certmagic's read-side bookkeeping.

func (c *CertClient) Lock(context.Context, string) error   { return nil }
func (c *CertClient) Unlock(context.Context, string) error { return nil }

func (c *CertClient) Exists(_ context.Context, key string) bool {
	_ = c.Refresh()
	_, ok := c.getFiles()[key]
	return ok
}

func (c *CertClient) Load(_ context.Context, key string) ([]byte, error) {
	_ = c.Refresh()
	f, ok := c.getFiles()[key]
	if !ok {
		return nil, fs.ErrNotExist
	}
	// Return a copy: certmagic may hold the slice while we replace the map.
	return bytes.Clone(f.content), nil
}

func (c *CertClient) Stat(_ context.Context, key string) (certmagic.KeyInfo, error) {
	_ = c.Refresh()
	f, ok := c.getFiles()[key]
	if !ok {
		return certmagic.KeyInfo{}, fs.ErrNotExist
	}
	return certmagic.KeyInfo{
		Key:        key,
		Modified:   f.modTime,
		Size:       int64(len(f.content)),
		IsTerminal: !f.isDir,
	}, nil
}

// List returns keys under prefix. Like certmagic.FileStorage, keys are returned
// relative to prefix when the prefix names a directory.
func (c *CertClient) List(_ context.Context, prefix string, recursive bool) ([]string, error) {
	_ = c.Refresh()
	files := c.getFiles()
	if files == nil {
		return nil, nil
	}

	prefix = strings.TrimSuffix(prefix, "/")
	keys := make([]string, 0, len(files))
	for k := range files {
		if prefix != "" && k != prefix && !strings.HasPrefix(k, prefix+"/") {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(k, prefix), "/")
		if rel == "" {
			continue
		}
		if !recursive && strings.Contains(rel, "/") {
			continue
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// String is handy in logs and satisfies fmt.Stringer.
func (c *CertClient) String() string {
	n := 0
	if f := c.getFiles(); f != nil {
		n = len(f)
	}
	return "certclient(" + c.CertPackURL + ", " + strconv.Itoa(n) + " entries)"
}
