package forward

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback: %v", err)
	}
	defer ln.Close()

	ch := make(chan *net.TCPConn, 1)
	errCh := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		ch <- c.(*net.TCPConn)
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	select {
	case s := <-ch:
		return client.(*net.TCPConn), s
	case err := <-errCh:
		t.Fatalf("accept: %v", err)
	}
	return nil, nil
}

// Regression guard: when one side half-closes first (every POST, every HTTP/2
// GOAWAY) the other direction must still deliver its data in full. The old
// implementation returned after the first io.Copy and closed both ends,
// truncating the response.
func TestPipeDeliversResponseAfterClientHalfClose(t *testing.T) {
	// client <-> relayLeft,  relayRight <-> server
	client, relayLeft := tcpPair(t)
	relayRight, server := tcpPair(t)
	defer client.Close()
	defer server.Close()

	go pipe(relayLeft, relayRight)

	payload := bytes.Repeat([]byte("resp"), 20000)

	// Client sends a request, then half-closes: done writing, still reading.
	go func() {
		_, _ = client.Write([]byte("request"))
		_ = client.CloseWrite()
	}()

	// Server sees EOF (proves the FIN propagated through the pipe).
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	req, err := io.ReadAll(server)
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	if string(req) != "request" {
		t.Fatalf("server got %q", req)
	}

	if _, err := server.Write(payload); err != nil {
		t.Fatalf("server write: %v", err)
	}
	_ = server.CloseWrite()

	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("truncated response: got %d of %d bytes", len(got), len(payload))
	}
}

func TestPipeForwardsPlainTraffic(t *testing.T) {
	client, relayLeft := tcpPair(t)
	relayRight, server := tcpPair(t)
	defer client.Close()
	defer server.Close()

	go pipe(relayLeft, relayRight)

	msg := []byte("hello through the pipe")
	if _, err := client.Write(msg); err != nil {
		t.Fatal(err)
	}
	_ = client.CloseWrite()

	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(server)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("got %q want %q", got, msg)
	}
}
