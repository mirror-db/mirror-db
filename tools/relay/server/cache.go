package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// Cache chain (M2).
//
// A request that resolves to a manifest mirror and uses GET or HEAD goes
// through this chain when a content store is configured. It is deliberately a
// hand-written exchange rather than the ReverseProxy: a hit must serve without
// touching the upstream, and a miss must tee the upstream body into the store
// while the client receives the same bytes.
//
// The cache chain never retries after response headers have been sent. Every
// pre-response failure either falls back to the ordinary proxy (returning false
// from serveCache) or answers 502, while a failure during body transfer simply
// ends the stream.

const (
	// variantCommitTimeout bounds the metadata write that follows a complete
	// body. It runs without the request context so a client that disconnected
	// after its last byte cannot lose an already-finished body.
	variantCommitTimeout = 30 * time.Second

	// variantCleanupTimeout bounds best-effort metadata deletion for a definite
	// cache miss.
	variantCleanupTimeout = 10 * time.Second
)

var errClientAborted = errors.New("cache: client aborted the response")

// serveCache attempts to answer a request from the local store. It returns
// false when the request must take the ordinary proxy path instead: no store,
// not GET/HEAD, not resolved to a mirror, a cache lookup failure, or a
// cache-specific reason to re-fetch (an unacceptable coding, or a range that
// would need transcoding).
func (s *Server) serveCache(w http.ResponseWriter, r *http.Request) bool {
	if s.hashStore == nil {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}

	st := s.manifest.Load()
	mirror, path, ok := s.resolveMirror(st, r)
	if !ok {
		return false
	}
	if isRelaytestPath(path) {
		return false
	}

	key := cacheKey(mirror, path, r.URL)

	requestPolicy := requestCachePolicyFrom(r)
	if requestPolicy.noStore {
		// The client asked that this exchange never touch the cache: no
		// lookup, and no write even if the response would otherwise qualify.
		s.serveMiss(w, r, key, path, nil, false, false)
		return true
	}

	variants, err := s.hashStore.Variants(r.Context(), key)
	if err != nil {
		log.Printf("relay cache: list %q: %v", key, err)
		return false
	}

	matches := matchingVariants(variants, r.Header)
	switch len(matches) {
	case 0:
		s.serveMiss(w, r, key, path, nil, true, false)
		return true
	case 1:
		v := matches[0]
		fresh := s.currentTime().Before(v.ExpiresAt) && !requestPolicy.forceRevalidate
		if fresh {
			return s.serveHit(w, r, key, path, v)
		}
		// A ranged request is answered by forwarding it: turning a 206 into a
		// full revalidation would lose the range semantics. A 304 from a
		// client's own If-Range validator is still handled upstream.
		if strings.TrimSpace(r.Header.Get("Range")) != "" {
			s.serveMiss(w, r, key, path, &v, true, false)
			return true
		}
		s.serveRevalidation(w, r, key, path, v)
		return true
	default:
		// Several records with different Vary sets all match. Picking one would
		// invent an identity the upstream never stated, so re-fetch instead.
		log.Printf("relay cache: %q is ambiguous (%d matching variants), re-fetching", key, len(matches))
		s.serveMiss(w, r, key, path, nil, true, true)
		return true
	}
}

// cacheKey is the mirror name plus the mount-relative escaped path and the raw
// query. The request scheme, relay domain and method deliberately stay out of
// it: a mirror's content is identified the same way whether it was reached by
// subdomain or by path, over HTTPS or keepHTTP, with GET or HEAD.
func cacheKey(m Mirror, path string, u *url.URL) string {
	if path == "" {
		path = "/"
	}
	key := "mirror://" + m.Name + path
	if u != nil && u.RawQuery != "" {
		key += "?" + u.RawQuery
	}
	return key
}

func isRelaytestPath(path string) bool {
	return path == "/relaytest" || strings.HasPrefix(path, "/relaytest/")
}

// matchingVariants filters a URL's variants by the request headers. It
// deliberately ignores accept-encoding: the coding is a property of the stored
// body and is negotiated at delivery, never part of a variant identity.
func matchingVariants(variants []hashstore.Variant, h http.Header) []hashstore.Variant {
	out := make([]hashstore.Variant, 0, len(variants))
	for _, v := range variants {
		if variantMatches(v, h) {
			out = append(out, v)
		}
	}
	return out
}

func variantMatches(v hashstore.Variant, h http.Header) bool {
	if v.NoVary {
		return true
	}
	v.Vary = withoutAcceptEncoding(v.Vary)
	return v.Matches(h)
}

func withoutAcceptEncoding(vary []string) []string {
	if len(vary) == 0 {
		return vary
	}
	out := make([]string, 0, len(vary))
	for _, name := range vary {
		if strings.EqualFold(name, "accept-encoding") {
			continue
		}
		out = append(out, name)
	}
	return out
}

func (s *Server) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// serveHit answers a request from one fresh variant. It returns false only if
// the caller should use the ordinary proxy path; definite body loss is
// converted into a cache miss inside.
func (s *Server) serveHit(w http.ResponseWriter, r *http.Request, key, path string, v hashstore.Variant) bool {
	storedCoding, ok := ContentCodingFromHeaders(v.ResponseHeaders)
	if !ok {
		log.Printf("relay cache: %q has unsupported Content-Encoding %q", key, v.ResponseHeaders.Get("Content-Encoding"))
		// Do not leave an entry that can only ever fall back upstream while
		// still occupying the store.
		s.dropVariant(v)
		return false
	}
	target, transcode, acceptable := AcceptEncodingFromRequest(r).Negotiate(storedCoding)
	if !acceptable {
		return false
	}

	// Conditional evaluation uses the representation that would actually be
	// delivered. A transcoded delivery has a weak validator, so a strong
	// If-Match can never match it.
	conditionHeaders := v.ResponseHeaders
	if transcode {
		conditionHeaders = v.ResponseHeaders.Clone()
		if conditionHeaders == nil {
			conditionHeaders = make(http.Header)
		}
		ApplyTranscodedResponseHeaders(conditionHeaders, target)
	}
	switch evaluatePreconditions(r, conditionHeaders) {
	case preconditionNotModified:
		writeNotModified(w, r, s, v, target, transcode)
		return true
	case preconditionFailed:
		w.WriteHeader(http.StatusPreconditionFailed)
		return true
	}

	hasRange := strings.TrimSpace(r.Header.Get("Range")) != ""
	// HEAD never evaluates Range and never produces a body, but it must honour
	// the same transcoding prohibitions as GET.
	if r.Method == http.MethodHead {
		if transcode && TranscodingForbidden(v.ResponseHeaders, path) {
			return false
		}
		if ok, served := s.ensureBody(w, r, key, path, v); !ok {
			return served
		}
		if transcode {
			h := s.deliveryHeaders(v)
			ApplyTranscodedResponseHeaders(h, target)
			h.Del("Accept-Ranges")
			copyHeader(w.Header(), h)
			w.WriteHeader(http.StatusOK)
			return true
		}
		h := s.deliveryHeaders(v)
		h.Set("Content-Length", fmt.Sprint(v.BodySize))
		h.Set("Accept-Ranges", "bytes")
		copyHeader(w.Header(), h)
		w.WriteHeader(http.StatusOK)
		return true
	}

	if transcode {
		if hasRange {
			// Slicing a stream that is being decoded and re-encoded would need a
			// full buffered copy; upstream answers instead.
			return false
		}
		if TranscodingForbidden(v.ResponseHeaders, path) {
			return false
		}
		return s.serveTranscoded(w, r, v, storedCoding, target)
	}

	if hasRange {
		if ok, served := s.ensureBody(w, r, key, path, v); !ok {
			return served
		}
		s.serveRanges(w, r, v)
		return true
	}

	rc, size, err := s.hashStore.Get(r.Context(), hashstore.SHA256, v.BodySHA256)
	if err != nil {
		if errors.Is(err, hashstore.ErrNotFound) {
			s.repairMissingVariant(w, r, key, path, v)
			return true
		}
		log.Printf("relay cache: open %q: %v", key, err)
		return false
	}
	defer rc.Close()
	if size != v.BodySize {
		log.Printf("relay cache: %q stored size %d, body size %d", key, v.BodySize, size)
	}
	h := s.deliveryHeaders(v)
	h.Set("Content-Length", fmt.Sprint(v.BodySize))
	h.Set("Accept-Ranges", "bytes")
	copyHeader(w.Header(), h)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil && r.Context().Err() == nil {
		log.Printf("relay cache: stream %q: %v", key, err)
	}
	return true
}

// ensureBody verifies the body behind a variant. A definite loss becomes a
// cache miss: the stale metadata is dropped and the request is answered from
// upstream, so served reports that the response has already been written. A
// transient error is only logged; ok is false and served is false, telling the
// caller to use the ordinary proxy without rewriting cache state.
func (s *Server) ensureBody(w http.ResponseWriter, r *http.Request, key, path string, v hashstore.Variant) (ok, served bool) {
	_, exists, err := s.hashStore.Stat(r.Context(), hashstore.SHA256, v.BodySHA256)
	if err != nil {
		log.Printf("relay cache: stat %q: %v", key, err)
		return false, false
	}
	if !exists {
		s.repairMissingVariant(w, r, key, path, v)
		return false, true
	}
	return true, false
}

// repairMissingVariant is called when metadata names a body that is definitely
// gone. It drops the stale metadata and answers the request as a fresh miss.
func (s *Server) repairMissingVariant(w http.ResponseWriter, r *http.Request, key, path string, v hashstore.Variant) {
	s.dropVariant(v)
	s.serveMiss(w, r, key, path, nil, true, false)
}

func (s *Server) serveTranscoded(w http.ResponseWriter, r *http.Request, v hashstore.Variant, from, to ContentCoding) bool {
	rc, _, err := s.hashStore.Get(r.Context(), hashstore.SHA256, v.BodySHA256)
	if err != nil {
		if errors.Is(err, hashstore.ErrNotFound) {
			s.dropVariant(v)
			return false
		}
		log.Printf("relay cache: transcode open %q: %v", v.URL, err)
		return false
	}
	defer rc.Close()

	body, err := Transcode(rc, from, to)
	if err != nil {
		log.Printf("relay cache: transcode %q: %v", v.URL, err)
		return false
	}
	defer body.Close()

	h := s.deliveryHeaders(v)
	ApplyTranscodedResponseHeaders(h, to)
	h.Del("Accept-Ranges")
	copyHeader(w.Header(), h)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil && r.Context().Err() == nil {
		log.Printf("relay cache: stream transcoded %q: %v", v.URL, err)
	}
	return true
}

// serveRevalidation issues a conditional request with the entry's validators.
// The client's own conditional headers are removed first: they are evaluated
// against the local copy afterwards, so an upstream 304 caused by our own
// If-None-Match can never be mistaken for an answer to the client.
func (s *Server) serveRevalidation(w http.ResponseWriter, r *http.Request, key, path string, v hashstore.Variant) {
	out, err := s.buildUpstreamRequest(r.Context(), r)
	if err != nil {
		log.Printf("relay cache: revalidate %q: %v", key, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	stripConditionalHeaders(out.Header)
	out.Header.Del("Range")
	out.Method = r.Method
	out.Body = nil
	out.GetBody = nil
	out.ContentLength = 0
	if etag := v.ResponseHeaders.Get("ETag"); etag != "" {
		out.Header.Set("If-None-Match", etag)
	}
	if modified := v.ResponseHeaders.Get("Last-Modified"); modified != "" {
		out.Header.Set("If-Modified-Since", modified)
	}

	resp, err := s.roundTrip(out)
	if err != nil {
		s.writeUpstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		next := s.revalidatedVariant(v, resp)
		directives := RelayDirectivesFromHeader(resp.Header)
		if storableMetadata(next.ResponseHeaders, directives, next.NoVary) {
			_ = s.commitVariant(r.Context(), next, nil)
		} else {
			// The 304 verified this body but also declared it must no longer be
			// stored. Serve from the verified copy, drop the old record, and let
			// the next request start over as a MISS.
			s.dropVariant(v)
		}
		if !s.serveHit(w, r, key, path, next) {
			s.proxy.ServeHTTP(w, r)
		}
	case http.StatusNotFound, http.StatusGone:
		s.dropVariant(v)
		s.deliverUpstream(w, r, resp, key, nil, true, false)
	case http.StatusOK:
		// The upstream answered the revalidation with a full representation.
		// The client's own conditions were removed from the conditional
		// request, so evaluate them now against the new response before any
		// body is delivered.
		switch evaluatePreconditions(r, resp.Header) {
		case preconditionNotModified:
			writeNotModifiedFromHeaders(w, resp.Header)
			return
		case preconditionFailed:
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		s.deliverUpstream(w, r, resp, key, &v, true, false)
	default:
		s.deliverUpstream(w, r, resp, key, &v, true, false)
	}
}

// revalidatedVariant folds a 304's metadata into the stored variant. Vary and
// Content-Encoding stay untouched: they describe the identity and the stored
// bytes, neither of which a 304 can change. The destination is the same key, so
// the variant's identity is preserved.
func (s *Server) revalidatedVariant(v hashstore.Variant, resp *http.Response) hashstore.Variant {
	folded := storageHeaders(resp.Header)
	merged := v.ResponseHeaders.Clone()
	for name, values := range folded {
		switch http.CanonicalHeaderKey(name) {
		case "Vary", "Content-Encoding", "Content-Length":
			continue
		}
		merged[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	now := s.currentTime()
	v.ResponseHeaders = merged
	v.StoredAt = now
	v.ExpiresAt = freshnessExpiry(merged, RelayDirectivesFromHeader(resp.Header), now)
	return v
}

// serveMiss fetches the upstream response and delivers it. old is set only when
// a stale variant's ranged request bypassed revalidation: a full 200 then
// replaces that variant, while a 206 leaves it alone. writable is false for a
// request that must not populate the cache (client Cache-Control: no-store).
func (s *Server) serveMiss(w http.ResponseWriter, r *http.Request, key, path string, old *hashstore.Variant, writable, cleanupAmbiguous bool) {
	out, err := s.buildUpstreamRequest(r.Context(), r)
	if err != nil {
		log.Printf("relay cache: request %q: %v", key, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	resp, err := s.roundTrip(out)
	if err != nil {
		s.writeUpstreamError(w, r, err)
		return
	}
	defer resp.Body.Close()
	s.deliverUpstream(w, r, resp, key, old, writable, cleanupAmbiguous)
}

// deliverUpstream decides whether an upstream response can enter the store and
// either streams it with a tee or passes it through.
func (s *Server) deliverUpstream(w http.ResponseWriter, r *http.Request, resp *http.Response, key string, old *hashstore.Variant, writable, cleanupAmbiguous bool) {
	directives := RelayDirectivesFromHeader(resp.Header)

	// A ranged request that received a full 200 means upstream ignored Range.
	// The full response may be cached, but the operator should be able to see
	// that a Range MISS turned into a full-body transfer.
	if r.Method == http.MethodGet && resp.StatusCode == http.StatusOK && strings.TrimSpace(r.Header.Get("Range")) != "" {
		log.Printf("relay cache warning: %s %s ignored Range and returned a full 200", r.Method, r.URL.Path)
	}

	storable := writable && s.storable(resp, r, directives)
	if storable {
		s.streamAndStore(w, r, resp, key, directives, old, cleanupAmbiguous)
		return
	}

	if old != nil {
		switch resp.StatusCode {
		case http.StatusOK, http.StatusNotFound, http.StatusGone:
			// The stored copy is known stale and the upstream no longer agrees
			// with it. Nothing new may be written, so drop the old record.
			s.dropVariant(*old)
		}
	}
	deliverRaw(w, resp)
}

// storable reports whether this response may become a cache entry. All the
// prohibitions are response facts; none of them can be relaxed by no-vary.
func (s *Server) storable(resp *http.Response, r *http.Request, directives RelayDirectives) bool {
	if r.Method != http.MethodGet || resp.StatusCode != http.StatusOK {
		return false
	}
	if directives.NoStore {
		return false
	}
	if cacheControlHas(resp.Header.Values("Cache-Control"), "no-store") ||
		cacheControlHas(resp.Header.Values("Cache-Control"), "private") {
		return false
	}
	if !directives.NoVary && varyContainsStar(resp.Header.Values("Vary")) {
		return false
	}
	if resp.Header.Get("Content-Range") != "" {
		return false
	}
	// A coding the relay cannot decode could never be negotiated or transcoded
	// later; storing it would leave an entry that permanently falls back
	// upstream while still occupying the cache.
	if _, ok := ContentCodingFromHeaders(resp.Header); !ok {
		return false
	}
	return resp.Body != nil
}

// storableMetadata applies the response-controlled store rules to metadata
// that is about to be persisted, without a body to inspect. It is used for the
// 304 revalidation path: a 304 that newly declares no-store or private must
// invalidate the entry instead of refreshing it.
func storableMetadata(h http.Header, directives RelayDirectives, noVary bool) bool {
	if directives.NoStore {
		return false
	}
	if cacheControlHas(h.Values("Cache-Control"), "no-store") ||
		cacheControlHas(h.Values("Cache-Control"), "private") {
		return false
	}
	if !noVary && varyContainsStar(h.Values("Vary")) {
		return false
	}
	return true
}

// streamAndStore delivers the response while teeing it into the store. Storage
// failures (an oversized body, a broken backend) only disable the tee: the
// client still receives the complete upstream response. A body whose declared
// length already exceeds the store's per-object budget is not teed at all.
func (s *Server) streamAndStore(w http.ResponseWriter, r *http.Request, resp *http.Response, key string, directives RelayDirectives, old *hashstore.Variant, cleanupAmbiguous bool) {
	stored := storageHeaders(resp.Header)
	storedAt := s.currentTime()
	expiresAt := freshnessExpiry(resp.Header, directives, storedAt)

	delivery := resp.Header.Clone()
	stripHopByHop(delivery)
	delivery.Del(RelayDirectiveHeader)
	copyHeader(w.Header(), delivery)
	w.WriteHeader(resp.StatusCode)

	// Put needs the real length to reject an oversized body before reading it.
	// Stats().Capacity is the LRU budget and the largest single body, so a declared length above it can never become an entry: skip the tee
	// rather than making Put pull a budget-sized temporary file first.
	putSize := resp.ContentLength
	budget := s.hashStore.Stats().Capacity
	storeBody := putSize < 0 || putSize <= budget

	type putResult struct {
		content hashstore.Content
		err     error
	}
	var resultCh chan putResult
	var pw *io.PipeWriter
	if storeBody {
		pr, writer := io.Pipe()
		pw = writer
		resultCh = make(chan putResult, 1)
		putCtx := context.WithoutCancel(r.Context())
		go func() {
			// Closing the reader is what unblocks a tee write after Put stops
			// reading (an oversized body or any store error). It must run even if
			// Put panics.
			defer pr.Close()
			result := putResult{}
			defer func() {
				if recovered := recover(); recovered != nil {
					result.err = fmt.Errorf("hashstore Put panic: %v", recovered)
				}
				// Buffered, so this send can never wait for the consumer and leave
				// the stream blocked on a full result channel.
				resultCh <- result
			}()
			result.content, result.err = s.hashStore.Put(putCtx, pr, putSize)
		}()
	} else {
		log.Printf("relay cache: %q declares %d bytes (budget %d), serving without storing", key, putSize, budget)
	}

	buf := make([]byte, 32*1024)
	tee := storeBody
	complete := false
	clientOK := true
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				clientOK = false
				break
			}
			if tee {
				if _, perr := pw.Write(buf[:n]); perr != nil {
					tee = false
					_ = pw.CloseWithError(perr)
				}
			}
		}
		if err == io.EOF {
			complete = true
			break
		}
		if err != nil {
			break
		}
	}

	if tee {
		if complete && clientOK {
			_ = pw.Close()
		} else {
			_ = pw.CloseWithError(errClientAborted)
		}
	}
	if complete && clientOK {
		// The client has every byte; flush now instead of waiting for the store
		// to finish. A ResponseWriter that cannot flush is fine.
		_ = http.NewResponseController(w).Flush()
	}
	_ = resp.Body.Close()
	if !storeBody {
		return
	}
	result := <-resultCh

	if !complete || !clientOK {
		// The body never fully reached the client, so no variant may name it.
		return
	}
	if result.err != nil {
		log.Printf("relay cache: store %q: %v", key, result.err)
		return
	}

	next := buildVariant(key, stored, directives, r.Header, storedAt, expiresAt, result.content)
	if err := s.commitVariant(r.Context(), next, old); err != nil {
		return
	}
	if cleanupAmbiguous {
		s.removeOtherVariants(key, next)
	}
}

func buildVariant(key string, stored http.Header, directives RelayDirectives, requestHeaders http.Header, storedAt, expiresAt time.Time, content hashstore.Content) hashstore.Variant {
	v := hashstore.Variant{
		URL:             key,
		ResponseHeaders: stored,
		BodySHA256:      content.SHA256,
		BodySize:        content.Size,
		StoredAt:        storedAt,
		ExpiresAt:       expiresAt,
	}
	if directives.NoVary {
		v.NoVary = true
		return v
	}
	vary := withoutAcceptEncoding(varyNames(stored.Values("Vary")))
	if len(vary) == 0 {
		v.NoVary = true
		return v
	}
	v.Vary = vary
	v.RequestHeaders = requestHeaders.Clone()
	return v
}

// commitVariant writes metadata after the response has already been delivered.
// A failure is logged, never surfaced: the client's response is complete. The
// error is returned only so callers can decide whether follow-up cleanup (an
// ambiguity rewrite) may run.
func (s *Server) commitVariant(parent context.Context, v hashstore.Variant, old *hashstore.Variant) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), variantCommitTimeout)
	defer cancel()

	var err error
	if old != nil {
		err = s.hashStore.ReplaceVariant(ctx, *old, v)
	} else {
		err = s.hashStore.PutVariant(ctx, v)
	}
	if err != nil && !errors.Is(err, hashstore.ErrClosed) {
		log.Printf("relay cache: commit %q: %v", v.URL, err)
	}
	return err
}

// removeOtherVariants is the cleanup after an ambiguous refetch stored a new
// identity: every other variant for the same URL is deleted so the next request
// has exactly one candidate. A failure here only leaves the old ambiguity in
// place; the delivered response is already complete.
func (s *Server) removeOtherVariants(key string, keep hashstore.Variant) {
	ctx, cancel := context.WithTimeout(context.Background(), variantCleanupTimeout)
	defer cancel()

	variants, err := s.hashStore.Variants(ctx, key)
	if err != nil {
		log.Printf("relay cache: list variants for cleanup %q: %v", key, err)
		return
	}
	for _, v := range variants {
		if sameVariantIdentity(v, keep) {
			continue
		}
		if err := s.hashStore.DeleteVariant(ctx, v); err != nil && !errors.Is(err, hashstore.ErrClosed) {
			log.Printf("relay cache: cleanup variant %q: %v", key, err)
		}
	}
}

func sameVariantIdentity(a, b hashstore.Variant) bool {
	if a.NoVary || b.NoVary {
		return a.NoVary == b.NoVary
	}
	if len(a.Vary) != len(b.Vary) {
		return false
	}
	for _, name := range a.Vary {
		found := false
		for _, other := range b.Vary {
			if !strings.EqualFold(name, other) {
				continue
			}
			found = true
			if variantHeaderValue(a.RequestHeaders, name) != variantHeaderValue(b.RequestHeaders, other) {
				return false
			}
			break
		}
		if !found {
			return false
		}
	}
	return true
}

// variantHeaderValue reads a varied request value case-insensitively, using
// the same normalisation the store applies: trim each value, drop empties, and
// join multiple values in order. Stored variants use lowercase names while a
// freshly built one carries canonical names, so a direct map lookup would miss
// one side.
func variantHeaderValue(h http.Header, name string) string {
	var parts []string
	for key, values := range h {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if value = strings.TrimSpace(value); value != "" {
				parts = append(parts, value)
			}
		}
	}
	return strings.Join(parts, ", ")
}

func (s *Server) dropVariant(v hashstore.Variant) {
	ctx, cancel := context.WithTimeout(context.Background(), variantCleanupTimeout)
	defer cancel()
	if err := s.hashStore.DeleteVariant(ctx, v); err != nil && !errors.Is(err, hashstore.ErrClosed) {
		log.Printf("relay cache: delete variant %q: %v", v.URL, err)
	}
}

func (s *Server) deliveryHeaders(v hashstore.Variant) http.Header {
	h := v.ResponseHeaders.Clone()
	if h == nil {
		h = make(http.Header)
	}
	h.Del("Content-Length")
	h.Del("Transfer-Encoding")
	h.Del("Content-Range")
	h.Del("Age")
	h.Del(RelayDirectiveHeader)
	h.Set("Age", fmt.Sprint(ageSeconds(v.StoredAt, s.currentTime())))
	return h
}

func writeNotModified(w http.ResponseWriter, r *http.Request, s *Server, v hashstore.Variant, target ContentCoding, transcode bool) {
	h := s.deliveryHeaders(v)
	h.Del("Content-Type")
	if transcode {
		ApplyTranscodedResponseHeaders(h, target)
		h.Del("Accept-Ranges")
	}
	copyHeader(w.Header(), h)
	w.WriteHeader(http.StatusNotModified)
}

// writeNotModifiedFromHeaders answers 304 from an upstream 200's header set,
// used when the client's own preconditions are re-evaluated after a stale
// revalidation produced new content.
func writeNotModifiedFromHeaders(w http.ResponseWriter, h http.Header) {
	out := h.Clone()
	if out == nil {
		out = make(http.Header)
	}
	stripHopByHop(out)
	out.Del(RelayDirectiveHeader)
	out.Del("Content-Length")
	out.Del("Content-Type")
	copyHeader(w.Header(), out)
	w.WriteHeader(http.StatusNotModified)
}

func deliverRaw(w http.ResponseWriter, resp *http.Response) {
	h := resp.Header.Clone()
	stripHopByHop(h)
	h.Del(RelayDirectiveHeader)
	copyHeader(w.Header(), h)
	w.WriteHeader(resp.StatusCode)
	if resp.Body != nil {
		_, _ = io.Copy(w, resp.Body)
	}
}

// buildUpstreamRequest maps an inbound request onto the upstream origin using
// the same host and base-path rules as rewriteUpstream.
func (s *Server) buildUpstreamRequest(ctx context.Context, in *http.Request) (*http.Request, error) {
	out := in.Clone(ctx)
	out.RequestURI = ""
	out.URL = cloneInboundURL(in.URL)

	basePath, baseRawPath := s.upstream.Path, s.upstream.RawPath
	if basePath == "" || basePath == "/" {
		basePath, baseRawPath = "", ""
	}

	out.URL.Scheme = s.upstream.Scheme
	out.URL.Host = s.upstreamHostFor(requestHost(in))
	if basePath != "" {
		basePath = strings.TrimSuffix(basePath, "/")
		out.URL.Path = basePath + in.URL.Path
		escapedBase := strings.TrimSuffix(baseRawPath, "/")
		if escapedBase == "" && in.URL.RawPath != "" {
			// The upstream base path needs an escaped form only when the inbound
			// path has one to concatenate. Let net/url escape the base path
			// consistently instead of dropping the inbound RawPath.
			escapedBase = (&url.URL{Path: basePath}).EscapedPath()
		}
		if escapedBase != "" {
			out.URL.RawPath = escapedBase + in.URL.EscapedPath()
		} else {
			out.URL.RawPath = ""
		}
	}

	out.Header = in.Header.Clone()
	stripRequestHopByHop(out.Header)
	setXForwarded(out, in)
	if out.Header.Get(RelayHostHeader) == "" {
		if _, base := s.matchBaseDomain(requestHost(in)); base != "" {
			out.Header.Set(RelayHostHeader, base)
		}
	}
	out.Host = out.URL.Host
	out.Close = false
	return out, nil
}

func cloneInboundURL(u *url.URL) *url.URL {
	if u == nil {
		return &url.URL{}
	}
	clone := *u
	return &clone
}

func (s *Server) roundTrip(req *http.Request) (*http.Response, error) {
	rt := s.upstreamTransport()
	if rt == nil {
		rt = http.DefaultTransport
	}
	return rt.RoundTrip(req)
}

func (s *Server) writeUpstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	log.Printf("relay cache: upstream error for %s %s: %v", r.Method, r.URL.Redacted(), err)
	http.Error(w, "upstream error", http.StatusBadGateway)
}

// setXForwarded mirrors httputil.ProxyRequest.SetXForwarded: append the direct
// client to X-Forwarded-For and describe the inbound request.
func setXForwarded(out, in *http.Request) {
	clientIP := in.RemoteAddr
	if host, _, err := net.SplitHostPort(in.RemoteAddr); err == nil {
		clientIP = host
	}
	if clientIP != "" {
		prior := out.Header.Values("X-Forwarded-For")
		if len(prior) > 0 {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		out.Header.Set("X-Forwarded-For", clientIP)
	}
	out.Header.Set("X-Forwarded-Host", in.Host)
	if in.TLS != nil {
		out.Header.Set("X-Forwarded-Proto", "https")
	} else {
		out.Header.Set("X-Forwarded-Proto", "http")
	}
}

func copyHeader(dst, src http.Header) {
	for name, values := range src {
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}
