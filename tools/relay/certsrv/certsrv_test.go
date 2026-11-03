package certsrv

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
)

// memStorage is an in-memory certmagic.Storage for tests.
type memStorage struct {
	mu      sync.Mutex
	files   map[string][]byte
	listErr error
}

var _ certmagic.Storage = (*memStorage)(nil)

func newMem(files map[string]string) *memStorage {
	m := &memStorage{files: map[string][]byte{}}
	for k, v := range files {
		m.files[k] = []byte(v)
	}
	return m
}

func (m *memStorage) Store(_ context.Context, key string, v []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[key] = v
	return nil
}

func (m *memStorage) Load(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.files[key]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return v, nil
}

func (m *memStorage) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, key)
	return nil
}

func (m *memStorage) Exists(_ context.Context, key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.files[key]
	return ok
}

func (m *memStorage) List(_ context.Context, _ string, _ bool) ([]string, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.files))
	for k := range m.files {
		keys = append(keys, k)
	}
	return keys, nil
}

func (m *memStorage) Stat(_ context.Context, key string) (certmagic.KeyInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.files[key]
	if !ok {
		return certmagic.KeyInfo{}, fs.ErrNotExist
	}
	return certmagic.KeyInfo{Key: key, Modified: time.Now(), Size: int64(len(v)), IsTerminal: true}, nil
}

func (m *memStorage) Lock(context.Context, string) error   { return nil }
func (m *memStorage) Unlock(context.Context, string) error { return nil }

func packNames(t *testing.T, body io.Reader) (map[string]string, error) {
	t.Helper()
	got := map[string]string{}
	tr := tar.NewReader(body)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return got, nil
		}
		if err != nil {
			return nil, err
		}
		b, _ := io.ReadAll(tr)
		got[hdr.Name] = string(b)
	}
}

func TestServeStreamsPack(t *testing.T) {
	srv := New(newMem(map[string]string{
		"certificates/a.com/a.com.crt": "cert",
		"certificates/a.com/a.com.key": "key",
		"acme/acme_account+key":        "acct",
	}))

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-tar" {
		t.Errorf("content-type %q", ct)
	}

	got, err := packNames(t, bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("tar: %v", err)
	}

	for _, want := range []string{
		"certmagic/certificates/a.com/a.com.crt",
		"certmagic/certificates/a.com/a.com.key",
		"certmagic/acme/acme_account+key",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q in pack (have %v)", want, got)
		}
	}
}

func TestServeSkipsLockFiles(t *testing.T) {
	srv := New(newMem(map[string]string{
		"certificates/a.com/a.com.crt": "cert",
		"locks/certificates/a.com":     "locked",
		"certificates/a.com.lock":      "locked",
	}))

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	got, err := packNames(t, bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for name := range got {
		if IsLockFile(name) {
			t.Fatalf("lock file leaked into pack: %s", name)
		}
	}
	if _, ok := got["certmagic/certificates/a.com/a.com.crt"]; !ok {
		t.Fatal("real cert missing from pack")
	}
}

func TestServeErrorsBeforeHeaders(t *testing.T) {
	st := newMem(nil)
	st.listErr = fs.ErrPermission

	w := httptest.NewRecorder()
	New(st).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", w.Code)
	}
}
