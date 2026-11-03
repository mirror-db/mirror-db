package certsrv

import (
	"archive/tar"
	"context"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
)

// CertService serves certmagic storage as a tar archive.
// Protected by JWT guard — only other relay instances can fetch.
//
// The pack is streamed: entries are listed and loaded while the response body
// is being written, so a 200 MB store never sits in memory as one buffer.
type CertService struct {
	storage certmagic.Storage
}

func New(storage certmagic.Storage) *CertService {
	return &CertService{storage: storage}
}

func (c *CertService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	// List first so a storage failure still yields an honest 500. Entries are
	// then streamed straight into the response — no full-pack buffer.
	entries, err := c.storage.List(ctx, "", true)
	if err != nil {
		http.Error(w, "list storage failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="cert-pack.tar"`)
	// Unknown length: the response is chunked and clients read to EOF.
	if err := c.writePack(ctx, w, entries); err != nil {
		// Headers are gone. A truncated tar makes the client reject the pack
		// and keep using its previous snapshot, which is the safe outcome.
		log.Printf("cert-pack stream aborted after headers: %v", err)
	}
}

func (c *CertService) writePack(ctx context.Context, w io.Writer, entries []string) error {
	tw := tar.NewWriter(w)

	for _, key := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Lock files are per-process and must never leave the machine.
		if isLockFile(key) {
			continue
		}

		stat, err := c.storage.Stat(ctx, key)
		if err != nil {
			// A key can vanish between List and Stat; skip rather than abort
			// the whole pack — one stale entry shouldn't block every peer.
			continue
		}

		name := path.Join("certmagic", key)

		if !stat.IsTerminal {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     name + "/",
				ModTime:  stat.Modified,
				Mode:     0755,
			}); err != nil {
				return err
			}
			continue
		}

		content, err := c.storage.Load(ctx, key)
		if err != nil {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			ModTime:  stat.Modified,
			Mode:     0644,
			Size:     int64(len(content)),
		}); err != nil {
			return err
		}
		if _, err := tw.Write(content); err != nil {
			return err
		}
	}

	return tw.Close()
}

func isLockFile(key string) bool {
	for _, seg := range strings.Split(key, "/") {
		if seg == "locks" || strings.HasSuffix(seg, ".lock") {
			return true
		}
	}
	return false
}

// IsLockFile reports whether a storage key is per-process bookkeeping that must
// never leave the machine.
func IsLockFile(key string) bool { return isLockFile(key) }
