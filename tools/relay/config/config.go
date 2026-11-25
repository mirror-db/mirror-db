package config

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/joho/godotenv/autoload"
	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
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

	// Manifest refresh cadence. Zero is not a valid value: refusing to poll
	// would silently stop policy distribution.
	viper.SetDefault("relay.manifest_sync_interval", "10m")

	viper.SetDefault("acme.source", "acme")
	viper.SetDefault("acme.email", "acme@mirs.uk")
	viper.SetDefault("acme.ca", "production")
	viper.SetDefault("acme.agree", true)

	// Prefetch scheduler bounds. Zero/invalid values fall back to these so a
	// missing key never disables background refresh silently.
	viper.SetDefault("prefetch.concurrency", DefaultPrefetchConcurrency)
	viper.SetDefault("prefetch.max_tasks", DefaultPrefetchMaxTasks)

	// Env override (highest priority).
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// Explicit bindings for the two keys whose env var names are not the plain
	// upper-cased form. Without these, RELAY_CONFIG_DIR would be visible only to
	// the one caller that reads os.Getenv directly while every other path went
	// through viper — config read from one dir, key + certs written to another.
	mustBind("relay.config_dir", "RELAY_CONFIG_DIR")
	mustBind("relay.base_domains", "RELAY_BASE_DOMAINS")

	bindHashStoreEnv(viper.GetViper())

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

// --- Hash content store ---

// bindHashStoreEnv maps the cache keys onto the env names operators and the
// RustFS/AWS documentation already use. CACHE_DIR, CACHE_CAPACITY and
// CACHE_MAX_SIZE need no explicit binding: dots → underscores already match.
// S3_BUCKET is not CACHE_S3_BUCKET, so those four are bound by name.
func bindHashStoreEnv(v *viper.Viper) {
	for key, env := range map[string]string{
		"cache.s3.bucket":   "S3_BUCKET",
		"cache.s3.endpoint": "S3_ENDPOINT",
		"cache.s3.prefix":   "S3_PREFIX",
		"cache.s3.region":   "S3_REGION",
	} {
		if err := v.BindEnv(key, env); err != nil {
			log.Fatalf("failed to bind %s to %s: %v", key, env, err)
		}
	}
	v.SetDefault("cache.s3.region", "us-east-1")
}

// GetHashStoreConfig builds the content-store configuration and validates it, so
// a bad capacity or S3 endpoint fails at startup rather than at the first MISS.
// A nil result means the store is disabled: CACHE_DIR is the on/off switch.
func GetHashStoreConfig() (*hashstore.Config, error) {
	return hashStoreConfigFrom(viper.GetViper())
}

func hashStoreConfigFrom(v *viper.Viper) (*hashstore.Config, error) {
	dir := strings.TrimSpace(v.GetString("cache.dir"))
	if dir == "" {
		return nil, nil
	}

	capacityRaw := strings.TrimSpace(v.GetString("cache.capacity"))
	if capacityRaw == "" {
		return nil, errors.New("CACHE_CAPACITY is required when CACHE_DIR is set")
	}
	capacity, err := hashstore.ParseByteSize(capacityRaw)
	if err != nil {
		return nil, fmt.Errorf("CACHE_CAPACITY %q: %w", capacityRaw, err)
	}

	cfg := &hashstore.Config{Dir: dir, Capacity: capacity}
	if maxRaw := strings.TrimSpace(v.GetString("cache.max_size")); maxRaw != "" {
		maxSize, err := hashstore.ParseByteSize(maxRaw)
		if err != nil {
			return nil, fmt.Errorf("CACHE_MAX_SIZE %q: %w", maxRaw, err)
		}
		if maxSize > capacity {
			return nil, fmt.Errorf("CACHE_MAX_SIZE %q exceeds CACHE_CAPACITY %q", maxRaw, capacityRaw)
		}
		cfg.MaxSize = maxSize
	}

	bucket := strings.TrimSpace(v.GetString("cache.s3.bucket"))
	if bucket != "" {
		endpoint := strings.TrimRight(strings.TrimSpace(v.GetString("cache.s3.endpoint")), "/")
		if endpoint == "" {
			return nil, errors.New("S3_ENDPOINT is required when S3_BUCKET is set")
		}
		cfg.S3 = &hashstore.S3Config{
			Bucket:   bucket,
			Endpoint: endpoint,
			Prefix:   strings.TrimSpace(v.GetString("cache.s3.prefix")),
			Region:   strings.TrimSpace(v.GetString("cache.s3.region")),
		}
	}

	// Fill CACHE_MAX_SIZE's default (10% of capacity) and validate the rest.
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// DefaultManifestSyncInterval is the manifest refresh cadence when unset.
const DefaultManifestSyncInterval = 10 * time.Minute

// Prefetch defaults. The scheduler lives only in memory; the limits bound a
// single relay's background work, not the store.
const (
	DefaultPrefetchConcurrency = 4
	DefaultPrefetchMaxTasks    = 4096
)

// GetPrefetchConcurrency is how many background refreshes one relay may run at
// once. A non-positive or missing value means the default.
func GetPrefetchConcurrency() int {
	n := viper.GetInt("prefetch.concurrency")
	if n <= 0 {
		return DefaultPrefetchConcurrency
	}
	return n
}

// GetPrefetchMaxTasks bounds the in-memory lease table. A non-positive or
// missing value means the default.
func GetPrefetchMaxTasks() int {
	n := viper.GetInt("prefetch.max_tasks")
	if n <= 0 {
		return DefaultPrefetchMaxTasks
	}
	return n
}

// GetManifestSyncInterval is how often the mirror manifest is revalidated. It
// must be a positive duration: a non-positive or unparsable value is an error
// rather than a silent default, so policy distribution cannot be disabled by
// accident.
func GetManifestSyncInterval() (time.Duration, error) {
	raw := strings.TrimSpace(viper.GetString("relay.manifest_sync_interval"))
	if raw == "" {
		return DefaultManifestSyncInterval, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("relay.manifest_sync_interval %q is not a duration: %w", raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("relay.manifest_sync_interval %q must be positive", raw)
	}
	return d, nil
}

// --- Cloudflare edge selection ---

// CFConfig values for the edge-selection engine. Zero means "use the engine's
// default"; the engine owns the defaults so they stay in one place.
func GetCfLatencyCapacity() int     { return viper.GetInt("cf.latency_capacity") }
func GetCfSpeedCapacity() int       { return viper.GetInt("cf.speed_capacity") }
func GetCfTopN() int                { return viper.GetInt("cf.top_n") }
func GetCfSeedCount() int           { return viper.GetInt("cf.seed_count") }
func GetCfSpeedSeedCount() int      { return viper.GetInt("cf.speed_seed_count") }
func GetCfLatencyURL() string       { return strings.TrimSpace(viper.GetString("cf.latency_url")) }
func GetCfSpeedURL() string         { return strings.TrimSpace(viper.GetString("cf.speed_url")) }
func GetCfLatencyMinScore() float64 { return viper.GetFloat64("cf.latency_min_score") }
func GetCfSpeedMinScore() float64   { return viper.GetFloat64("cf.speed_min_score") }
func GetCfPenalty() float64         { return viper.GetFloat64("cf.penalty") }

func GetCfPatrolLatencyInterval() time.Duration {
	return viper.GetDuration("cf.patrol_latency_interval")
}
func GetCfPatrolSpeedInterval() time.Duration {
	return viper.GetDuration("cf.patrol_speed_interval")
}

// GetCfIntensity is the opening sweep's patience, as a fraction of one full
// cycle over the block list: the sweep stops after this many consecutive probes
// that did not improve the pool. 0.13 is ~303 probes against 2334 /24s.
func GetCfIntensity() float64 { return viper.GetFloat64("cf.intensity") }

// GetCfInterval is how long the steady state rests between single probes.
func GetCfInterval() time.Duration { return viper.GetDuration("cf.interval") }

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
