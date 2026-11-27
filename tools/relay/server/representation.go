package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Representation delivery.
//
// Both cache tiers hand an already-selected representation to this module and
// forget how it was obtained. A stored variant and an active assembly differ
// only in where their bytes come from; everything after that point is HTTP
// representation semantics and must be one implementation:
//
//   - conditional requests (304 / 412) against the delivered representation;
//   - If-Range, then the Range grammar, then the full / single / multipart /
//     416 decision;
//   - the headers, status and framing of every shape, including the Age of the
//     represented bytes.
//
// The module never retries after response headers are written. A source failure
// before that point is returned in deliveryResult.Err with Delivered false, so
// the caller can apply its own recovery (an ordinary repair or proxy fallback,
// an active entry failure). A failure after the headers is classified into a
// read error (the bytes are broken) and a client error (the client went away),
// which is the one distinction the active tier needs to decide whether a shared
// assembly must be failed.

// maxMultipartRanges bounds how many intervals one request may ask the relay to
// assemble. RFC 9110 lets a server ignore a Range it considers excessive; the
// limit keeps one request from opening thousands of source reads and writing a
// multipart body far larger than the representation. Repeated intervals count
// individually, because each one costs a part.
const maxMultipartRanges = 32

// errRangeUnsatisfiable reports that a source cannot open the requested
// interval because the extent is smaller than the representation metadata
// claimed, which is a 416 rather than a fallback.
var errRangeUnsatisfiable = errors.New("representation: range outside the extent")

// byteRange is one satisfiable interval of a representation.
type byteRange struct {
	start  int64
	length int64
}

// representation is one selected response representation: the validators a
// conditional request reads, the headers a delivery writes, the complete
// extent, and a source for the bytes.
type representation struct {
	// validators is the representation actually delivered. A transcoded
	// delivery carries its weak validators here, never a stored copy's strong
	// ones.
	validators http.Header
	// header is the delivered header set, with Age already computed and the
	// shape-specific headers removed.
	header http.Header
	// size is the complete extent, or -1 when the source cannot state one.
	size int64
	// acceptRanges states whether the delivered representation can answer
	// byte ranges. A transcoded stream cannot.
	acceptRanges bool
	// trackCopyErrors requests per-write attribution of a copy failure. It is
	// set by the active tier, whose reader failures must fail a shared entry;
	// without it a full-body copy keeps the ResponseWriter's ReaderFrom
	// fast path (sendfile) and reports an unclassified copy error.
	trackCopyErrors bool
	// source acquires the bytes.
	source representationSource
	// deferBody means the caller writes the body (ordinary transcoding). This
	// module still answers conditions, HEAD, 304 and 412.
	deferBody bool
	// label names the representation in diagnostics.
	label string
}

// representationSource acquires the bytes of one representation. probe never
// reads a body; open returns the complete representation; openSection returns
// exactly length bytes at offset, or errRangeUnsatisfiable when the extent does
// not contain that interval.
type representationSource interface {
	probe(ctx context.Context) error
	open(ctx context.Context) (io.ReadCloser, error)
	openSection(ctx context.Context, offset, length int64) (io.ReadCloser, error)
}

// deliveryResult reports what one delivery wrote.
type deliveryResult struct {
	// Delivered is true when the response is finished: a 304, 412 or HEAD
	// answer, or a body response whose headers were written.
	Delivered bool
	// Err is a source failure before any header was written. The caller must
	// recover (repair, proxy, fail the entry) and nothing has reached the
	// client.
	Err error
	// BodyComplete is true when every byte the response promised reached the
	// client.
	BodyComplete bool
	// WholeRepresentation is true when the delivered bytes cover the complete
	// representation: a full body, or a range set whose union is [0,size).
	WholeRepresentation bool
	// Multipart is true when the response was a multipart/byteranges body.
	Multipart bool
	// ReadErr is a source failure after the headers were written: the bytes
	// are broken.
	ReadErr error
	// ClientErr is a failure writing to the client: it says nothing about the
	// source.
	ClientErr error
}

// deliverRepresentation answers r from rep. It never fetches and never retries
// after the headers; callers own the bytes behind rep.source.
func deliverRepresentation(w http.ResponseWriter, r *http.Request, rep representation) deliveryResult {
	switch evaluatePreconditions(r, rep.validators) {
	case preconditionNotModified:
		h := representationHeaderCopy(rep.header)
		h.Del("Content-Type")
		copyHeader(w.Header(), h)
		w.WriteHeader(http.StatusNotModified)
		return deliveryResult{Delivered: true}
	case preconditionFailed:
		w.WriteHeader(http.StatusPreconditionFailed)
		return deliveryResult{Delivered: true}
	}

	if r.Method == http.MethodHead {
		if rep.source != nil {
			if err := rep.source.probe(r.Context()); err != nil {
				return deliveryResult{Err: err}
			}
		}
		h := representationHeaderCopy(rep.header)
		// A transcoded representation has no stored length: its size only
		// describes the bytes behind the coding.
		if rep.size >= 0 && !rep.deferBody {
			h.Set("Content-Length", strconv.FormatInt(rep.size, 10))
		}
		if rep.acceptRanges {
			h.Set("Accept-Ranges", "bytes")
		}
		copyHeader(w.Header(), h)
		w.WriteHeader(http.StatusOK)
		return deliveryResult{Delivered: true}
	}

	if rep.deferBody {
		return deliveryResult{}
	}

	rangeHeader := strings.TrimSpace(r.Header.Get("Range"))
	if rangeHeader == "" {
		return deliverFullRepresentation(w, r, rep)
	}
	// A Range is answered from the representation's extent, so the extent must
	// exist before any 206/416 header is written. A definite absence becomes a
	// caller-owned repair; a transient check failure stays a fallback and is
	// never mistaken for a missing entry. The active source's probe is a no-op.
	if err := rep.source.probe(r.Context()); err != nil {
		return deliveryResult{Err: err}
	}
	if value := r.Header.Get("If-Range"); value != "" && !ifRangeMatches(value, rep.validators) {
		// The validator does not describe this representation: ignore Range
		// and send the complete body.
		return deliverFullRepresentation(w, r, rep)
	}
	if rep.size < 0 {
		// A Range cannot be evaluated without a total extent. The caller's
		// ordinary chain forwards the request instead.
		return deliveryResult{}
	}

	ranges, satisfiable, valid := parseByteRanges(rangeHeader, rep.size)
	if !valid {
		return deliverFullRepresentation(w, r, rep)
	}
	if !satisfiable {
		return deliverUnsatisfiable(w, rep)
	}
	if len(ranges) > maxMultipartRanges {
		// Too many intervals: ignore the Range field and send the complete 200
		// representation instead of assembling an unbounded multipart body.
		log.Printf("relay representation: %q asked for %d ranges (limit %d), ignoring Range", rep.label, len(ranges), maxMultipartRanges)
		return deliverFullRepresentation(w, r, rep)
	}
	if len(ranges) == 1 {
		return deliverSingleRange(w, r, rep, ranges[0])
	}
	return deliverMultipartRanges(w, r, rep, ranges)
}

// deliverFullRepresentation writes the complete representation.
func deliverFullRepresentation(w http.ResponseWriter, r *http.Request, rep representation) deliveryResult {
	rc, err := rep.source.open(r.Context())
	if err != nil {
		return deliveryResult{Err: err}
	}
	defer rc.Close()

	h := representationHeaderCopy(rep.header)
	if rep.size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(rep.size, 10))
	}
	if rep.acceptRanges {
		h.Set("Accept-Ranges", "bytes")
	}
	copyHeader(w.Header(), h)
	w.WriteHeader(http.StatusOK)

	res := deliveryResult{Delivered: true}
	if rep.trackCopyErrors {
		tracker := &deliveryWriter{w: w}
		written, copyErr := io.Copy(tracker, rc)
		if copyErr != nil {
			res.classifyCopyError(copyErr, tracker)
			return res
		}
		res.BodyComplete = rep.size < 0 || written == rep.size
		res.WholeRepresentation = res.BodyComplete
		return res
	}
	// No attribution is needed, so the writer keeps its ReaderFrom fast path
	// (net/http can sendfile a stored body). The error is reported unclassified.
	written, copyErr := copyFullUnattributed(w, rc)
	if copyErr != nil {
		res.ClientErr = copyErr
		return res
	}
	res.BodyComplete = rep.size < 0 || written == rep.size
	res.WholeRepresentation = res.BodyComplete
	return res
}

// copyFullUnattributed copies a complete representation without wrapping the
// destination, so an io.ReaderFrom implementation (sendfile) is used when the
// ResponseWriter provides one.
func copyFullUnattributed(w io.Writer, src io.Reader) (int64, error) {
	if rf, ok := w.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(w, src)
}

// deliverSingleRange writes one satisfiable interval.
func deliverSingleRange(w http.ResponseWriter, r *http.Request, rep representation, rng byteRange) deliveryResult {
	rc, err := rep.source.openSection(r.Context(), rng.start, rng.length)
	if err != nil {
		if errors.Is(err, errRangeUnsatisfiable) {
			return deliverUnsatisfiable(w, rep)
		}
		return deliveryResult{Err: err}
	}
	defer rc.Close()

	h := representationHeaderCopy(rep.header)
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.start+rng.length-1, rep.size))
	h.Set("Content-Length", strconv.FormatInt(rng.length, 10))
	if rep.acceptRanges {
		h.Set("Accept-Ranges", "bytes")
	}
	copyHeader(w.Header(), h)
	w.WriteHeader(http.StatusPartialContent)

	res := deliveryResult{Delivered: true}
	var (
		written int64
		copyErr error
	)
	if rep.trackCopyErrors {
		tracker := &deliveryWriter{w: w}
		written, copyErr = io.CopyN(tracker, rc, rng.length)
		if copyErr != nil {
			res.classifyCopyError(copyErr, tracker)
			return res
		}
	} else {
		// io.CopyN keeps the ResponseWriter's ReaderFrom fast path: it copies
		// through a LimitedReader, which net/http can still sendfile.
		written, copyErr = io.CopyN(w, rc, rng.length)
		if copyErr != nil {
			res.ClientErr = copyErr
			return res
		}
	}
	res.BodyComplete = written == rng.length
	res.WholeRepresentation = rangesCoverAll([]byteRange{rng}, rep.size)
	return res
}

// deliverMultipartRanges writes a multipart/byteranges body. Parts are opened
// one at a time; a failure ends the body mid-frame, exactly like a source that
// stops mid-transfer.
func deliverMultipartRanges(w http.ResponseWriter, r *http.Request, rep representation, ranges []byteRange) deliveryResult {
	boundary := newBoundary()
	mw := multipart.NewWriter(w)
	_ = mw.SetBoundary(boundary)

	h := representationHeaderCopy(rep.header)
	h.Set("Content-Type", "multipart/byteranges; boundary="+boundary)
	h.Del("Content-Length")
	if rep.acceptRanges {
		h.Set("Accept-Ranges", "bytes")
	}
	copyHeader(w.Header(), h)
	w.WriteHeader(http.StatusPartialContent)

	contentType := strings.TrimSpace(rep.validators.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	res := deliveryResult{Delivered: true, Multipart: true}
	for _, rng := range ranges {
		partHeader := make(textproto.MIMEHeader)
		partHeader.Set("Content-Type", contentType)
		partHeader.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.start+rng.length-1, rep.size))
		part, err := mw.CreatePart(partHeader)
		if err != nil {
			res.ClientErr = err
			log.Printf("relay representation: multipart part %q: %v", rep.label, err)
			return res
		}
		rc, err := rep.source.openSection(r.Context(), rng.start, rng.length)
		if err != nil {
			res.ReadErr = err
			log.Printf("relay representation: multipart range %q: %v", rep.label, err)
			return res
		}
		tracker := &deliveryWriter{w: part}
		_, copyErr := io.CopyN(tracker, rc, rng.length)
		_ = rc.Close()
		if copyErr != nil {
			res.classifyCopyError(copyErr, tracker)
			log.Printf("relay representation: multipart part %q: %v", rep.label, copyErr)
			return res
		}
	}
	if err := mw.Close(); err != nil {
		res.ClientErr = err
		if r.Context().Err() == nil {
			log.Printf("relay representation: multipart close %q: %v", rep.label, err)
		}
		return res
	}
	res.BodyComplete = true
	res.WholeRepresentation = rangesCoverAll(ranges, rep.size)
	return res
}

// deliverUnsatisfiable writes the 416 for a parsed Range that addresses no
// existing bytes.
func deliverUnsatisfiable(w http.ResponseWriter, rep representation) deliveryResult {
	h := representationHeaderCopy(rep.header)
	h.Set("Content-Range", fmt.Sprintf("bytes */%d", rep.size))
	h.Set("Content-Length", "0")
	if rep.acceptRanges {
		h.Set("Accept-Ranges", "bytes")
	}
	copyHeader(w.Header(), h)
	w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	return deliveryResult{Delivered: true}
}

// deliveryWriter remembers why a write failed, so a copy error can be
// attributed to the client rather than to the bytes.
type deliveryWriter struct {
	w   io.Writer
	err error
}

func (w *deliveryWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if err != nil && w.err == nil {
		w.err = err
	}
	return n, err
}

func (r *deliveryResult) classifyCopyError(err error, tracker *deliveryWriter) {
	if tracker.err != nil {
		r.ClientErr = err
		return
	}
	r.ReadErr = err
}

func representationHeaderCopy(h http.Header) http.Header {
	out := h.Clone()
	if out == nil {
		out = make(http.Header)
	}
	return out
}

// representationHeaders is the header set one delivery writes: the stored
// representation headers, with the response-specific fields removed and Age
// recomputed for these bytes.
func representationHeaders(stored http.Header, storedAt, now time.Time) http.Header {
	h := representationHeaderCopy(stored)
	h.Del("Content-Length")
	h.Del("Transfer-Encoding")
	h.Del("Content-Range")
	h.Del("Age")
	h.Del(RelayDirectiveHeader)
	h.Set("Age", fmt.Sprint(ageSeconds(storedAt, now)))
	return h
}

// parseByteRanges parses a Range field against a representation size. valid is
// false when the field is syntactically unusable (the caller then ignores it
// and serves the full body); satisfiable is false when the field parsed but no
// interval addresses existing bytes (the caller answers 416).
func parseByteRanges(value string, size int64) (ranges []byteRange, satisfiable, valid bool) {
	value = strings.TrimSpace(value)
	unit, spec, ok := strings.Cut(value, "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return nil, false, false
	}
	if size < 0 {
		size = 0
	}

	parts := strings.Split(spec, ",")
	sawValid := false
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		sawValid = true

		if strings.HasPrefix(part, "-") {
			suffix, err := strconv.ParseInt(strings.TrimSpace(part[1:]), 10, 64)
			if err != nil || suffix < 0 {
				return nil, false, false
			}
			if suffix == 0 || size == 0 {
				continue
			}
			start := size - suffix
			if start < 0 {
				start = 0
			}
			ranges = append(ranges, byteRange{start: start, length: size - start})
			continue
		}

		startText, endText, hasEnd := strings.Cut(part, "-")
		start, err := strconv.ParseInt(strings.TrimSpace(startText), 10, 64)
		if err != nil || start < 0 {
			return nil, false, false
		}
		end := size - 1
		if hasEnd && strings.TrimSpace(endText) != "" {
			end, err = strconv.ParseInt(strings.TrimSpace(endText), 10, 64)
			if err != nil || end < 0 {
				return nil, false, false
			}
		}
		if start >= size || end < start {
			continue
		}
		if end > size-1 {
			end = size - 1
		}
		ranges = append(ranges, byteRange{start: start, length: end - start + 1})
	}

	if !sawValid {
		return nil, false, false
	}
	if len(ranges) == 0 {
		return nil, false, true
	}
	return ranges, true, true
}

func newBoundary() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "mirror-db-relay-boundary"
	}
	return hex.EncodeToString(buf[:])
}

// rangesCoverAll reports whether the union of ranges covers [0,size). A single
// full range and a multipart set that happens to tile the object both publish
// the complete body, so both may wait for the handoff and write a variant.
func rangesCoverAll(ranges []byteRange, size int64) bool {
	if size <= 0 || len(ranges) == 0 {
		return false
	}
	sorted := append([]byteRange(nil), ranges...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start < sorted[j].start })
	var covered int64
	for _, rng := range sorted {
		if rng.start > covered {
			return false
		}
		end := rng.start + rng.length
		if end > covered {
			covered = end
		}
		if covered >= size {
			return true
		}
	}
	return covered >= size
}
