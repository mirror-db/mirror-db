package forward

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/server"
	"github.com/spf13/cobra"
)

// Cmd returns the cobra command for forward.
func Cmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forward [listen-addr]",
		Short: "L4 TCP forwarder with optional SSH disguise unwrap",
		Long: `Forward TCP connections to an upstream, optionally unwrapping SSH disguise.

This is an operator convenience: it exposes the ssh:// (SSH-banner disguised)
transport that only the relay itself normally speaks, so a local client can
reach an upstream relay through it.

  local client ──plain TCP──▶ forward :8443 ──SSH banner + TLS──▶ upstream :344

Schemes:
  https://host[:port]  — plain TCP forward (default port 443)
  ssh://host:port      — connect via SSH-banner disguise, forward plain TLS

Examples:
  relay forward --upstream https://foobar.com
  relay forward --upstream ssh://foobar.com:344
  relay forward --upstream ssh://foobar.com:344 :8443`,
		Args: cobra.MaximumNArgs(1),
		RunE: run,
	}

	cmd.Flags().String("upstream", "", "upstream URL (https:// or ssh://host:port)")
	if err := cmd.MarkFlagRequired("upstream"); err != nil {
		panic(err)
	}

	return cmd
}

type forwardConfig struct {
	listenAddr   string
	upstreamAddr string
	disguise     bool
	upstreamHost string // for display
}

func parseConfig(cmd *cobra.Command, args []string) (*forwardConfig, error) {
	raw, err := cmd.Flags().GetString("upstream")
	if err != nil {
		return nil, err
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL: %w", err)
	}

	cfg := &forwardConfig{upstreamHost: u.Hostname()}

	switch u.Scheme {
	case "https":
		port := u.Port()
		if port == "" {
			port = "443"
		}
		cfg.upstreamAddr = net.JoinHostPort(u.Hostname(), port)
	case "ssh":
		port := u.Port()
		if port == "" {
			return nil, fmt.Errorf("ssh:// scheme requires explicit port")
		}
		cfg.upstreamAddr = net.JoinHostPort(u.Hostname(), port)
		cfg.disguise = true
	default:
		return nil, fmt.Errorf("unsupported scheme %q (use https:// or ssh://)", u.Scheme)
	}

	if cfg.upstreamHost == "" {
		return nil, fmt.Errorf("upstream URL has no host")
	}

	if len(args) > 0 {
		cfg.listenAddr = args[0]
	} else {
		cfg.listenAddr = ":443"
	}

	return cfg, nil
}

func run(cmd *cobra.Command, args []string) error {
	cfg, err := parseConfig(cmd, args)
	if err != nil {
		return err
	}

	mode := "plain TCP"
	if cfg.disguise {
		mode = "SSH disguise → plain TLS"
	}

	log.Printf("forward %s → %s (%s)", cfg.listenAddr, cfg.upstreamAddr, mode)

	ln, err := net.Listen("tcp", cfg.listenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()
	log.Printf("listening on %s", ln.Addr())

	fmt.Println()
	fmt.Println("Test:")
	fmt.Printf("  curl -o /dev/null --connect-to '%s:443:127.0.0.1:%s' 'https://%s/speedtest/256'\n\n",
		cfg.upstreamHost, portFromAddr(ln.Addr().String()), cfg.upstreamHost)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			log.Printf("accept: %v", err)
			continue
		}
		go handleForward(conn, cfg)
	}
}

func handleForward(client net.Conn, cfg *forwardConfig) {
	defer client.Close()

	upstream, err := net.DialTimeout("tcp", cfg.upstreamAddr, 10*time.Second)
	if err != nil {
		log.Printf("dial upstream: %v", err)
		return
	}
	defer upstream.Close()

	if cfg.disguise {
		if err := server.ClientBannerExchange(upstream); err != nil {
			log.Printf("disguise handshake: %v", err)
			return
		}
	}

	pipe(client, upstream)
}

// pipe copies bidirectionally until BOTH directions finish.
//
// Waiting on only one direction truncates the response whenever the client
// half-closes first (every POST, every HTTP/2 GOAWAY): the deferred Close then
// kills the still-copying direction mid-body. Here each EOF instead half-closes
// the opposite side so buffered bytes flush, and only two finished copies
// close the connection.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		closeWrite(a)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		closeWrite(b)
	}()

	wg.Wait()
}

// closeWrite sends FIN without tearing down the read side.
func closeWrite(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

func portFromAddr(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		if strings.HasPrefix(addr, ":") {
			return addr[1:]
		}
		return "443"
	}
	return port
}
