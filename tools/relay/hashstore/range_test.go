package hashstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
)

func readRange(t *testing.T, s *Store, algo Algorithm, digest string, offset int64) (string, int64) {
	t.Helper()
	rc, total, err := s.Range(context.Background(), algo, digest, offset)
	if err != nil {
		t.Fatalf("range %s/%s at %d: %v", algo, digest, offset, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read range: %v", err)
	}
	if int64(len(data)) != total-offset {
		t.Fatalf("read %d bytes at %d, total %d", len(data), offset, total)
	}
	return string(data), total
}

func TestLocalRangeBoundaries(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 1 << 20})
	const body = "0123456789"
	c := putBody(t, s, body)

	for _, tc := range []struct {
		offset int64
		want   string
	}{
		{0, body},
		{3, "3456789"},
		{9, "9"},
		{10, ""},
	} {
		got, total := readRange(t, s, SHA256, c.SHA256, tc.offset)
		if got != tc.want {
			t.Errorf("offset %d = %q, want %q", tc.offset, got, tc.want)
		}
		if total != int64(len(body)) {
			t.Errorf("offset %d total = %d, want %d", tc.offset, total, len(body))
		}
	}
	// The SHA-1 address is a valid range address too.
	if got, _ := readRange(t, s, SHA1, c.SHA1, 4); got != "456789" {
		t.Fatalf("sha1 range = %q", got)
	}

	for _, offset := range []int64{-1, 11, 1 << 20} {
		if _, _, err := s.Range(context.Background(), SHA256, c.SHA256, offset); !errors.Is(err, ErrInvalidRange) {
			t.Errorf("offset %d: got %v, want ErrInvalidRange", offset, err)
		}
	}
	if _, _, err := s.Range(context.Background(), SHA256, strings.Repeat("0", 64), 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing body: got %v, want ErrNotFound", err)
	}

	empty := putBody(t, s, "")
	if got, total := readRange(t, s, SHA256, empty.SHA256, 0); got != "" || total != 0 {
		t.Fatalf("empty body range = (%q,%d)", got, total)
	}
}

func TestLocalRangeUnreadableBodyKeepsIndex(t *testing.T) {
	dir := t.TempDir()
	s := openLocal(t, Config{Dir: dir, Capacity: 1 << 20})
	c := putBody(t, s, "keep my index")

	be := newLocalBackend(filepath.Join(dir, dataDirName))
	p, err := be.path(c.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o750); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Range(context.Background(), SHA256, c.SHA256, 1); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("range: got %v, want a non-miss error", err)
	}
	if st := s.Stats(); st.Objects != 1 {
		t.Fatalf("range dropped the index entry: %+v", st)
	}
}

func TestSharedRangeBoundaries(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	const body = "0123456789"
	c := putBody(t, s, body)

	for _, tc := range []struct {
		offset int64
		want   string
	}{
		{0, body},
		{3, "3456789"},
		{10, ""},
	} {
		got, total := readRange(t, s, SHA256, c.SHA256, tc.offset)
		if got != tc.want {
			t.Errorf("offset %d = %q, want %q", tc.offset, got, tc.want)
		}
		if total != int64(len(body)) {
			t.Errorf("offset %d total = %d, want %d", tc.offset, total, len(body))
		}
	}

	for _, offset := range []int64{-1, 11} {
		if _, _, err := s.Range(context.Background(), SHA256, c.SHA256, offset); !errors.Is(err, ErrInvalidRange) {
			t.Errorf("offset %d: got %v, want ErrInvalidRange", offset, err)
		}
	}
	if _, _, err := s.Range(context.Background(), SHA1, c.SHA1, 11); !errors.Is(err, ErrInvalidRange) {
		t.Errorf("sha1 beyond end: got %v", err)
	}
	if _, _, err := s.Range(context.Background(), SHA256, strings.Repeat("0", 64), 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing body: got %v, want ErrNotFound", err)
	}

	empty := putBody(t, s, "")
	if got, total := readRange(t, s, SHA256, empty.SHA256, 0); got != "" || total != 0 {
		t.Fatalf("empty body range = (%q,%d)", got, total)
	}
}

// A 206 without Content-Range cannot prove the reader starts at the requested
// offset. Serving it would silently return the wrong bytes.
func TestSharedRangeRefusesAmbiguousSuffix(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	c := putBody(t, s, "0123456789")

	fake.setOmitContentRange(true)
	if _, _, err := s.Range(context.Background(), SHA256, c.SHA256, 3); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("ranged GET without Content-Range: got %v, want a real error", err)
	}
	// Offset zero does not rely on Content-Range: the whole body is the answer.
	if got, total := readRange(t, s, SHA256, c.SHA256, 0); got != "0123456789" || total != 10 {
		t.Fatalf("full range = (%q,%d)", got, total)
	}
}

// httpStatusError models a smithy-shaped response whose only reliable signal is
// the HTTP status: R2-style servers may return an empty or unrelated code.
type httpStatusError struct {
	code   string
	status int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("s3: %s (status %d)", e.code, e.status)
}

func (e *httpStatusError) ErrorCode() string             { return e.code }
func (e *httpStatusError) ErrorMessage() string          { return e.Error() }
func (e *httpStatusError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }
func (e *httpStatusError) HTTPStatusCode() int           { return e.status }

// A 416 must be recognised from the status even when the error code is empty or
// unrelated; a non-416 status with an empty code stays a real error.
func TestSharedRangeAcceptsStatusOnly416(t *testing.T) {
	fake := newFakeS3()
	s := openShared(t, t.TempDir(), fake, 1<<20, 0)
	c := putBody(t, s, "0123456789")

	for _, code := range []string{"", "SomethingElse"} {
		fake.setRangeErr(&httpStatusError{code: code, status: 416})
		if got, total := readRange(t, s, SHA256, c.SHA256, 10); got != "" || total != 10 {
			t.Fatalf("code %q: EOF range = (%q,%d), want empty at 10", code, got, total)
		}
		if _, _, err := s.Range(context.Background(), SHA256, c.SHA256, 11); !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("code %q: past EOF: got %v, want ErrInvalidRange", code, err)
		}
	}

	fake.setRangeErr(&httpStatusError{code: "", status: 500})
	if _, _, err := s.Range(context.Background(), SHA256, c.SHA256, 10); err == nil || errors.Is(err, ErrInvalidRange) || errors.Is(err, ErrNotFound) {
		t.Fatalf("status 500: got %v, want a real error", err)
	}
}

// A Range hit promotes recency exactly like Get; Stat does not.
func TestRangeTouchesLRUAndStatDoesNot(t *testing.T) {
	s := openLocal(t, Config{Dir: t.TempDir(), Capacity: 100, MaxSize: 100})
	a := putBody(t, s, strings.Repeat("a", 40))
	b := putBody(t, s, strings.Repeat("b", 40))

	if got, _ := readRange(t, s, SHA256, a.SHA256, 10); got != strings.Repeat("a", 30) {
		t.Fatalf("A suffix = %q", got)
	}
	putBody(t, s, strings.Repeat("c", 40))
	if _, _, err := s.Get(context.Background(), SHA256, b.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B should be the LRU tail, got %v", err)
	}
	if got := readContent(t, s, SHA256, a.SHA256); got != strings.Repeat("a", 40) {
		t.Fatalf("A body = %q", got)
	}

	// A fresh store: Stat must not move A, so the next PUT evicts A.
	statStore := openLocal(t, Config{Dir: t.TempDir(), Capacity: 100, MaxSize: 100})
	a2 := putBody(t, statStore, strings.Repeat("a", 40))
	putBody(t, statStore, strings.Repeat("b", 40))
	if _, ok, err := statStore.Stat(context.Background(), SHA256, a2.SHA256); err != nil || !ok {
		t.Fatalf("stat A: (%v,%v)", ok, err)
	}
	putBody(t, statStore, strings.Repeat("c", 40))
	if _, _, err := statStore.Get(context.Background(), SHA256, a2.SHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat promoted A; got %v, want ErrNotFound", err)
	}
}
