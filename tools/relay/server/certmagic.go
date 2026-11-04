package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"github.com/mirror-db/mirror-db/tools/relay/certclient"
	"github.com/mirror-db/mirror-db/tools/relay/config"
)

// CertMagicStorage returns the storage backend for this process:
//   - ACME mode: a local file store under the config dir.
//   - cert-pack mode: the shared CertClient snapshot of the upstream pack.
//
// Resolved once per process; every caller (TLS config, /cert-pack endpoint,
// renewals) must see the same instance.
var CertMagicStorage = sync.OnceValue(func() certmagic.Storage {
	if c := buildCertClient(); c != nil {
		return c
	}
	return &certmagic.FileStorage{Path: config.ResolveConfig("certmagic")}
})

// domainTokenMap builds a mapping from domain suffix to CF API token.
// For a config like [{domain:"a.com", cf_api_token:"tok-a"}, {domain:"b.com", cf_api_token:"tok-b"}],
// a cert for "a.com" or "*.a.com" uses tok-a, "b.com"/"*.b.com" uses tok-b.
func domainTokenMap() map[string]string {
	m := make(map[string]string)
	for _, bd := range config.GetBaseDomains() {
		if bd.CFAPIToken != "" {
			m[bd.Domain] = bd.CFAPIToken
		}
	}
	return m
}

// tokenForCertName finds the CF API token for a given cert name (domain or wildcard).
func tokenForCertName(name string, tokens map[string]string) string {
	// Strip wildcard prefix
	name = strings.TrimPrefix(name, "*.")
	// Direct match
	if tok, ok := tokens[name]; ok {
		return tok
	}
	// Walk up domain (sub.a.com → a.com)
	for {
		idx := strings.IndexByte(name, '.')
		if idx < 0 {
			break
		}
		name = name[idx+1:]
		if tok, ok := tokens[name]; ok {
			return tok
		}
	}
	return ""
}

// makeIssuer creates an ACME issuer with the given CF token for DNS-01.
func makeIssuer(cfg *certmagic.Config, cfToken string) certmagic.Issuer {
	acmeIssuer := certmagic.ACMEIssuer{
		CA:     resolveCertAuthority(config.GetAcmeCA()),
		TestCA: certmagic.DefaultACME.TestCA,
		Logger: certmagic.DefaultACME.Logger,
		Email:  config.GetAcmeEmail(),
		Agreed: config.GetAcmeAgree(),
	}
	if cfToken != "" {
		acmeIssuer.DNS01Solver = &certmagic.DNS01Solver{
			DNSManager: certmagic.DNSManager{
				DNSProvider: &cloudflare.Provider{
					APIToken: cfToken,
				},
			},
		}
	}
	return certmagic.NewACMEIssuer(cfg, acmeIssuer)
}

// buildCertMagicConfig creates a certmagic config with per-domain DNS-01 solver.
// Returns both the config (for TLS) and the cache (for ManageSync per-token).
func buildCertMagicConfig() (*certmagic.Config, *certmagic.Cache) {
	storage := CertMagicStorage()
	tokens := domainTokenMap()

	// GetConfigForCert is called per-certificate for renewal.
	// Select the correct CF API token based on which domain the cert covers.
	// The config returned here must be built with certmagic.New so it carries a
	// pointer back to this cache. A hand-built &certmagic.Config{} has a nil
	// cache, and certmagic's maintenance loop rejects it:
	//
	//   error maintenance unable to get configuration to manage certificate;
	//   unable to renew: config returned for certificate [x] has nil cache
	//
	// which means renewals never run — the certificates simply expire. The cache
	// pointer is stored after construction because NewCache needs the closure
	// first; certmagic only calls it from the maintenance goroutine.
	var cachePtr atomic.Pointer[certmagic.Cache]

	cacheOpts := certmagic.CacheOptions{
		GetConfigForCert: func(cert certmagic.Certificate) (*certmagic.Config, error) {
			c := cachePtr.Load()
			if c == nil {
				return nil, fmt.Errorf("certmagic cache not initialised yet")
			}
			tok := tokenForCertName(cert.Names[0], tokens)
			cfg := certmagic.New(c, certmagic.Config{
				RenewalWindowRatio: certmagic.Default.RenewalWindowRatio,
				KeySource:          certmagic.Default.KeySource,
				Logger:             certmagic.Default.Logger,
				Storage:            storage,
			})
			cfg.Issuers = []certmagic.Issuer{makeIssuer(cfg, tok)}
			return cfg, nil
		},
		Logger: certmagic.Default.Logger,
	}

	cache := certmagic.NewCache(cacheOpts)
	cachePtr.Store(cache)

	// Default config — issuer placeholder; actual issuance done via manageDomainsPerToken.
	cmCfg := certmagic.Config{
		RenewalWindowRatio: certmagic.Default.RenewalWindowRatio,
		KeySource:          certmagic.Default.KeySource,
		Logger:             certmagic.Default.Logger,
		Storage:            storage,
	}
	cmCfg.Issuers = []certmagic.Issuer{makeIssuer(&cmCfg, "")}

	return certmagic.New(cache, cmCfg), cache
}

// manageDomainsPerToken groups domains by their CF token and calls ManageSync
// with the correct issuer for each group. This ensures initial cert acquisition
// uses the right token (GetConfigForCert only applies to renewals).
func manageDomainsPerToken(ctx context.Context, cache *certmagic.Cache, storage certmagic.Storage) error {
	// Group domains by token
	type group struct {
		token   string
		domains []string
	}
	groups := make(map[string]*group)
	for _, bd := range config.GetBaseDomains() {
		tok := bd.CFAPIToken
		g, ok := groups[tok]
		if !ok {
			g = &group{token: tok}
			groups[tok] = g
		}
		g.domains = append(g.domains, bd.Domain, "*."+bd.Domain)
	}

	for _, g := range groups {
		cfg := certmagic.Config{
			RenewalWindowRatio: certmagic.Default.RenewalWindowRatio,
			KeySource:          certmagic.Default.KeySource,
			Logger:             certmagic.Default.Logger,
			Storage:            storage,
		}
		cfg.Issuers = []certmagic.Issuer{makeIssuer(&cfg, g.token)}

		cm := certmagic.New(cache, cfg)
		if err := cm.ManageSync(ctx, g.domains); err != nil {
			return fmt.Errorf("manage %v: %w", g.domains, err)
		}
	}
	return nil
}

// domainTokenMap is also used by manageDomainsPerToken — already defined above.
// (tokenForCertName walks up parent domains to find the matching token.)

// ListenAndServeHTTPS starts HTTP (keepHTTP + ACME + redirect) and HTTPS listeners.
func ListenAndServeHTTPS(srv *Server) error {
	ctx := context.Background()
	cm, cache := buildCertMagicConfig()
	storage := CertMagicStorage()

	// ACME mode is the only one that needs to acquire/keep certificates; in
	// cert-pack mode the parent owns them and this relay is a pure consumer.
	if !config.IsAcmeUpstream() {
		// Manage certs per-token group so each domain uses the correct CF API token.
		if err := manageDomainsPerToken(ctx, cache, storage); err != nil {
			return fmt.Errorf("certmagic manage: %w", err)
		}
	} else {
		// certmagic consults Storage on its own only for names it manages, or
		// when on-demand TLS is enabled. A cert-pack consumer does neither, so
		// without an explicit load the pack sits in storage and every handshake
		// answers "no certificate available"
		// (docs/adr/0008-cert-pack-consumer-caches-the-pack.md).
		pc := newPackCache(cache, cm, storage)
		if n, err := pc.reload(ctx); err != nil {
			log.Printf("cert-pack: %v (no TLS until it can be read)", err)
		} else {
			log.Printf("cert-pack: %d certificates loaded", n)
		}
		go pc.run(ctx)
	}
	// HTTP listener
	httpLn, err := net.Listen("tcp", fmt.Sprintf(":%d", certmagic.HTTPPort))
	if err != nil {
		return fmt.Errorf("http listen: %w", err)
	}

	// HTTPS listener
	tlsConfig := cm.TLSConfig()
	tlsConfig.NextProtos = append([]string{"h2", "http/1.1"}, tlsConfig.NextProtos...)
	httpsLn, err := tls.Listen("tcp", fmt.Sprintf(":%d", certmagic.HTTPSPort), tlsConfig)
	if err != nil {
		httpLn.Close()
		return fmt.Errorf("https listen: %w", err)
	}

	// HTTP handler: ACME challenge wraps the HTTPRouter (keepHTTP + redirect).
	// In cert-pack mode this relay issues nothing, so it must not advertise an
	// ACME challenge endpoint.
	var httpHandler http.Handler = srv.HTTPRouter
	if !config.IsAcmeUpstream() && len(cm.Issuers) > 0 {
		if am, ok := cm.Issuers[0].(*certmagic.ACMEIssuer); ok {
			httpHandler = am.HTTPChallengeHandler(srv.HTTPRouter)
		}
	}

	httpSrv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       5 * time.Minute,
		Handler:           httpHandler,
	}

	httpsSrv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       5 * time.Minute,
		Handler:           srv.Router,
	}

	log.Printf("HTTPS on %s, HTTP on %s", httpsLn.Addr(), httpLn.Addr())
	go httpSrv.Serve(httpLn)

	// Disguise listener (SSH-banner-wrapped TLS) on optional extra port.
	if port := config.GetDisguisePort(); port != 0 {
		dLn, err := newDisguiseListener(fmt.Sprintf(":%d", port), tlsConfig)
		if err != nil {
			httpLn.Close()
			httpsLn.Close()
			return fmt.Errorf("disguise listen: %w", err)
		}
		log.Printf("Disguise (SSH+TLS) on :%d", port)
		// Same handler set as :443, so HTTP/2 (already in tlsConfig.NextProtos)
		// works identically on the disguised port.
		go httpsSrv.Serve(dLn)
	}

	return httpsSrv.Serve(httpsLn)
}

func resolveCertAuthority(ca string) string {
	switch ca {
	case "production":
		return certmagic.LetsEncryptProductionCA
	case "staging":
		return certmagic.LetsEncryptStagingCA
	default:
		return ca
	}
}

// buildCertClient returns the cert-pack client for this process, or nil in
// ACME mode. The instance is shared so the deferred TLS config, the cert-pack
// endpoint and the certmagic storage all read the same snapshot.
var buildCertClient = sync.OnceValue(func() *certclient.CertClient {
	if !config.IsAcmeUpstream() {
		return nil
	}
	c := &certclient.CertClient{
		CertPackURL: config.CertPackURL(),
		SecretKey:   config.GetSecretKey(),
		Transport:   certTransport(),
	}
	if err := fetchPackWithRetry(c); err != nil {
		log.Fatalf("failed to fetch cert-pack from upstream: %v", err)
	}
	return c
})

// fetchPackWithRetry retries the first pack fetch a few times.
//
// The process cannot listen without certificates, so a failure here is fatal —
// but a first attempt can lose to a parent that is restarting at that exact
// moment (a one-second overlap is enough: observed, connection refused), and
// dying on that costs a systemd restart cycle for a problem that fixes itself in
// seconds.
func fetchPackWithRetry(c *certclient.CertClient) error {
	const attempts = 3

	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * 2 * time.Second)
			log.Printf("cert-pack: retrying (%d/%d)", i+1, attempts)
		}
		if err = c.Refresh(); err == nil {
			return nil
		}
		log.Printf("cert-pack: fetch failed: %v", err)
	}
	return err
}

// certTransport follows the same upstream path rules as proxying, so a relay
// that can only reach its parent through the SSH disguise can still sync certs.
func certTransport() http.RoundTripper {
	if px := config.GetUpstreamProxy(); px != "" {
		if t, err := buildProxyTransport(px); err == nil {
			return t
		}
	}
	if dp := config.GetUpstreamDisguisePort(); dp != 0 {
		if u, err := url.Parse(config.CertPackURL()); err == nil {
			return buildDisguiseTransport(u.Hostname(), dp)
		}
	}
	return http.DefaultTransport
}

// packCache loads a certificate pack into certmagic's in-memory cache and keeps
// it in step with what the upstream publishes.
//
// This exists because certmagic will not do it: `getCertificate` only reads
// Storage when it is managing the name itself (which a cert-pack consumer does
// not) or when on-demand TLS is enabled (which would try to issue). Without an
// explicit load, a pack that is perfectly present in storage produces
// "no certificate available for 'example.com'" on every handshake.
type packCache struct {
	cache   *certmagic.Cache
	cm      *certmagic.Config
	storage certmagic.Storage

	mu     sync.Mutex
	hashes []string
}

func newPackCache(cache *certmagic.Cache, cm *certmagic.Config, storage certmagic.Storage) *packCache {
	return &packCache{cache: cache, cm: cm, storage: storage}
}

// reload loads every certificate in the pack, and drops any that has left it.
func (p *packCache) reload(ctx context.Context) (int, error) {
	// Recursive on purpose: the pack is keyed by issuer directory
	// (certificates/<issuer>/<name>/), so a non-recursive listing of
	// "certificates" stops above the names we want.
	keys, err := p.storage.List(ctx, "certificates", true)
	if errors.Is(err, fs.ErrNotExist) {
		// A pack with no certificates yet is not a failure: the parent may still
		// be issuing. Serving nothing is correct until it does.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("list certificates: %w", err)
	}

	var (
		loaded int
		hashes []string
	)
	for _, key := range keys {
		if !strings.HasSuffix(key, ".crt") {
			continue
		}
		// certmagic pairs <name>.crt with <name>.key in the same directory.
		keyFile := strings.TrimSuffix(key, ".crt") + ".key"

		certPEM, err := p.storage.Load(ctx, key)
		if err != nil {
			log.Printf("cert-pack: read %s: %v", key, err)
			continue
		}
		keyPEM, err := p.storage.Load(ctx, keyFile)
		if err != nil {
			log.Printf("cert-pack: read %s: %v", keyFile, err)
			continue
		}

		hash, err := p.cm.CacheUnmanagedCertificatePEMBytes(ctx, certPEM, keyPEM, nil)
		if err != nil {
			log.Printf("cert-pack: cache %s: %v", key, err)
			continue
		}
		hashes = append(hashes, hash)
		loaded++
	}

	p.mu.Lock()
	previous := p.hashes
	p.hashes = hashes
	p.mu.Unlock()

	// A renewed certificate hashes differently; without this the old one would
	// linger in the cache beside its replacement and could still be selected.
	if stale := missingFrom(hashes, previous); len(stale) > 0 {
		p.cache.Remove(stale)
	}

	return loaded, nil
}

// run re-reads the pack on a timer. The storage refreshes itself lazily on
// access, so this is what turns "the parent renewed a certificate" into "this
// relay serves the renewed one" — within half an hour, which is far inside any
// renewal window.
func (p *packCache) run(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := p.reload(ctx)
			if err != nil {
				log.Printf("cert-pack sync: %v (keeping what is cached)", err)
				continue
			}
			log.Printf("cert-pack: %d certificates loaded", n)
		}
	}
}

// missingFrom returns the hashes in previous that are absent from current.
func missingFrom(current, previous []string) []string {
	keep := make(map[string]struct{}, len(current))
	for _, h := range current {
		keep[h] = struct{}{}
	}
	var stale []string
	for _, h := range previous {
		if _, ok := keep[h]; !ok {
			stale = append(stale, h)
		}
	}
	return stale
}
