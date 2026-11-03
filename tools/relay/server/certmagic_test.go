package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
)

// writePack lays out a certificate the way certmagic's FileStorage does:
// certificates/<issuer>/<name>/<name>.{crt,key}.
func writePack(t *testing.T, dir, issuer, name string, certPEM, keyPEM []byte) {
	t.Helper()
	d := filepath.Join(dir, "certificates", issuer, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, name+".crt"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, name+".key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

// selfSigned returns a certificate for name, with a serial that makes two calls
// produce different certificates (so a "rotation" test can tell them apart).
func selfSigned(t *testing.T, name string, serial int64) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// newPackCacheFixture builds the pair certmagic needs — a cache and a config
// bound to it — over a directory-backed storage.
func newPackCacheFixture(t *testing.T, dir string) (*packCache, *certmagic.Config, *certmagic.Cache) {
	t.Helper()

	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return nil, fmt.Errorf("a cert-pack consumer renews nothing")
		},
	})
	t.Cleanup(cache.Stop)

	storage := &certmagic.FileStorage{Path: dir}
	cm := certmagic.New(cache, certmagic.Config{Storage: storage})

	return newPackCache(cache, cm, storage), cm, cache
}

// The whole point of the loader: certmagic does not consult storage for a name
// it is not managing, so a pack that is present but never cached serves nothing.
// Before this existed, every handshake failed with "no certificate available".
func TestPackCacheServesWhatThePackHolds(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := selfSigned(t, "anitya.net", 1)
	writePack(t, dir, "acme-dir", "anitya.net", certPEM, keyPEM)

	pc, cm, cache := newPackCacheFixture(t, dir)

	// Nothing is in the cache until the loader runs.
	if n := len(cache.AllMatchingCertificates("anitya.net")); n != 0 {
		t.Fatalf("cache started with %d certificates", n)
	}

	loaded, err := pc.reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded != 1 {
		t.Fatalf("loaded %d certificates, want 1", loaded)
	}

	got, err := cm.GetCertificate(&tls.ClientHelloInfo{ServerName: "anitya.net"})
	if err != nil {
		t.Fatalf("GetCertificate after load: %v", err)
	}
	if got == nil || got.Leaf == nil || got.Leaf.Subject.CommonName != "anitya.net" {
		t.Fatalf("wrong certificate served: %+v", got)
	}
}

// A renewed certificate replaces its predecessor instead of sitting beside it.
func TestPackCacheReplacesRotatedCertificate(t *testing.T) {
	dir := t.TempDir()
	oldCert, oldKey := selfSigned(t, "anitya.net", 1)
	writePack(t, dir, "acme-dir", "anitya.net", oldCert, oldKey)

	pc, cm, cache := newPackCacheFixture(t, dir)
	if _, err := pc.reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	newCert, newKey := selfSigned(t, "anitya.net", 2)
	writePack(t, dir, "acme-dir", "anitya.net", newCert, newKey)
	if _, err := pc.reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	if n := len(cache.AllMatchingCertificates("anitya.net")); n != 1 {
		t.Fatalf("cache holds %d certificates for one name after rotation", n)
	}
	got, err := cm.GetCertificate(&tls.ClientHelloInfo{ServerName: "anitya.net"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Leaf.SerialNumber.Int64() != 2 {
		t.Fatalf("serving serial %v, want the renewed one", got.Leaf.SerialNumber)
	}
}

// A pack with no certificates is not an error — the parent may still be issuing
// — but it must load zero rather than panic.
func TestPackCacheToleratesEmptyPack(t *testing.T) {
	pc, _, _ := newPackCacheFixture(t, t.TempDir())

	loaded, err := pc.reload(context.Background())
	if err != nil {
		t.Fatalf("reload on an empty pack: %v", err)
	}
	if loaded != 0 {
		t.Fatalf("loaded %d certificates from an empty pack", loaded)
	}
}

// A certificate without its key must be skipped, not fatal: one broken entry
// cannot be allowed to take the whole pack down.
func TestPackCacheSkipsCertWithoutKey(t *testing.T) {
	dir := t.TempDir()
	certPEM, _ := selfSigned(t, "lonely.example", 1)
	d := filepath.Join(dir, "certificates", "acme-dir", "lonely.example")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "lonely.example.crt"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	pc, _, _ := newPackCacheFixture(t, dir)
	loaded, err := pc.reload(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded != 0 {
		t.Fatalf("loaded %d certificates; an unpaired .crt must be skipped", loaded)
	}
}
