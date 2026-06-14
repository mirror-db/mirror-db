// Package server — SSH banner disguise for TLS connections.
//
// DPI classifies traffic by the first few bytes. By exchanging a fake SSH-2.0
// banner before starting TLS, the connection looks like SSH to middleboxes.
package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"time"
)

const sshBanner = "SSH-2.0-OpenSSH_9.6\r\n"

// --- Server side (listener wrapper) ---

// disguiseListener wraps a raw TCP listener: each accepted connection exchanges
// SSH banners, then the returned net.Conn is ready for TLS.
type disguiseListener struct {
	net.Listener
}

// newDisguiseListener creates a TCP listener on addr that performs SSH banner
// exchange, then wraps in TLS.
func newDisguiseListener(addr string, tlsConfig *tls.Config) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return tls.NewListener(&disguiseListener{ln}, tlsConfig), nil
}

func (l *disguiseListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if err := serverBannerExchange(conn); err != nil {
			log.Printf("disguise handshake failed: %v", err)
			conn.Close()
			continue
		}
		return conn, nil
	}
}

func serverBannerExchange(conn net.Conn) error {
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetDeadline(time.Time{})

	// Server sends banner first
	if _, err := conn.Write([]byte(sshBanner)); err != nil {
		return err
	}
	// Read client banner
	buf := make([]byte, len(sshBanner))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	return nil
}

// --- Client side (dialer) ---

// disguiseDialTLS dials a remote address, exchanges SSH banners, then performs
// TLS handshake. Returns a ready-to-use *tls.Conn.
func disguiseDialTLS(ctx context.Context, addr string, tlsConfig *tls.Config) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	if err := clientBannerExchange(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("disguise handshake: %w", err)
	}

	tlsConn := tls.Client(conn, tlsConfig)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

func clientBannerExchange(conn net.Conn) error {
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetDeadline(time.Time{})

	// Read server banner first
	buf := make([]byte, len(sshBanner))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	// Send client banner
	if _, err := conn.Write([]byte(sshBanner)); err != nil {
		return err
	}
	return nil
}
