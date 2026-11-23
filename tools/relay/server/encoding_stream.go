package server

import (
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// zstdMaxWindow caps the back-reference window a transcoded zstd stream may
// use. 8 MiB is enough for ordinary registry artifacts and keeps one
// concurrent transcode from allocating an arbitrary decoder window.
const zstdMaxWindow = 8 << 20

// Transcode returns a reader that delivers src's body re-encoded from one
// content coding to another. Every conversion decodes to identity and then
// encodes the target on the fly: bytes are never collected into one buffer, and
// an encoding-to-encoding shortcut is never attempted.
//
// The returned ReadCloser must be closed by the caller. Closing early unblocks
// the internal writer; a source error surfaces from Read unchanged.
//
// A HEAD delivery never calls Transcode. It uses ApplyTranscodedResponseHeaders
// alone, so it adjusts headers without producing a single byte.
func Transcode(src io.Reader, from, to ContentCoding) (io.ReadCloser, error) {
	if src == nil {
		return nil, fmt.Errorf("transcode: nil source")
	}

	fromCoding, err := normalizeCoding(from)
	if err != nil {
		return nil, fmt.Errorf("transcode from: %w", err)
	}
	toCoding, err := normalizeCoding(to)
	if err != nil {
		return nil, fmt.Errorf("transcode to: %w", err)
	}

	if fromCoding == toCoding {
		return ensureReadCloser(src), nil
	}

	decoder, closeDecoder, err := newDecoder(src, fromCoding)
	if err != nil {
		return nil, fmt.Errorf("transcode from %s: %w", fromCoding, err)
	}

	pr, pw := io.Pipe()
	encoder, closeEncoder, err := newEncoder(pw, toCoding)
	if err != nil {
		closeDecoder()
		_ = pr.Close()
		return nil, fmt.Errorf("transcode to %s: %w", toCoding, err)
	}

	go func() {
		_, copyErr := io.Copy(encoder, decoder)
		flushErr := closeEncoder()
		closeDecoder()

		switch {
		case copyErr != nil:
			_ = pw.CloseWithError(copyErr)
		case flushErr != nil:
			_ = pw.CloseWithError(flushErr)
		default:
			_ = pw.Close()
		}
	}()

	return &transcodedReadCloser{reader: pr}, nil
}

type transcodedReadCloser struct {
	reader *io.PipeReader
}

func (t *transcodedReadCloser) Read(p []byte) (int, error) {
	return t.reader.Read(p)
}

func (t *transcodedReadCloser) Close() error {
	return t.reader.Close()
}

func ensureReadCloser(r io.Reader) io.ReadCloser {
	if rc, ok := r.(io.ReadCloser); ok {
		return rc
	}
	return io.NopCloser(r)
}

func normalizeCoding(c ContentCoding) (ContentCoding, error) {
	if c == "" {
		return CodingIdentity, nil
	}
	parsed, ok := ParseContentCoding(string(c))
	if !ok {
		return "", fmt.Errorf("unsupported content coding %q", c)
	}
	return parsed, nil
}

func newDecoder(src io.Reader, coding ContentCoding) (io.Reader, func(), error) {
	switch coding {
	case CodingIdentity:
		return src, func() {}, nil
	case CodingGzip:
		r, err := gzip.NewReader(src)
		if err != nil {
			return nil, nil, err
		}
		return r, func() { _ = r.Close() }, nil
	case CodingDeflate:
		r, err := zlib.NewReader(src)
		if err != nil {
			return nil, nil, err
		}
		return r, func() { _ = r.Close() }, nil
	case CodingBrotli:
		return brotli.NewReader(src), func() {}, nil
	case CodingZstd:
		decoder, err := zstd.NewReader(src,
			// Keep a single transcoded stream bounded: one synchronous
			// decoder, low-memory mode, and a window no larger than 8 MiB. A
			// valid upstream frame that needs a larger window fails on read
			// and aborts the transcode; that is the intended safety boundary.
			zstd.WithDecoderMaxWindow(zstdMaxWindow),
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderLowmem(true),
		)
		if err != nil {
			return nil, nil, err
		}
		rc := decoder.IOReadCloser()
		return rc, func() { _ = rc.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("unsupported content coding %q", coding)
	}
}

func newEncoder(dst io.Writer, coding ContentCoding) (io.Writer, func() error, error) {
	switch coding {
	case CodingIdentity:
		return dst, func() error { return nil }, nil
	case CodingGzip:
		w := gzip.NewWriter(dst)
		return w, w.Close, nil
	case CodingDeflate:
		w := zlib.NewWriter(dst)
		return w, w.Close, nil
	case CodingBrotli:
		w := brotli.NewWriter(dst)
		return w, w.Close, nil
	case CodingZstd:
		w, err := zstd.NewWriter(dst, zstd.WithEncoderConcurrency(1))
		if err != nil {
			return nil, nil, err
		}
		return w, w.Close, nil
	default:
		return nil, nil, fmt.Errorf("unsupported content coding %q", coding)
	}
}
