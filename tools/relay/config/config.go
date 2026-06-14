package config

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "github.com/joho/godotenv/autoload"
	"github.com/spf13/viper"
)

// BaseDomain represents one domain the relay serves, with its own CF API token for DNS-01.
type BaseDomain struct {
	Domain     string `mapstructure:"domain"`
	CFAPIToken string `mapstructure:"cf_api_token"`
}

func init() {
	// Defaults
	viper.SetDefault("relay.config_dir", "/etc/mirror-relay")

	viper.SetDefault("upstream.url", "https://mirs.uk")
	viper.SetDefault("upstream.pool_size", 25)

	viper.SetDefault("acme.source", "acme")
	viper.SetDefault("acme.email", "acme@mirs.uk")
	viper.SetDefault("acme.ca", "production")
	viper.SetDefault("acme.agree", true)

	// Config file
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(getConfigDir())
	viper.AddConfigPath(".")

	if err := viper.ReadInConfig(); err != nil {
		log.Println("no config file found, using defaults + env")
	}

	// Env override (highest priority)
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
}

func getConfigDir() string {
	if dir := os.Getenv("RELAY_CONFIG_DIR"); dir != "" {
		return dir
	}
	return "/etc/mirror-relay"
}

// --- Accessors ---

func GetConfigDir() string         { return viper.GetString("relay.config_dir") }
func GetPoolSize() int             { return viper.GetInt("upstream.pool_size") }
func GetCfNodesFile() string       { return ResolveConfig("cf-nodes.csv") }
func GetDisguisePort() int         { return viper.GetInt("relay.disguise_port") }
func GetUpstreamURL() string       { return viper.GetString("upstream.url") }
func GetUpstreamDisguisePort() int { return viper.GetInt("upstream.disguise_port") }
func GetAcmeSource() string        { return viper.GetString("acme.source") }
func GetAcmeEmail() string         { return viper.GetString("acme.email") }
func GetAcmeCA() string            { return viper.GetString("acme.ca") }
func GetAcmeAgree() bool           { return viper.GetBool("acme.agree") }

// IsAcmeUpstream returns true when acme.source is not "acme" (i.e. a URL or "upstream").
func IsAcmeUpstream() bool {
	s := GetAcmeSource()
	return s != "" && s != "acme"
}

// CertPackURL returns the cert-pack endpoint for upstream cert sync.
// acme.source can be a URL origin (e.g. "https://anitya.net") or "upstream"
// which falls back to upstream.url.
func CertPackURL() string {
	s := GetAcmeSource()
	if s == "upstream" {
		s = GetUpstreamURL()
	}
	return strings.TrimRight(s, "/") + "/.mirror-relay/cert-pack"
}

// GetBaseDomains returns the list of domains this relay serves.
// Supports both config file (yaml array) and env var (JSON string):
//
//	RELAY_BASE_DOMAINS='[{"domain":"x.com","cf_api_token":"tok"}]'
var GetBaseDomains = sync.OnceValue(func() []BaseDomain {
	// Try env var as JSON first (highest priority)
	if raw := os.Getenv("RELAY_BASE_DOMAINS"); raw != "" {
		var domains []BaseDomain
		if err := json.Unmarshal([]byte(raw), &domains); err != nil {
			log.Fatalf("failed to parse RELAY_BASE_DOMAINS env (expect JSON array): %v", err)
		}
		return domains
	}

	// Fall back to config file
	var domains []BaseDomain
	if err := viper.UnmarshalKey("relay.base_domains", &domains); err != nil {
		log.Fatalf("failed to parse relay.base_domains: %v", err)
	}
	return domains
})

// AllDomainNames returns all domain strings (for certmagic, host matching, etc.)
func AllDomainNames() []string {
	domains := GetBaseDomains()
	names := make([]string, 0, len(domains))
	for _, d := range domains {
		if d.Domain != "" {
			names = append(names, d.Domain)
		}
	}
	return names
}

// ResolveConfig joins a filename to the config directory.
func ResolveConfig(name string) string {
	return filepath.Join(GetConfigDir(), name)
}

// --- Secret key ---

var GetSecretKey = sync.OnceValue(func() []byte {
	if sk := strings.TrimSpace(viper.GetString("relay.secret_key")); sk != "" {
		return []byte(sk)
	}
	return ensureSecretKeyFile()
})

func ensureSecretKeyFile() []byte {
	file := ResolveConfig("secret.key")

	_, err := os.Stat(file)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(GetConfigDir(), 0755); err != nil {
			log.Fatalf("failed to create config dir: %v", err)
		}
		key := rand.Text()
		if err := os.WriteFile(file, []byte(key), 0600); err != nil {
			log.Fatalf("failed to write secret key: %v", err)
		}
	}

	data, err := os.ReadFile(file)
	if err != nil {
		log.Fatalf("failed to read secret key: %v", err)
	}
	return bytes.TrimSpace(data)
}
