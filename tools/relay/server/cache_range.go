package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// maxMultipartRanges bounds how many intervals one request may ask the relay to
// assemble. RFC 9110 lets a server ignore a Range it considers excessive; the
// limit keeps one request from opening thousands of store reads and writing a
// multipart body far larger than the representation. Repeated intervals count
// individually, because each one costs a part.
const maxMultipartRanges = 32

// byteRange is one satisfiable interval of a stored body.
type byteRange struct {
	start  int64
	length int64
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

// serveRanges answers a Range request from a complete, non-transcoded stored
// body. Evaluation order is If-Range, then parsing, then 206/416.
func (s *Server) serveRanges(w http.ResponseWriter, r *http.Request, v hashstore.Variant) {
	if ifRange := r.Header.Get("If-Range"); ifRange != "" && !ifRangeMatches(ifRange, v.ResponseHeaders) {
		s.serveFullBody(w, r, v)
		return
	}

	size := v.BodySize
	ranges, satisfiable, valid := parseByteRanges(r.Header.Get("Range"), size)
	if !valid {
		s.serveFullBody(w, r, v)
		return
	}
	if !satisfiable {
		h := s.deliveryHeaders(v)
		h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		h.Set("Content-Length", "0")
		h.Set("Accept-Ranges", "bytes")
		copyHeader(w.Header(), h)
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if len(ranges) > maxMultipartRanges {
		// Too many intervals: ignore the Range field and send the complete 200
		// representation instead of assembling an unbounded multipart body.
		log.Printf("relay cache: %q asked for %d ranges (limit %d), ignoring Range", v.URL, len(ranges), maxMultipartRanges)
		s.serveFullBody(w, r, v)
		return
	}

	if len(ranges) == 1 {
		s.serveSingleRange(w, r, v, ranges[0])
		return
	}
	s.serveMultipartRanges(w, r, v, ranges)
}

func (s *Server) serveSingleRange(w http.ResponseWriter, r *http.Request, v hashstore.Variant, rng byteRange) {
	rc, total, err := s.hashStore.Range(r.Context(), hashstore.SHA256, v.BodySHA256, rng.start)
	if err != nil {
		if err == hashstore.ErrNotFound {
			s.dropVariant(v)
		} else {
			log.Printf("relay cache: range %q: %v", v.URL, err)
		}
		// Nothing has been written yet; the ordinary proxy can still answer.
		s.proxy.ServeHTTP(w, r)
		return
	}
	defer rc.Close()
	if total != v.BodySize {
		log.Printf("relay cache: %q stored size %d, body size %d", v.URL, v.BodySize, total)
	}

	h := s.deliveryHeaders(v)
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.start+rng.length-1, v.BodySize))
	h.Set("Content-Length", fmt.Sprint(rng.length))
	h.Set("Accept-Ranges", "bytes")
	copyHeader(w.Header(), h)
	w.WriteHeader(http.StatusPartialContent)
	if _, err := io.CopyN(w, rc, rng.length); err != nil && r.Context().Err() == nil {
		log.Printf("relay cache: stream range %q: %v", v.URL, err)
	}
}

func (s *Server) serveMultipartRanges(w http.ResponseWriter, r *http.Request, v hashstore.Variant, ranges []byteRange) {
	boundary := newBoundary()
	mw := multipart.NewWriter(w)
	_ = mw.SetBoundary(boundary)

	h := s.deliveryHeaders(v)
	h.Set("Content-Type", "multipart/byteranges; boundary="+boundary)
	h.Del("Content-Length")
	h.Set("Accept-Ranges", "bytes")
	copyHeader(w.Header(), h)
	w.WriteHeader(http.StatusPartialContent)

	contentType := strings.TrimSpace(v.ResponseHeaders.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	for _, rng := range ranges {
		partHeader := make(textproto.MIMEHeader)
		partHeader.Set("Content-Type", contentType)
		partHeader.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.start+rng.length-1, v.BodySize))
		part, err := mw.CreatePart(partHeader)
		if err != nil {
			log.Printf("relay cache: multipart %q: %v", v.URL, err)
			return
		}
		rc, _, err := s.hashStore.Range(r.Context(), hashstore.SHA256, v.BodySHA256, rng.start)
		if err != nil {
			log.Printf("relay cache: multipart range %q: %v", v.URL, err)
			return
		}
		_, copyErr := io.CopyN(part, rc, rng.length)
		_ = rc.Close()
		if copyErr != nil {
			log.Printf("relay cache: multipart part %q: %v", v.URL, copyErr)
			return
		}
	}
	if err := mw.Close(); err != nil && r.Context().Err() == nil {
		log.Printf("relay cache: multipart close %q: %v", v.URL, err)
	}
}

func newBoundary() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "mirror-db-relay-boundary"
	}
	return hex.EncodeToString(buf[:])
}

// serveFullBody serves the whole stored representation. It is shared by a
// normal hit and by a Range request whose If-Range or syntax falls back to the
// full response.
func (s *Server) serveFullBody(w http.ResponseWriter, r *http.Request, v hashstore.Variant) bool {
	rc, size, err := s.hashStore.Get(r.Context(), hashstore.SHA256, v.BodySHA256)
	if err != nil {
		if err == hashstore.ErrNotFound {
			s.dropVariant(v)
		} else {
			log.Printf("relay cache: open %q: %v", v.URL, err)
		}
		// No headers have been written yet, so the ordinary proxy can answer.
		s.proxy.ServeHTTP(w, r)
		return true
	}
	defer rc.Close()
	if size != v.BodySize {
		log.Printf("relay cache: %q stored size %d, body size %d", v.URL, v.BodySize, size)
	}

	h := s.deliveryHeaders(v)
	h.Set("Content-Length", fmt.Sprint(v.BodySize))
	h.Set("Accept-Ranges", "bytes")
	copyHeader(w.Header(), h)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil && r.Context().Err() == nil {
		log.Printf("relay cache: stream %q: %v", v.URL, err)
	}
	return true
}
