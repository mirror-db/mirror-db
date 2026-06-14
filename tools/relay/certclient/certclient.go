package certclient

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/mirror-db/mirror-db/tools/relay/guard"
)

// CertClient implements certmagic.Storage by fetching cert-pack tar from
// an upstream relay instance.
type CertClient struct {
	CertPackURL string
	SecretKey   []byte

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
	return time.Since(time.UnixMilli(ts)) < time.Hour
}

// Refresh fetches cert-pack if cache expired (1 hour).
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

	req, err := http.NewRequest("GET", c.CertPackURL, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenStr)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch cert-pack: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cert-pack returned %s", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read cert-pack: %w", err)
	}

	files, err := untar(data)
	if err != nil {
		return fmt.Errorf("untar cert-pack: %w", err)
	}

	c.files.Store(&files)
	c.updateAt.Store(time.Now().UnixMilli())
	log.Printf("cert-pack refreshed: %d entries", len(files))
	return nil
}

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

		// Strip "certmagic/" prefix to get storage key
		key := hdr.Name
		if rest, ok := cutPrefix(key, "certmagic/"); ok {
			key = rest
		}
		key = filepath.Clean(key)

		if hdr.Typeflag == tar.TypeDir {
			files[key] = fileEntry{isDir: true, modTime: hdr.ModTime}
		} else {
			content, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			files[key] = fileEntry{content: content, modTime: hdr.ModTime}
		}
	}
	return files, nil
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return s, false
}

func (c *CertClient) getFiles() map[string]fileEntry {
	p := c.files.Load()
	if p == nil {
		return nil
	}
	return *p
}

// certmagic.Storage implementation

func (c *CertClient) Store(_ context.Context, _ string, _ []byte) error { return nil }
func (c *CertClient) Delete(_ context.Context, _ string) error          { return nil }
func (c *CertClient) Lock(_ context.Context, _ string) error            { return nil }
func (c *CertClient) Unlock(_ context.Context, _ string) error          { return nil }

func (c *CertClient) Exists(_ context.Context, key string) bool {
	c.Refresh()
	files := c.getFiles()
	if files == nil {
		return false
	}
	_, ok := files[key]
	return ok
}

func (c *CertClient) Load(_ context.Context, key string) ([]byte, error) {
	c.Refresh()
	files := c.getFiles()
	if files == nil {
		return nil, fs.ErrNotExist
	}
	f, ok := files[key]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return f.content, nil
}

func (c *CertClient) Stat(_ context.Context, key string) (certmagic.KeyInfo, error) {
	c.Refresh()
	files := c.getFiles()
	if files == nil {
		return certmagic.KeyInfo{}, fs.ErrNotExist
	}
	f, ok := files[key]
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

func (c *CertClient) List(_ context.Context, prefix string, recursive bool) ([]string, error) {
	c.Refresh()
	files := c.getFiles()
	if files == nil {
		return nil, nil
	}
	var keys []string
	for k := range files {
		if k == prefix || hasPathPrefix(k, prefix) {
			rel := k
			if prefix != "" {
				rel = k[len(prefix):]
				if len(rel) > 0 && rel[0] == '/' {
					rel = rel[1:]
				}
			}
			if !recursive && containsSlash(rel) {
				continue
			}
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func hasPathPrefix(path, prefix string) bool {
	if prefix == "" || prefix == "." {
		return true
	}
	return len(path) > len(prefix) && path[:len(prefix)] == prefix && path[len(prefix)] == '/'
}

func containsSlash(s string) bool {
	for _, c := range s {
		if c == '/' {
			return true
		}
	}
	return false
}
