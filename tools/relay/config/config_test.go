package config

import (
	"os"
	"path/filepath"
	"testing"

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
