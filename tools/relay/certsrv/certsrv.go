package certsrv

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
)

// CertService serves certmagic storage as a tar archive.
// Protected by JWT guard — only other relay instances can fetch.
type CertService struct {
	storage certmagic.Storage

	mu        sync.Mutex
	certPack  []byte
	createdAt time.Time
}

func New(storage certmagic.Storage) *CertService {
	return &CertService{storage: storage}
}

func (c *CertService) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	pack, err := c.getCertPack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Write(pack)
}

func (c *CertService) getCertPack() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if time.Since(c.createdAt) <= time.Hour && c.certPack != nil {
		return c.certPack, nil
	}

	pack, err := c.buildPack()
	if err != nil {
		return nil, err
	}

	c.certPack = pack
	c.createdAt = time.Now()
	return pack, nil
}

func (c *CertService) buildPack() ([]byte, error) {
	ctx := context.Background()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	files, err := c.storage.List(ctx, "", true)
	if err != nil {
		return nil, fmt.Errorf("list storage: %w", err)
	}

	for _, file := range files {
		if strings.HasPrefix(file, "locks/") {
			continue
		}

		stat, err := c.storage.Stat(ctx, file)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", file, err)
		}

		if stat.IsTerminal {
			content, err := c.storage.Load(ctx, file)
			if err != nil {
				return nil, fmt.Errorf("load %s: %w", file, err)
			}
			tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg,
				Name:     filepath.Join("certmagic", file),
				ModTime:  stat.Modified,
				Mode:     0644,
				Size:     int64(len(content)),
			})
			tw.Write(content)
		} else {
			tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     filepath.Join("certmagic", file) + "/",
				ModTime:  stat.Modified,
				Mode:     0755,
			})
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}
	return buf.Bytes(), nil
}
