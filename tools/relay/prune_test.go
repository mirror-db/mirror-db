package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
	"github.com/spf13/viper"
)

// isolateCacheConfig keeps the command tests away from whatever the host
// config file or environment says about the cache.
func isolateCacheConfig(t *testing.T) {
	t.Helper()
	keys := []string{
		"cache.dir", "cache.capacity", "cache.max_size",
		"cache.s3.bucket", "cache.s3.endpoint", "cache.s3.prefix", "cache.s3.region",
	}
	for _, key := range keys {
		viper.Set(key, "")
	}
	t.Cleanup(func() {
		for _, key := range keys {
			viper.Set(key, "")
		}
	})
}

func runPruneCommand(t *testing.T) (string, error) {
	t.Helper()
	cmd := pruneCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	return out.String(), err
}

func TestPruneCommandRequiresEnabledCache(t *testing.T) {
	isolateCacheConfig(t)

	out, err := runPruneCommand(t)
	if err == nil {
		t.Fatalf("expected an error, got output %q", out)
	}
	if !strings.Contains(err.Error(), "cache is not enabled") {
		t.Fatalf("error = %v, want a clear disabled-cache message", err)
	}
}

func TestPruneCommandRemovesOrphan(t *testing.T) {
	isolateCacheConfig(t)
	dir := t.TempDir()
	viper.Set("cache.dir", dir)
	viper.Set("cache.capacity", "1MiB")

	// A body this relay indexes.
	seed, err := hashstore.Open(hashstore.Config{Dir: dir, Capacity: 1 << 20})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := seed.Put(context.Background(), strings.NewReader("kept"), 4); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// An orphan written while no store was running.
	orphan := sha256.Sum256([]byte("orphan"))
	orphanDigest := hex.EncodeToString(orphan[:])
	orphanPath := filepath.Join(dir, "data", orphanDigest[0:2], orphanDigest[2:4], orphanDigest)
	if err := os.MkdirAll(filepath.Dir(orphanPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runPruneCommand(t)
	if err != nil {
		t.Fatalf("prune: %v (output %q)", err, out)
	}
	const want = "prune: scanned=2 kept=1 deleted=1 pending=0 stale=0 skipped=0 failed=0\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	if _, err := os.Stat(orphanPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("orphan survived: %v", err)
	}

	// The indexed body is untouched.
	reopened, err := hashstore.Open(hashstore.Config{Dir: dir, Capacity: 1 << 20})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	body, _, err := reopened.Get(context.Background(), hashstore.SHA256, hashstoreContentDigest(t, "kept"))
	if err != nil {
		t.Fatalf("get kept body: %v", err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "kept" {
		t.Fatalf("kept body = %q, %v", data, err)
	}
}

func hashstoreContentDigest(t *testing.T, body string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
