package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"github.com/mirror-db/mirror-db/tools/relay/certclient"
	"github.com/mirror-db/mirror-db/tools/relay/config"
)

// buildCertMagicStorage returns either file-based or upstream cert-client storage.
func buildCertMagicStorage() certmagic.Storage {
	if config.IsAcmeUpstream() {
		client := &certclient.CertClient{
			CertPackURL: config.CertPackURL(),
			SecretKey:   config.GetSecretKey(),
		}
		if err := client.Refresh(); err != nil {
			log.Fatalf("failed to fetch cert-pack from upstream: %v", err)
		}
		return client
	}
	return &certmagic.FileStorage{Path: config.ResolveConfig("certmagic")}
}

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
	storage := buildCertMagicStorage()
	tokens := domainTokenMap()

	// GetConfigForCert is called per-certificate for renewal.
	// Select the correct CF API token based on which domain the cert covers.
	cacheOpts := certmagic.CacheOptions{
		GetConfigForCert: func(cert certmagic.Certificate) (*certmagic.Config, error) {
			tok := tokenForCertName(cert.Names[0], tokens)
			cfg := &certmagic.Config{
				RenewalWindowRatio: certmagic.Default.RenewalWindowRatio,
				KeySource:          certmagic.Default.KeySource,
				Logger:             certmagic.Default.Logger,
				Storage:            storage,
			}
			cfg.Issuers = []certmagic.Issuer{makeIssuer(cfg, tok)}
			return cfg, nil
		},
		Logger: certmagic.Default.Logger,
	}

	cache := certmagic.NewCache(cacheOpts)

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
	storage := buildCertMagicStorage()

	// Manage certs per-token group so each domain uses the correct CF API token.
	if err := manageDomainsPerToken(ctx, cache, storage); err != nil {
		return fmt.Errorf("certmagic manage: %w", err)
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

	// HTTP handler: ACME challenge wraps the HTTPRouter (keepHTTP + redirect)
	var httpHandler http.Handler = srv.HTTPRouter
	if len(cm.Issuers) > 0 {
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

// GetCertMagicStorage exposes the storage (used by cert-pack endpoint).
func GetCertMagicStorage() certmagic.Storage {
	return buildCertMagicStorage()
}
