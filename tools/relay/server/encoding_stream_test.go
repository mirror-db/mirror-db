package server

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
)

var allCodings = []ContentCoding{
	CodingIdentity,
	CodingGzip,
	CodingDeflate,
	CodingBrotli,
	CodingZstd,
}

func readTranscoded(t *testing.T, src io.Reader, from, to ContentCoding) ([]byte, error) {
	t.Helper()
	rc, err := Transcode(src, from, to)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func mustTranscode(t *testing.T, src []byte, from, to ContentCoding) []byte {
	t.Helper()
	got, err := readTranscoded(t, bytes.NewReader(src), from, to)
	if err != nil {
		t.Fatalf("transcode %s -> %s: %v", from, to, err)
	}
	return got
}

func testPayload() []byte {
	// Binary, repetitive and non-repetitive data: every byte value, then a
	// deterministic pattern long enough for each encoder to emit real output.
	payload := make([]byte, 0, 4096)
	for i := 0; i < 16; i++ {
		for b := 0; b < 256; b++ {
			payload = append(payload, byte(b))
		}
	}
	for i := 0; i < 1024; i++ {
		payload = append(payload, byte(i*31+i/7))
	}
	return payload
}

func TestTranscodeRoundTripAndCrossCoding(t *testing.T) {
	payload := testPayload()

	encoded := make(map[ContentCoding][]byte, len(allCodings))
	for _, coding := range allCodings {
		if coding == CodingIdentity {
			encoded[coding] = payload
			continue
		}
		encoded[coding] = mustTranscode(t, payload, CodingIdentity, coding)
		if bytes.Equal(encoded[coding], payload) && coding != CodingIdentity {
			t.Fatalf("%s did not change the bytes", coding)
		}
	}

	for _, from := range allCodings {
		for _, to := range allCodings {
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				got := mustTranscode(t, encoded[from], from, to)
				decoded := mustTranscode(t, got, to, CodingIdentity)
				if !bytes.Equal(decoded, payload) {
					t.Fatalf("round trip through %s -> %s changed the body", from, to)
				}
			})
		}
	}
}

var errSourceBoom = errors.New("source boom")

type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestTranscodePropagatesSourceError(t *testing.T) {
	payload := bytes.Repeat([]byte("streaming data "), 512)
	for _, to := range allCodings {
		t.Run(string(to), func(t *testing.T) {
			_, err := readTranscoded(t, &failingReader{data: payload, err: errSourceBoom}, CodingIdentity, to)
			if !errors.Is(err, errSourceBoom) {
				t.Fatalf("error = %v, want source error", err)
			}
		})
	}
}

func TestTranscodeDecoderErrors(t *testing.T) {
	t.Run("invalid gzip header", func(t *testing.T) {
		if _, err := Transcode(bytes.NewReader([]byte("not gzip")), CodingGzip, CodingIdentity); err == nil {
			t.Fatal("expected an error for an invalid gzip header")
		}
	})

	t.Run("invalid deflate header", func(t *testing.T) {
		if _, err := Transcode(bytes.NewReader([]byte("not zlib")), CodingDeflate, CodingIdentity); err == nil {
			t.Fatal("expected an error for an invalid zlib header")
		}
	})

	for _, coding := range []ContentCoding{CodingGzip, CodingDeflate, CodingBrotli, CodingZstd} {
		t.Run("truncated "+string(coding)+" body", func(t *testing.T) {
			compressed := mustTranscode(t, testPayload(), CodingIdentity, coding)
			truncated := compressed[:len(compressed)/2]
			_, err := readTranscoded(t, bytes.NewReader(truncated), coding, CodingIdentity)
			if err == nil {
				t.Fatalf("expected an error for a truncated %s body", coding)
			}
		})
	}
}

func TestTranscodeRejectsOversizedZstdWindow(t *testing.T) {
	var compressed bytes.Buffer
	enc, err := zstd.NewWriter(&compressed,
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(16<<20),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(bytes.Repeat([]byte("mirror"), 1<<18)); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = readTranscoded(t, bytes.NewReader(compressed.Bytes()), CodingZstd, CodingIdentity)
	if err == nil {
		t.Fatal("expected a zstd frame with a 16 MiB window to be rejected")
	}
}

func TestTranscodeRejectsUnsupportedCoding(t *testing.T) {
	if _, err := Transcode(bytes.NewReader(nil), ContentCoding("lzma"), CodingGzip); err == nil {
		t.Fatal("unsupported source coding accepted")
	}
	if _, err := Transcode(bytes.NewReader(nil), CodingIdentity, ContentCoding("lzma")); err == nil {
		t.Fatal("unsupported target coding accepted")
	}
	if _, err := Transcode(nil, CodingIdentity, CodingGzip); err == nil {
		t.Fatal("nil source accepted")
	}
}

type trackingReadCloser struct {
	*bytes.Reader
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestTranscodeSameCodingKeepsReadCloser(t *testing.T) {
	src := &trackingReadCloser{Reader: bytes.NewReader([]byte("unchanged"))}
	rc, err := Transcode(src, CodingGzip, ContentCoding("GZIP"))
	if err != nil {
		t.Fatal(err)
	}
	if rc != src {
		t.Fatal("same-coding transcode should return the source reader")
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if !src.closed {
		t.Fatal("closing the returned reader should close the source")
	}
}

func TestTranscodeCloseEarly(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1<<20)
	rc, err := Transcode(bytes.NewReader(payload), CodingIdentity, CodingZstd)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatalf("read prefix: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := rc.Read(buf); err == nil {
		t.Fatal("Read after Close should fail")
	}
}
