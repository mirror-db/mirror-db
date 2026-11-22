package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// RELAY_CONFIG_DIR must reach the same accessor every path derives from.
// Before the fix it was only read by one caller, so the config file could be
// loaded from one directory while secret.key and certmagic/ were written to
// another.
func TestConfigDirEnvIsSingleSourceOfTruth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RELAY_CONFIG_DIR", dir)

	// viper captured env at init time; mirror the binding for this process.
	if err := viper.BindEnv("relay.config_dir", "RELAY_CONFIG_DIR"); err != nil {
		t.Fatal(err)
	}

	if got := GetConfigDir(); got != dir {
		t.Fatalf("GetConfigDir() = %q, want %q", got, dir)
	}
	if got := ResolveConfig("secret.key"); got != filepath.Join(dir, "secret.key") {
		t.Fatalf("ResolveConfig = %q", got)
	}
	if got := ResolveConfig("certmagic"); got != filepath.Join(dir, "certmagic") {
		t.Fatalf("ResolveConfig(certmagic) = %q", got)
	}
}

func TestBaseDomainsFromEnv(t *testing.T) {
	t.Setenv("RELAY_BASE_DOMAINS", `[{"domain":"a.example.com","cf_api_token":"tok-a"},{"domain":"b.example.org"}]`)

	domains := GetBaseDomains()
	if len(domains) != 2 {
		t.Fatalf("got %d domains", len(domains))
	}
	if domains[0].Domain != "a.example.com" || domains[0].CFAPIToken != "tok-a" {
		t.Errorf("domain 0 = %+v", domains[0])
	}

	names := AllDomainNames()
	if len(names) != 2 || names[1] != "b.example.org" {
		t.Errorf("AllDomainNames = %v", names)
	}
}

func TestBaseDomainsInvalidEnvIsFatal(t *testing.T) {
	// log.Fatalf exits; verify the decode path via json instead of killing the
	// test binary.
	if _, err := decodeBaseDomains(`not json`); err == nil {
		t.Fatal("expected decode error")
	}
	if got, err := decodeBaseDomains(`[{"domain":"x.com"}]`); err != nil || len(got) != 1 {
		t.Fatalf("decode: %v %v", got, err)
	}
}

func TestAcmeSource(t *testing.T) {
	cases := []struct {
		source   string
		upstream bool
		packURL  string
	}{
		{"acme", false, ""},
		{"upstream", true, "https://mirs.uk/.mirror-relay/cert-pack"},
		{"https://parent.example.com/", true, "https://parent.example.com/.mirror-relay/cert-pack"},
	}
	for _, c := range cases {
		viper.Set("acme.source", c.source)
		if got := IsAcmeUpstream(); got != c.upstream {
			t.Errorf("source %q: IsAcmeUpstream = %v", c.source, got)
		}
		if c.upstream {
			if got := CertPackURL(); got != c.packURL {
				t.Errorf("source %q: CertPackURL = %q, want %q", c.source, got, c.packURL)
			}
		}
	}
	viper.Set("acme.source", "acme")
}

func TestManifestSyncInterval(t *testing.T) {
	const key = "relay.manifest_sync_interval"
	t.Cleanup(func() { viper.Set(key, "10m") })

	// Unset/empty falls back to the documented default.
	viper.Set(key, "")
	if got, err := GetManifestSyncInterval(); err != nil || got != DefaultManifestSyncInterval {
		t.Fatalf("default: got (%v,%v), want (%v,nil)", got, err, DefaultManifestSyncInterval)
	}

	viper.Set(key, "5m")
	if got, err := GetManifestSyncInterval(); err != nil || got != 5*time.Minute {
		t.Fatalf("override: got (%v,%v), want (5m,nil)", got, err)
	}

	for _, raw := range []string{"0", "-1m", "10", "not-a-duration"} {
		viper.Set(key, raw)
		if got, err := GetManifestSyncInterval(); err == nil {
			t.Errorf("%q: got (%v,nil), want an error", raw, got)
		}
	}
}

func TestSecretKeyFilePermissions(t *testing.T) {
	dir := t.TempDir()
	viper.Set("relay.config_dir", dir)
	t.Cleanup(func() { viper.Set("relay.config_dir", "/etc/mirror-relay") })

	key := ensureSecretKeyFile()
	if len(key) == 0 {
		t.Fatal("empty secret key")
	}

	info, err := os.Stat(ResolveConfig("secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		t.Errorf("secret.key mode %o is world-accessible", perm)
	}

	// Second call reads the same file rather than regenerating.
	if string(ensureSecretKeyFile()) != string(key) {
		t.Fatal("secret key is not stable across calls")
	}
}

// freshCacheViper mirrors the global viper's cache bindings without mutating it,
// so these tests can set keys and env vars without leaking into each other.
func freshCacheViper() *viper.Viper {
	v := viper.New()
	bindHashStoreEnv(v)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	return v
}

func TestHashStoreConfigDisabledWithoutCacheDir(t *testing.T) {
	v := freshCacheViper()
	v.Set("cache.dir", "")
	cfg, err := hashStoreConfigFrom(v)
	if err != nil || cfg != nil {
		t.Fatalf("got (%v,%v), want nil config and no error", cfg, err)
	}
}

func TestHashStoreConfigRequiresCapacity(t *testing.T) {
	v := freshCacheViper()
	v.Set("cache.dir", t.TempDir())
	if _, err := hashStoreConfigFrom(v); err == nil {
		t.Fatal("CACHE_DIR without CACHE_CAPACITY must fail")
	}
}

func TestHashStoreConfigDefaultMaxSizeIsTenPercent(t *testing.T) {
	v := freshCacheViper()
	v.Set("cache.dir", t.TempDir())
	v.Set("cache.capacity", "1GiB")

	cfg, err := hashStoreConfigFrom(v)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Capacity != 1<<30 {
		t.Errorf("capacity = %d, want %d", cfg.Capacity, 1<<30)
	}
	if want := int64(1<<30) / 10; cfg.MaxSize != want {
		t.Errorf("max size = %d, want 10%% of capacity (%d)", cfg.MaxSize, want)
	}
}

func TestHashStoreConfigSizeValidation(t *testing.T) {
	cases := []struct {
		name     string
		capacity string
		maxSize  string
		wantErr  bool
	}{
		{"explicit max size below capacity", "128GiB", "64MiB", false},
		{"max size equals capacity", "1MiB", "1MiB", false},
		{"invalid capacity unit", "12XB", "", true},
		{"zero capacity", "0", "", true},
		{"negative capacity", "-1GiB", "", true},
		{"max size above capacity", "1MiB", "2MiB", true},
		{"invalid max size", "1MiB", "lots", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := freshCacheViper()
			v.Set("cache.dir", t.TempDir())
			v.Set("cache.capacity", tc.capacity)
			if tc.maxSize != "" {
				v.Set("cache.max_size", tc.maxSize)
			}
			_, err := hashStoreConfigFrom(v)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// The S3_* env names are the contract; CACHE_* comes from the dotted-key mapping.
func TestHashStoreConfigS3EnvAndDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CACHE_DIR", dir)
	t.Setenv("CACHE_CAPACITY", "128GiB")
	t.Setenv("S3_BUCKET", "env-bucket")
	t.Setenv("S3_ENDPOINT", "https://rustfs.example:9000/")
	t.Setenv("S3_PREFIX", "/mirror/")
	t.Setenv("S3_REGION", "")

	cfg, err := hashStoreConfigFrom(freshCacheViper())
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Dir != dir {
		t.Errorf("dir = %q, want %q", cfg.Dir, dir)
	}
	if cfg.S3 == nil {
		t.Fatal("S3 section not built")
	}
	if cfg.S3.Bucket != "env-bucket" {
		t.Errorf("bucket = %q", cfg.S3.Bucket)
	}
	if cfg.S3.Endpoint != "https://rustfs.example:9000" {
		t.Errorf("endpoint = %q", cfg.S3.Endpoint)
	}
	if cfg.S3.Prefix != "mirror" {
		t.Errorf("prefix = %q", cfg.S3.Prefix)
	}
	if cfg.S3.Region != "us-east-1" {
		t.Errorf("region = %q, want the us-east-1 default", cfg.S3.Region)
	}
}

func TestHashStoreConfigS3NeedsEndpoint(t *testing.T) {
	v := freshCacheViper()
	v.Set("cache.dir", t.TempDir())
	v.Set("cache.capacity", "1GiB")
	v.Set("cache.s3.bucket", "bucket")
	v.Set("cache.s3.endpoint", "")
	if _, err := hashStoreConfigFrom(v); err == nil {
		t.Fatal("S3 bucket without an endpoint must fail")
	}
}

// The S3 prefix is cleaned once, at startup, so writes (path.Join) and prune's
// ListObjectsV2 prefix always refer to the same place.
func TestHashStoreConfigS3PrefixIsCleaned(t *testing.T) {
	v := freshCacheViper()
	v.Set("cache.dir", t.TempDir())
	v.Set("cache.capacity", "1GiB")
	v.Set("cache.s3.bucket", "bucket")
	v.Set("cache.s3.endpoint", "https://rustfs:9000")

	for raw, want := range map[string]string{
		"/relay//x/": "relay/x",
		"relay/":     "relay",
		"a/../b":     "b",
		"/":          "",
		"":           "",
	} {
		v.Set("cache.s3.prefix", raw)
		cfg, err := hashStoreConfigFrom(v)
		if err != nil {
			t.Fatalf("prefix %q: %v", raw, err)
		}
		if cfg.S3.Prefix != want {
			t.Errorf("prefix %q -> %q, want %q", raw, cfg.S3.Prefix, want)
		}
	}
}
