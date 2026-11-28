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
		s.serveMiss(w, r, dispositionRequest{key: key, path: path})
		return true
	}

	// An immutable policy declares the bytes under the path cannot change. Only
	// a GET that the client allows to be cached may assemble them in the active
	// segment; a HEAD may only reuse a body that is already addressable, since
	// it must never create an entry.
	immutable, digest := immutableFromContext(r.Context())
	identityAcceptable := AcceptEncodingFromRequest(r).Accepts(CodingIdentity)
	digestEligible := s.active != nil && immutable && !requestPolicy.forceRevalidate &&
		identityAcceptable && (r.Method == http.MethodGet || r.Method == http.MethodHead)
	activeEligible := digestEligible && r.Method == http.MethodGet
	// A manifest's media type and headers are repository metadata, not a
	// property of its digest. A synthetic digest-only hit would serve another
	// repository's manifest as application/octet-stream, so manifests stay on
	// their own URL variant (and the active path keys them by URL).
	digestHitEligible := digestEligible && !isOCIManifestPath(path)

	variants, err := s.hashStore.Variants(r.Context(), key)
	if err != nil {
		log.Printf("relay cache: list %q: %v", key, err)
		return false
	}

	// A fresh APT index is authoritative for the paths it covers. A variant
	// whose body contradicts the index must not be served; dropping it lets the
	// hash-prediction path (or a normal miss) answer instead.
	expected, expectedGen, expectedOK := s.freshAPTFile(mirror, path, s.currentTime())
	matches := matchingVariants(variants, r.Header)
	if expectedOK && len(matches) == 1 && !s.variantMatchesExpectedFile(r.Context(), matches[0], expected) {
		log.Printf("relay prefetch: dropping variant %q: body hash disagrees with the fresh index", key)
		s.dropVariant(matches[0])
		matches = nil
	}
	switch len(matches) {
	case 0:
		// A client that asked for revalidation (no-cache, max-age=0, Pragma:
		// no-cache) must reach the upstream, even though the predicted delivery
		// would satisfy the request without one.
		if expectedOK && !requestPolicy.forceRevalidate && s.servePredicted(w, r, key, path, expected, expectedGen) {
			return true
		}
		if digestHitEligible {
			// A body already published under the path's digest is the same bytes
			// no matter which mount or repository names it, so it is served
			// without an upstream request.
			if s.serveImmutableDigestHit(w, r, key, path, digest) {
				return true
			}
		}
		if activeEligible {
			if s.serveImmutableActive(w, r, key, path, digest) {
				return true
			}
		}
		s.serveMiss(w, r, dispositionRequest{
			key: key, path: path, writable: true, allowResponseActive: true,
		})
		return true
	case 1:
		v := matches[0]
		fresh := s.currentTime().Before(v.ExpiresAt) && !requestPolicy.forceRevalidate
		if fresh {
			if s.prefetch != nil {
				s.prefetch.TouchNormalVariant(key, v, s.currentTime())
			}
			return s.serveHit(w, r, key, path, v)
		}
		// An OCI digest address may still be servable locally even though the
		// metadata is stale: the body cannot change under that URL, so freshness
		// says nothing about the bytes. A client that asked for revalidation
		// (no-cache, max-age=0) still reaches the upstream.
		if !requestPolicy.forceRevalidate && s.serveStaleDigestHit(w, r, key, path, v) {
			return true
		}
		// The same reasoning applies to every path the merged immutable pattern
		// matched: the policy itself states the bytes cannot change. A captured
		// digest additionally has to name the stored body.
		if s.active != nil && !requestPolicy.forceRevalidate && immutable && s.serveImmutableStaleHit(w, r, key, path, v, digest) {
			return true
		}
		// A ranged request is answered by forwarding it: turning a 206 into a
		// full revalidation would lose the range semantics. A 304 from a
		// client's own If-Range validator is still handled upstream.
		if strings.TrimSpace(r.Header.Get("Range")) != "" {
			s.serveMiss(w, r, dispositionRequest{
				key: key, path: path, old: &v, writable: true, allowResponseActive: true,
			})
			return true
		}
		s.serveRevalidation(w, r, key, path, v)
		return true
	default:
		// Several records with different Vary sets all match. Picking one would
		// invent an identity the upstream never stated, so re-fetch instead.
		log.Printf("relay cache: %q is ambiguous (%d matching variants), re-fetching", key, len(matches))
		s.serveMiss(w, r, dispositionRequest{
			key: key, path: path, writable: true, cleanupAmbiguous: true, allowResponseActive: true,
		})
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
		if relayStoredIdentity(v).Matches(h) {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// storedRepresentationSource serves one hash-store body. The store is the only
// place a complete body lives, so the source is just its digest plus the
// acquisition calls; everything after that is representation delivery.
type storedRepresentationSource struct {
	store  *hashstore.Store
	digest string
	size   int64
	label  string
}

var _ representationSource = storedRepresentationSource{}

func (s storedRepresentationSource) probe(ctx context.Context) error {
	_, exists, err := s.store.Stat(ctx, hashstore.SHA256, s.digest)
	if err != nil {
		return err
	}
	if !exists {
		return hashstore.ErrNotFound
	}
	return nil
}

func (s storedRepresentationSource) open(ctx context.Context) (io.ReadCloser, error) {
	rc, size, err := s.store.Get(ctx, hashstore.SHA256, s.digest)
	if err == nil && s.size >= 0 && size != s.size {
		log.Printf("relay cache: %q stored size %d, body size %d", s.label, s.size, size)
	}
	return rc, err
}

func (s storedRepresentationSource) openSection(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	rc, total, err := s.store.Range(ctx, hashstore.SHA256, s.digest, offset)
	if err == nil && s.size >= 0 && total != s.size {
		log.Printf("relay cache: %q stored size %d, body size %d", s.label, s.size, total)
	}
	return rc, err
}

// serveHit answers a request from one fresh variant. Coding negotiation and
// transcoding stay here; conditional, range and full-body semantics belong to
// deliverRepresentation. It returns false only if the caller should use the
// ordinary proxy path; definite body loss is converted into a cache miss inside.
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
	// A digest-addressed or no-transform response may not be re-encoded. When
	// the client accepts the stored coding, deliver it unchanged instead of
	// punishing the hit with an upstream fetch; only a client that cannot take
	// the stored coding falls through.
	if transcode && TranscodingForbidden(v.ResponseHeaders, path) {
		if !AcceptEncodingFromRequest(r).Accepts(storedCoding) {
			return false
		}
		target, transcode = storedCoding, false
	}

	// Conditional evaluation uses the representation that would actually be
	// delivered. A transcoded delivery has a weak validator, so a strong
	// If-Match can never match it.
	validators := v.ResponseHeaders
	header := representationHeaders(v.ResponseHeaders, v.StoredAt, s.currentTime())
	acceptRanges := true
	if transcode {
		validators = v.ResponseHeaders.Clone()
		if validators == nil {
			validators = make(http.Header)
		}
		ApplyTranscodedResponseHeaders(validators, target)
		ApplyTranscodedResponseHeaders(header, target)
		header.Del("Accept-Ranges")
		acceptRanges = false
	}

	res := deliverRepresentation(w, r, representation{
		validators:   validators,
		header:       header,
		size:         v.BodySize,
		acceptRanges: acceptRanges,
		source: storedRepresentationSource{
			store:  s.hashStore,
			digest: v.BodySHA256,
			size:   v.BodySize,
			label:  key,
		},
		// The transcoded body is written by serveTranscoded: this module still
		// answers conditions and HEAD, but must not open the stored bytes.
		deferBody: transcode,
		label:     key,
	})
	if res.Delivered {
		// A multipart failure is logged by the delivery module at the failing
		// stage; other shapes have no other observer.
		if !res.Multipart {
			if err := firstDeliveryError(res); err != nil && r.Context().Err() == nil {
				log.Printf("relay cache: stream %q: %v", key, err)
			}
		}
		return true
	}
	if res.Err == nil {
		// The caller owns the body. A ranged transcoding request reaches
		// upstream: slicing a stream while decoding it would need a full copy.
		if transcode && strings.TrimSpace(r.Header.Get("Range")) == "" {
			return s.serveTranscoded(w, r, v, storedCoding, target)
		}
		return false
	}
	if errors.Is(res.Err, hashstore.ErrNotFound) {
		// Any definite body loss, ranged or not, is repaired as a fresh miss:
		// the client's own Range is forwarded, so a multipart request can never
		// receive a truncated multipart body built from a vanished entry.
		s.repairMissingVariant(w, r, key, path, v)
		return true
	}
	log.Printf("relay cache: open %q: %v", key, res.Err)
	return false
}

// firstDeliveryError returns the copy failure worth logging: a source failure
// is always reported, and a client-side failure is reported too (an ordinary
// delivery has no other observer). Active callers classify the same fields
// themselves.
func firstDeliveryError(res deliveryResult) error {
	if res.ReadErr != nil {
		return res.ReadErr
	}
	return res.ClientErr
}

// serveStaleDigestHit answers a stale entry from the local store when the
// request addresses an OCI object by digest and the stored body is provably
// that object: the path digest names the same SHA-256 the variant recorded, and
// the body is present at the recorded size. A digest address cannot be
// re-pointed, so the metadata's freshness has no bearing on the bytes.
//
// It reuses serveHit, so conditional evaluation, Range, HEAD, coding
// negotiation and the digest transcoding prohibition all behave exactly as on a
// fresh hit; nothing here rewrites the variant or its expiry. Any doubt — a tag,
// another algorithm, a digest mismatch, a missing or resized body, a store
// error — reports false, and the caller keeps its ordinary stale path.
func (s *Server) serveStaleDigestHit(w http.ResponseWriter, r *http.Request, key, path string, v hashstore.Variant) bool {
	digest, ok := OCIDigestFromPath(path)
	if !ok {
		return false
	}
	bodyDigest, err := hashstore.NormalizeDigest(hashstore.SHA256, v.BodySHA256)
	if err != nil || digest != bodyDigest {
		return false
	}
	size, exists, err := s.hashStore.Stat(r.Context(), hashstore.SHA256, bodyDigest)
	if err != nil {
		log.Printf("relay cache: stat %q for digest hit: %v", key, err)
		return false
	}
	if !exists || size != v.BodySize {
		return false
	}
	return s.serveHit(w, r, key, path, v)
}

// repairMissingVariant is called when metadata names a body that is definitely
// gone. It drops the stale metadata and answers the request as a fresh miss.
func (s *Server) repairMissingVariant(w http.ResponseWriter, r *http.Request, key, path string, v hashstore.Variant) {
	s.dropVariant(v)
	s.serveMiss(w, r, dispositionRequest{key: key, path: path, writable: true, allowResponseActive: true})
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

	h := representationHeaders(v.ResponseHeaders, v.StoredAt, s.currentTime())
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

	// Every revalidation response is one disposition. A 304 folds metadata, a
	// 200 answers the client's own conditions before storage, and a definite
	// absence invalidates the stale entry.
	s.disposition().settle(w, r, dispositionRequest{
		key:             key,
		path:            path,
		resp:            resp,
		old:             &v,
		writable:        true,
		revalidated:     true,
		revalidatedFrom: &v,
	})
}

// revalidatedVariant folds a 304's metadata into the stored variant. Vary and
// Content-Encoding stay untouched: they describe the identity and the stored
// bytes, neither of which a 304 can change. The destination is the same key, so
// the variant's identity is preserved.
//
// A persistent variant (one whose response declared its own immutability) keeps
// the persistent lifetime even when the 304 states a short one and does not
// repeat the declaration: the bytes still cannot change. Callers rewrite the
// stored headers with withPersistentLifetime after the store rules accept the
// 304, so a 304 that newly declares no-store/private still invalidates it.
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
	if v.Persistent {
		v.ExpiresAt = now.Add(persistentVariantLifetime)
	} else {
		v.ExpiresAt = freshnessExpiry(merged, RelayDirectivesFromHeader(resp.Header), now)
	}
	return v
}

// serveMiss fetches the upstream response and hands it to the disposition
// module. The pre-response facts travel in the same request value, so callers
// never pass a row of adjacent booleans.
func (s *Server) serveMiss(w http.ResponseWriter, r *http.Request, req dispositionRequest) {
	out, err := s.buildUpstreamRequest(r.Context(), r)
	if err != nil {
		log.Printf("relay cache: request %q: %v", req.key, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	resp, err := s.roundTrip(out)
	if err != nil {
		s.writeUpstreamError(w, r, err)
		return
	}
	req.resp = resp
	s.disposition().settle(w, r, req)
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
//
// The tee's Put and the URL variant commit run in one detached task owned by
// Server.background: the handler flushes and returns once the client has every
// byte, and never waits for the store. The task learns from the delivery outcome
// whether a variant may be written, so a truncated or aborted client response
// still leaves no metadata naming a body it never received.
func (s *Server) streamAndStore(w http.ResponseWriter, r *http.Request, resp *http.Response, key string, directives RelayDirectives, old *hashstore.Variant, cleanupAmbiguous bool) {
	stored, persistent := responseVariantHeaders(resp.Header, directives)
	storedAt := s.currentTime()
	expiresAt := freshnessExpiry(stored, directives, storedAt)

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
	// A policy that captured a digest from the path states which bytes the URL
	// names. The ordinary tee is the only writer left when the active path
	// refused the assembly, so it must apply the same expectation: publishing a
	// body that contradicts the address would make the next request a hit on
	// bytes the path never named.
	digestPolicy := storedDigestPolicy(r, resp)
	var putAlgo hashstore.Algorithm
	var putDigest string
	nonIdentity := false
	if digestPolicy.declared {
		if !digestPolicy.identity {
			// A negotiated representation is not the object the digest names, so
			// there is no usable expectation to compare: the response is still
			// delivered whole, but it must not enter the store at all.
			log.Printf("relay cache: %q has a policy digest but a non-identity coding, serving without storing", key)
			nonIdentity = true
			storeBody = false
		} else {
			putAlgo, putDigest = digestPolicy.algo, digestPolicy.digest
		}
	}

	var (
		pw         *io.PipeWriter
		deliveryCh chan bool
	)
	if storeBody {
		pr, writer := io.Pipe()
		pw = writer
		deliveryCh = make(chan bool, 1)
		// Captured before returning: the request context is cancelled once the
		// handler ends, and the request itself must not be read from another
		// goroutine afterwards.
		putCtx := context.WithoutCancel(r.Context())
		reqHeaders := r.Header.Clone()
		// stop is closed when Server.Close begins. The task must not wait for a
		// slow client's last byte during shutdown: Put has already returned, the
		// pipe is closed, and no variant may be written for a response whose
		// delivery outcome is unknown.
		stop := s.background().stopping()
		if !s.background().start(func() {
			// Closing the reader is what unblocks a tee write after Put stops
			// reading (an oversized body or any store error). It must run even if
			// Put panics.
			defer pr.Close()
			var (
				content hashstore.Content
				putErr  error
			)
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						putErr = fmt.Errorf("hashstore Put panic: %v", recovered)
					}
				}()
				content, putErr = s.hashStore.PutExpected(putCtx, pr, putSize, putAlgo, putDigest)
			}()
			// Put may return before the handler wrote the whole body (an
			// oversized body, a store error). Close the reader now so the next
			// tee write fails instead of blocking on a pipe nobody reads.
			_ = pr.Close()
			select {
			case delivered := <-deliveryCh:
				if !delivered {
					// The body never fully reached the client, so no variant
					// may name it.
					return
				}
			case <-stop:
				// Shutdown began while the handler was still writing to its
				// client. The store work is already over; leave the response to
				// finish and write no metadata for an unknown delivery outcome.
				return
			}
			if putErr != nil {
				log.Printf("relay cache: store %q: %v", key, putErr)
				return
			}
			next := buildVariant(key, stored, directives, reqHeaders, storedAt, expiresAt, content)
			next.Persistent = persistent
			if err := s.commitVariant(context.Background(), next, old); err != nil {
				return
			}
			if cleanupAmbiguous {
				s.removeOtherVariants(key, next)
			}
		}) {
			// The server is already closing; the response still reaches the
			// client, just without a tee.
			_ = pw.CloseWithError(context.Canceled)
			_ = pr.Close()
			pw = nil
			deliveryCh = nil
			storeBody = false
		}
	} else if !nonIdentity {
		log.Printf("relay cache: %q declares %d bytes (budget %d), serving without storing", key, putSize, budget)
	}

	// The outcome must reach the detached task on every path, including a panic
	// in the write loop below: a task that never learns the outcome would hold
	// the server's background drain forever.
	outcome := false
	defer func() {
		if pw != nil {
			if outcome {
				_ = pw.Close()
			} else {
				_ = pw.CloseWithError(errClientAborted)
			}
		}
		if deliveryCh != nil {
			deliveryCh <- outcome
		}
	}()

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

	if complete && clientOK {
		// The client has every byte; flush now instead of waiting for the store
		// to finish. A ResponseWriter that cannot flush is fine.
		_ = http.NewResponseController(w).Flush()
	}
	outcome = complete && clientOK
	// The handler owns the upstream body through the disposition module, which
	// closes it when this call returns. The detached tee no longer touches it:
	// it only drains bytes already written into the pipe, and the outcome above
	// decides whether they may be named.
}

// digestStorePolicy describes how the store must treat a response whose merged
// policy captured a content digest from the path. declared means the capture is
// a usable digest; identity means the response body is the object's own bytes
// rather than a negotiated representation of them.
type digestStorePolicy struct {
	declared bool
	identity bool
	algo     hashstore.Algorithm
	digest   string
}

// storedDigestPolicy returns the policy digest an ordinary or prefetch store
// write must satisfy. A pattern that matched without a usable digest declares
// immutability only. The digest names the object's bytes, not a negotiated
// representation: a non-identity response is never stored, because its bytes
// cannot be verified against (and are not) that object.
func storedDigestPolicy(r *http.Request, resp *http.Response) digestStorePolicy {
	_, digest := immutableFromContext(r.Context())
	if digest.Algorithm == "" {
		return digestStorePolicy{}
	}
	algo, err := hashstore.ParseAlgorithm(digest.Algorithm)
	if err != nil {
		return digestStorePolicy{}
	}
	normalized, err := hashstore.NormalizeDigest(algo, digest.Hex)
	if err != nil {
		return digestStorePolicy{}
	}
	coding, ok := ContentCodingFromHeaders(resp.Header)
	return digestStorePolicy{
		declared: true,
		identity: ok && coding == CodingIdentity,
		algo:     algo,
		digest:   normalized,
	}
}

// responseVariantHeaders returns the header set a stored variant keeps. A
// response that declared X-MDB-Relay: immutable carries the persistent lifetime
// instead of whatever the upstream stated; its bytes cannot change. The
// delivered header set is never rewritten: the first client sees the upstream
// Cache-Control. Orthogonal directives such as no-transform survive, and a
// declaration never makes a response storable (the store rules already ran).
func responseVariantHeaders(h http.Header, directives RelayDirectives) (http.Header, bool) {
	stored := storageHeaders(h)
	if !directives.Immutable {
		return stored, false
	}
	withPersistentLifetime(stored)
	return stored, true
}

func buildVariant(key string, stored http.Header, directives RelayDirectives, requestHeaders http.Header, storedAt, expiresAt time.Time, content hashstore.Content) hashstore.Variant {
	identity := relayResponseIdentity(directives.NoVary, stored.Values("Vary"), requestHeaders)
	return hashstore.Variant{
		URL:             key,
		NoVary:          identity.NoVary(),
		Vary:            identity.Vary(),
		RequestHeaders:  identity.RequestHeaders(),
		ResponseHeaders: stored,
		BodySHA256:      content.SHA256,
		BodySize:        content.Size,
		StoredAt:        storedAt,
		ExpiresAt:       expiresAt,
	}
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
		if storedVariantIdentity(v).Equal(storedVariantIdentity(keep)) {
			continue
		}
		if err := s.hashStore.DeleteVariant(ctx, v); err != nil && !errors.Is(err, hashstore.ErrClosed) {
			log.Printf("relay cache: cleanup variant %q: %v", key, err)
		}
	}
}

func (s *Server) dropVariant(v hashstore.Variant) {
	ctx, cancel := context.WithTimeout(context.Background(), variantCleanupTimeout)
	defer cancel()
	if err := s.hashStore.DeleteVariant(ctx, v); err != nil && !errors.Is(err, hashstore.ErrClosed) {
		log.Printf("relay cache: delete variant %q: %v", v.URL, err)
	}
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
