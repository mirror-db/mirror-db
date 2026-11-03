package server

import (
	"archive/tar"
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/mirror-db/mirror-db/tools/relay/certclient"
	"github.com/mirror-db/mirror-db/tools/relay/guard"
)

func packTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A parent that is restarting when its consumer starts is a one-second
// coincidence, and it used to be fatal. The fetch now retries before giving up.
func TestFetchPackRetriesTransientFailure(t *testing.T) {
	pack := packTar(t, map[string]string{"certmagic/certificates/a.com/a.com.crt": "x"})
	var hits atomic.Int32

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := (&guard.Guard{SecretKey: []byte("s3cret")}).Check(r); err != nil {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		if hits.Add(1) == 1 {
			http.Error(w, "parent is restarting", http.StatusServiceUnavailable)
			return
		}
		w.Write(pack)
	}))
	defer srv.Close()

	c := &certclient.CertClient{
		CertPackURL: srv.URL,
		SecretKey:   []byte("s3cret"),
		Transport:   srv.Client().Transport,
	}

	if err := fetchPackWithRetry(c); err != nil {
		t.Fatalf("fetchPackWithRetry: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("%d requests, want the failed one plus a retry", got)
	}
}
