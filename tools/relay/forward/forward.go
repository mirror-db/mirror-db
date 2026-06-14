package forward

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const sshBanner = "SSH-2.0-OpenSSH_9.6\r\n"

// Cmd returns the cobra command for forward.
func Cmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forward [listen-addr]",
		Short: "L4 TCP forwarder with optional SSH disguise unwrap",
		Long: `Forward TCP connections to an upstream, optionally unwrapping SSH disguise.

Schemes:
  https://host[:port]  — plain TCP forward (default port 443)
  ssh://host:port      — connect via SSH-banner disguise, forward plain TLS

Examples:
  relay forward --upstream https://foobar.com
  relay forward --upstream ssh://foobar.com:344
  relay forward --upstream ssh://foobar.com:344 :8443`,
		Args: cobra.MaximumNArgs(1),
		Run:  run,
	}

	cmd.Flags().String("upstream", "", "upstream URL (https:// or ssh://host:port)")
	cmd.MarkFlagRequired("upstream")

	return cmd
}

type forwardConfig struct {
	listenAddr   string
	upstreamAddr string
	disguise     bool
	upstreamHost string // for display
}

func parseConfig(cmd *cobra.Command, args []string) (*forwardConfig, error) {
	raw, _ := cmd.Flags().GetString("upstream")

	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL: %w", err)
	}

	cfg := &forwardConfig{}

	switch u.Scheme {
	case "https":
		host := u.Hostname()
		port := u.Port()
		if port == "" {
			port = "443"
		}
		cfg.upstreamAddr = net.JoinHostPort(host, port)
		cfg.upstreamHost = host
		cfg.disguise = false
	case "ssh":
		host := u.Hostname()
		port := u.Port()
		if port == "" {
			return nil, fmt.Errorf("ssh:// scheme requires explicit port")
		}
		cfg.upstreamAddr = net.JoinHostPort(host, port)
		cfg.upstreamHost = host
		cfg.disguise = true
	default:
		return nil, fmt.Errorf("unsupported scheme %q (use https:// or ssh://)", u.Scheme)
	}

	// Listen address
	if len(args) > 0 {
		cfg.listenAddr = args[0]
	} else {
		cfg.listenAddr = ":443"
	}

	return cfg, nil
}

func run(cmd *cobra.Command, args []string) {
	cfg, err := parseConfig(cmd, args)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	mode := "plain TCP"
	if cfg.disguise {
		mode = "SSH disguise → plain TLS"
	}

	log.Printf("forward %s → %s (%s)", cfg.listenAddr, cfg.upstreamAddr, mode)
	log.Println()

	// Print test examples
	listenPort := portFromAddr(cfg.listenAddr)
	fmt.Println("Test Examples:")
	fmt.Printf("  curl -o /tmp/speedtest --connect-to '%s:443:127.0.0.1:%s' 'https://%s/speedtest/256'\n",
		cfg.upstreamHost, listenPort, cfg.upstreamHost)
	fmt.Println()

	ln, err := net.Listen("tcp", cfg.listenAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("listening on %s", ln.Addr())

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handleForward(conn, cfg)
	}
}

func handleForward(client net.Conn, cfg *forwardConfig) {
	defer client.Close()

	upstream, err := net.Dial("tcp", cfg.upstreamAddr)
	if err != nil {
		log.Printf("dial upstream: %v", err)
		return
	}
	defer upstream.Close()

	if cfg.disguise {
		// Perform SSH banner exchange with upstream (we are the client)
		upstream.SetDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, len(sshBanner))
		if _, err := io.ReadFull(upstream, buf); err != nil {
			log.Printf("disguise read banner: %v", err)
			return
		}
		if _, err := upstream.Write([]byte(sshBanner)); err != nil {
			log.Printf("disguise write banner: %v", err)
			return
		}
		upstream.SetDeadline(time.Time{})
	}

	// Bidirectional copy (L4 forward)
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, client); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

func portFromAddr(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Maybe just ":443"
		if strings.HasPrefix(addr, ":") {
			return addr[1:]
		}
		return "443"
	}
	return port
}
