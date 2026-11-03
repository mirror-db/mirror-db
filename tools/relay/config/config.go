package config

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/joho/godotenv/autoload"
	"github.com/spf13/viper"
)

// BaseDomain represents one domain the relay serves, with its own CF API token for DNS-01.
type BaseDomain struct {
	Domain     string `mapstructure:"domain" json:"domain"`
	CFAPIToken string `mapstructure:"cf_api_token" json:"cf_api_token"`
}

func init() {
	// Defaults
	viper.SetDefault("relay.config_dir", "/etc/mirror-relay")

	viper.SetDefault("upstream.url", "https://mirs.uk")
	viper.SetDefault("upstream.transport", "latency")

	viper.SetDefault("acme.source", "acme")
	viper.SetDefault("acme.email", "acme@mirs.uk")
	viper.SetDefault("acme.ca", "production")
	viper.SetDefault("acme.agree", true)

	// Env override (highest priority).
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// Explicit bindings for the two keys whose env var names are not the plain
	// upper-cased form. Without these, RELAY_CONFIG_DIR would be visible only to
	// the one caller that reads os.Getenv directly while every other path went
	// through viper — config read from one dir, key + certs written to another.
	mustBind("relay.config_dir", "RELAY_CONFIG_DIR")
	mustBind("relay.base_domains", "RELAY_BASE_DOMAINS")

	// Config file
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(GetConfigDir())

	if err := viper.ReadInConfig(); err != nil {
		log.Println("no config file found, using defaults + env")
	}
}

func mustBind(key, envVar string) {
	if err := viper.BindEnv(key, envVar); err != nil {
		log.Fatalf("failed to bind %s to %s: %v", key, envVar, err)
	}
}

// decodeBaseDomains parses the JSON array form used by RELAY_BASE_DOMAINS.
func decodeBaseDomains(raw string) ([]BaseDomain, error) {
	var domains []BaseDomain
	if err := json.Unmarshal([]byte(raw), &domains); err != nil {
		return nil, err
	}
	return domains, nil
}

// --- Accessors ---

// GetConfigDir returns the directory holding config.yaml, secret.key and the
// certmagic store. Every path in this package derives from it, so the config
// file, the secret and the certificates can never diverge.
func GetConfigDir() string         { return viper.GetString("relay.config_dir") }
func GetDisguisePort() int         { return viper.GetInt("relay.disguise_port") }
func GetUpstreamURL() string       { return viper.GetString("upstream.url") }
func GetUpstreamDisguisePort() int { return viper.GetInt("upstream.disguise_port") }
func GetUpstreamProxy() string     { return strings.TrimSpace(viper.GetString("upstream.proxy")) }
func GetAcmeSource() string        { return viper.GetString("acme.source") }
func GetAcmeEmail() string         { return viper.GetString("acme.email") }
func GetAcmeCA() string            { return viper.GetString("acme.ca") }
func GetAcmeAgree() bool           { return viper.GetBool("acme.agree") }

// --- Cloudflare edge selection ---

// GetUpstreamTransport selects which pool the proxy draws from: "latency"
// (default) or "speed".
//
// They rank differently on purpose: latency decides a manifest fetch, where a
// few kilobytes round-trip time dominates, while speed decides a large layer
// download. A mirror serving mostly blobs can prefer "speed"; the default suits
// mixed traffic.
func GetUpstreamTransport() string {
	v := strings.ToLower(strings.TrimSpace(viper.GetString("upstream.transport")))
	if v == "speed" {
		return "speed"
	}
	return "latency"
}

// CFConfig values for the edge-selection engine. Zero means "use the engine's
// default"; the engine owns the defaults so they stay in one place.
func GetCfPoolCapacity() int        { return viper.GetInt("cf.latency_capacity") }
func GetCfSpeedCapacity() int       { return viper.GetInt("cf.speed_capacity") }
func GetCfTopN() int                { return viper.GetInt("cf.top_n") }
func GetCfSeedCount() int           { return viper.GetInt("cf.seed_count") }
func GetCfSpeedSeedCount() int      { return viper.GetInt("cf.speed_seed_count") }
func GetCfLatencyURL() string       { return strings.TrimSpace(viper.GetString("cf.latency_url")) }
func GetCfSpeedURL() string         { return strings.TrimSpace(viper.GetString("cf.speed_url")) }
func GetCfLatencyMinScore() float64 { return viper.GetFloat64("cf.latency_min_score") }
func GetCfSpeedMinScore() float64   { return viper.GetFloat64("cf.speed_min_score") }
func GetCfPenalty() float64         { return viper.GetFloat64("cf.penalty") }
func GetCfScanSamples() int         { return viper.GetInt("cf.scan_samples") }
func GetCfRecruitSamples() int      { return viper.GetInt("cf.recruit_samples") }

func GetCfScanInterval() time.Duration {
	return viper.GetDuration("cf.scan_interval")
}
func GetCfPatrolLatencyInterval() time.Duration {
	return viper.GetDuration("cf.patrol_latency_interval")
}
func GetCfPatrolSpeedInterval() time.Duration {
	return viper.GetDuration("cf.patrol_speed_interval")
}
func GetCfRecruitInterval() time.Duration {
	return viper.GetDuration("cf.recruit_interval")
}

// GetCfPatrolIntensity scales how much a patrol measures: 1 re-measures every
// pool member once, 0.5 measures half, above 1 averages repeated samples.
func GetCfPatrolIntensity() float64 {
	if v := viper.GetFloat64("cf.patrol_intensity"); v > 0 {
		return v
	}
	return 1
}

// GetCfCandidatesFile is where the candidate snapshot lives. It lets a restart
// skip exploring every published range, and it is the successor to the old
// cf-nodes.csv (which nothing reads any more).
func GetCfCandidatesFile() string {
	if p := strings.TrimSpace(viper.GetString("cf.candidates_file")); p != "" {
		return p
	}
	return ResolveConfig("cf-candidates.csv")
}

// GetCfSnapshotInterval is how often the candidate snapshot is refreshed. It is
// also written on shutdown.
func GetCfSnapshotInterval() time.Duration {
	if d := viper.GetDuration("cf.snapshot_interval"); d > 0 {
		return d
	}
	return time.Hour
}

// GetCfBootstrapHost is resolved when no snapshot exists, to seed the first
// candidate list. Defaults to the upstream hostname: the name clients actually
// use is a better prior than a random IP per subnet.
func GetCfBootstrapHost() string {
	if h := strings.TrimSpace(viper.GetString("cf.bootstrap_host")); h != "" {
		return h
	}
	u, err := url.Parse(GetUpstreamURL())
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// GetCfInitTimeout bounds how long startup waits for the edge pools to fill.
// Exceeding it is not fatal: the relay falls back to unoptimised transport.
func GetCfInitTimeout() time.Duration {
	if d := viper.GetDuration("cf.init_timeout"); d > 0 {
		return d
	}
	return 30 * time.Second
}

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
	// Env var holds JSON; viper cannot decode it into []BaseDomain on its own.
	if raw := strings.TrimSpace(os.Getenv("RELAY_BASE_DOMAINS")); raw != "" {
		domains, err := decodeBaseDomains(raw)
		if err != nil {
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
		// 0750: the service user can read it; nobody else can list the dir.
		if err := os.MkdirAll(GetConfigDir(), 0750); err != nil {
			log.Fatalf("failed to create config dir: %v", err)
		}
		key := rand.Text()
		// 0640: readable by the service group, not world-readable.
		if err := os.WriteFile(file, []byte(key), 0640); err != nil {
			log.Fatalf("failed to write secret key: %v", err)
		}
	}

	data, err := os.ReadFile(file)
	if err != nil {
		log.Fatalf("failed to read secret key: %v", err)
	}
	return bytes.TrimSpace(data)
}
