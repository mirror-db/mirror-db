package certclient

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mirror-db/mirror-db/tools/relay/guard"
)

func tarWith(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     0644,
			Size:     int64(len(content)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUntarStripsPrefix(t *testing.T) {
	pack := tarWith(t, map[string]string{
		"certmagic/acme/acme_account+key":        "acct",
		"certmagic/certificates/a.com/a.com.crt": "cert",
		"loose.txt":                              "not in certmagic",
	})

	files, err := untar(pack)
	if err != nil {
		t.Fatal(err)
	}

	if string(files["acme/acme_account+key"].content) != "acct" {
		t.Errorf("prefix not stripped: %v", files)
	}
	if string(files["certificates/a.com/a.com.crt"].content) != "cert" {
		t.Errorf("nested key missing")
	}
	if _, ok := files["loose.txt"]; !ok {
		t.Errorf("non-prefixed entry should be kept")
	}
}

func TestUntarRejectsTraversal(t *testing.T) {
	pack := tarWith(t, map[string]string{"certmagic/../../etc/passwd": "x"})
	if _, err := untar(pack); err == nil {
		t.Fatal("expected traversal entry to be rejected")
	}
}

func TestUntarEmpty(t *testing.T) {
	files, err := untar(tarWith(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("expected empty map, got %d", len(files))
	}
}

// newClient points a CertClient at a fake parent that serves pack.
func newClient(t *testing.T, pack []byte, secret string) (*CertClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The parent verifies with the same guard the client signs for.
		if err := (&guard.Guard{SecretKey: []byte(secret)}).Check(r); err != nil {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		w.Write(pack)
	}))
	t.Cleanup(srv.Close)

	c := &CertClient{
		CertPackURL: srv.URL,
		SecretKey:   []byte(secret),
		Transport:   srv.Client().Transport.(*http.Transport).Clone(),
	}
	c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test server cert
	return c, srv
}

func TestCertClientRoundTrip(t *testing.T) {
	pack := tarWith(t, map[string]string{
		"certmagic/certificates/a.com/a.com.crt": "-----BEGIN CERTIFICATE-----",
		"certmagic/certificates/a.com/a.com.key": "-----BEGIN PRIVATE KEY-----",
	})
	c, _ := newClient(t, pack, "s3cret")

	if err := c.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	ctx := context.Background()
	if !c.Exists(ctx, "certificates/a.com/a.com.crt") {
		t.Fatal("Exists returned false")
	}

	data, err := c.Load(ctx, "certificates/a.com/a.com.crt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "-----BEGIN CERTIFICATE-----" {
		t.Fatalf("Load = %q", data)
	}

	// Loaded bytes must not alias the cached entry.
	data[0] = 'X'
	again, _ := c.Load(ctx, "certificates/a.com/a.com.crt")
	if again[0] == 'X' {
		t.Fatal("Load returned a shared slice; caller mutation leaked into cache")
	}

	if _, err := c.Load(ctx, "certificates/missing"); err != fs.ErrNotExist {
		t.Fatalf("missing key error = %v, want fs.ErrNotExist", err)
	}

	keys, err := c.List(ctx, "certificates/a.com", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("List returned %d keys, want 2: %v", len(keys), keys)
	}

	stat, err := c.Stat(ctx, "certificates/a.com/a.com.key")
	if err != nil {
		t.Fatal(err)
	}
	if !stat.IsTerminal || stat.Size == 0 {
		t.Fatalf("odd stat: %+v", stat)
	}
}

func TestCertClientWriteRejected(t *testing.T) {
	c, _ := newClient(t, tarWith(t, map[string]string{"certmagic/a": "b"}), "k")
	ctx := context.Background()

	if err := c.Store(ctx, "x", []byte("y")); err != ErrReadOnly {
		t.Errorf("Store = %v, want ErrReadOnly", err)
	}
	if err := c.Delete(ctx, "x"); err != ErrReadOnly {
		t.Errorf("Delete = %v, want ErrReadOnly", err)
	}
	if err := c.Lock(ctx, "x"); err != nil {
		t.Errorf("Lock = %v, want nil", err)
	}
}

func TestCertClientRejectsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	c := &CertClient{CertPackURL: srv.URL, SecretKey: []byte("k"), Transport: http.DefaultTransport}
	if err := c.Refresh(); err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}

func TestCertClientKeepsOldSnapshotOnFailure(t *testing.T) {
	pack := tarWith(t, map[string]string{"certmagic/a": "b"})
	c, srv := newClient(t, pack, "k")
	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}

	srv.Close()
	// Force a staleness check that fails.
	c.updateAt.Store(0)
	if err := c.Refresh(); err == nil {
		t.Fatal("expected refresh failure after server close")
	}
	if !c.Exists(context.Background(), "a") {
		t.Fatal("failed refresh must keep the previous snapshot")
	}
}

var _ = io.Discard

// A non-recursive listing reports the immediate children — directories as their
// own keys — not every file buried below the prefix. certmagic's own storage
// implementations behave this way, and a consumer that walks the pack one level
// at a time needs it to.
func TestCertClientListNonRecursive(t *testing.T) {
	pack := tarWith(t, map[string]string{
		"certificates/issuer-a/anitya.net/anitya.net.crt": "a",
		"certificates/issuer-a/anitya.net/anitya.net.key": "b",
		"certificates/issuer-b/netx.ink/netx.ink.crt":     "c",
		"acme/issuer-a/users/me/acme.key":                 "d",
	})
	c, _ := newClient(t, pack, "s3cret")
	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	got, err := c.List(ctx, "certificates", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"certificates/issuer-a", "certificates/issuer-b"}
	if len(got) != len(want) {
		t.Fatalf("List(certificates, false) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List(certificates, false) = %v, want %v", got, want)
		}
	}

	// One level deeper reports the certificate directories, which is how the
	// pack is walked to find <name>.crt / <name>.key.
	got, _ = c.List(ctx, "certificates/issuer-a", false)
	if len(got) != 1 || got[0] != "certificates/issuer-a/anitya.net" {
		t.Fatalf("List(certificates/issuer-a, false) = %v", got)
	}

	// Recursive still returns the files themselves.
	got, _ = c.List(ctx, "certificates", true)
	if len(got) != 3 {
		t.Fatalf("List(certificates, true) = %v, want 3 files", got)
	}
}
