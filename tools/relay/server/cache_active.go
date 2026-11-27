package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	tiered "github.com/geektr-cloud/go-tiered-cache"
	httpadapter "github.com/geektr-cloud/go-tiered-cache/adapters/http"
	"github.com/mirror-db/mirror-db/tools/relay/activecache"
	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// Immutable active path (M4).
//
// An immutable policy states that the bytes under a path cannot change, and may
// capture their content digest. Such a path does not need a variant identity or
// a freshness check before its body can be shared: every request for the same
// bytes is a reader of one object. The active segment assembles that object
// once, from the first upstream response, and hands it to hashstore.Adopt when
// the whole extent is cached.
//
// The path is deliberately opt-in and narrow. It handles only a client-cacheable
// GET, only while the ordinary variant lookup found nothing usable, and it
// falls back to the ordinary chain on every admission failure. Non-immutable
// requests keep streamAndStore untouched.

const (
	// activePublishTimeout bounds how long a completed full GET waits for
	// hashstore.Adopt before deciding not to write a URL variant.
	activePublishTimeout = 60 * time.Second
)

// errActivePartialFirst means a partial plan reached the shared cache before the
// full body was taken. Failing only that plan lets the full reader proceed; the
// partial reader falls back to the ordinary chain.
var errActivePartialFirst = errors.New("relay active: partial section before full body")

// activeMetadata is what the creator shares with later readers through
// Entry.Metadata. It records the immutable part of the upstream response: the
// headers a delivery may replay, the headers the committed variant persists (the
// two differ when the response declared a persistent lifetime), the object size
// (-1 when unknown), and the freshness the resulting URL variant should carry.
type activeMetadata struct {
	headers http.Header
	// stored is the header set a committed URL variant uses. A response that
	// declared X-MDB-Relay: immutable keeps the persistent lifetime here while
	// headers keeps the upstream value for delivery, so the first client sees
	// what the origin sent.
	stored     http.Header
	persistent bool
	size       int64
	// urlKey is the cache key of the request that seeded the entry. A joiner on
	// another URL may read the shared bytes, but its response headers came from
	// the creator's response, so it must not write them as a variant for its own
	// URL. Only the same metadata scope may commit.
	urlKey    string
	storedAt  time.Time
	expiresAt time.Time
}

// logActiveFallback keeps an expected admission refusal from looking like a
// relay error. Capacity, the disk watermark and a closed manager all mean the
// request takes the ordinary chain; only an unexpected failure is logged.
func logActiveFallback(stage, key string, err error) {
	if err == nil ||
		errors.Is(err, activecache.ErrCapacity) ||
		errors.Is(err, activecache.ErrLowDiskSpace) ||
		errors.Is(err, activecache.ErrClosed) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return
	}
	log.Printf("relay active: %s %q falls back to the ordinary chain: %v", stage, key, err)
}

// serveImmutableActive assembles or reuses the active entry for an immutable
// GET. It returns false only when the caller must use the ordinary chain; every
// failure before response headers have been written is converted into that
// fallback.
func (s *Server) serveImmutableActive(w http.ResponseWriter, r *http.Request, key, path string, digest contentDigest) bool {
	if s.active == nil {
		return false
	}

	activeKey, _, digestHex := activeKeyFor(key, path, digest)
	entry, created, err := s.active.Acquire(r.Context(), activeKey)
	if err != nil {
		logActiveFallback("acquire", activeKey, err)
		return false
	}
	defer entry.Release()

	if created {
		// The upstream's own validator decides a conditional request. Creating
		// an entry from a 304 or from a representation selected by the client's
		// headers would invent an identity the upstream never stated.
		if hasConditionalHeaders(r) {
			entry.Fail(nil)
			return false
		}
		// A policy digest is the pre-request fact and wins over anything the
		// response declares; only a pattern without a digest lets the response
		// supply the Adopt expectation.
		return s.seedImmutableActive(w, r, key, path, entry, digestHex == "")
	}

	meta, ok := entry.Metadata().(activeMetadata)
	if !ok {
		log.Printf("relay active: entry %q has no metadata", activeKey)
		return false
	}
	return s.deliverImmutableActive(w, r, key, path, entry, meta)
}

type activeSeedOutcome int

const (
	// activeSeedUseEntry: Init succeeded; serve from the shared cache.
	activeSeedUseEntry activeSeedOutcome = iota
	// activeSeedOrdinary: nothing was consumed; the same response must be
	// delivered through the ordinary chain instead of fetching again.
	activeSeedOrdinary
	// activeSeedFallback: the response is no longer deliverable here; the caller
	// must fetch again or answer with an error.
	activeSeedFallback
)

// seedImmutableActive fetches the first response for a policy-seeded entry and
// hands it to the disposition module. It reports whether the request is
// finished; false tells the caller to start a fresh upstream exchange.
func (s *Server) seedImmutableActive(w http.ResponseWriter, r *http.Request, key, path string, entry *activecache.Entry, useResponseDigest bool) bool {
	// The body's lifetime is the shared cache's, not the creator's: a client
	// that disconnects must not take away bytes other readers are waiting for.
	// Detaching removes the client's cancellation; the transport still bounds
	// dial, TLS and response-header waits, and Manager.Close closes the body
	// when the entry retires.
	seedCtx := context.WithoutCancel(r.Context())

	out, err := s.buildUpstreamRequest(seedCtx, r)
	if err != nil {
		entry.Fail(err)
		log.Printf("relay active: request %q: %v", key, err)
		s.writeUpstreamError(w, r, err)
		return true
	}
	// Byte offsets address the encoded representation. The adapter enforces
	// this for later requests; the seed request has to do it itself.
	out.Header.Set("Accept-Encoding", "identity")
	// The adapter consumes one range response. A client's multipart Range must
	// not be forwarded as-is: seed with its first interval, and the remaining
	// intervals are fetched by the cache when their readers ask for them. A
	// non-bytes unit has no usable interval and is treated as a full GET.
	seedRange := activeSeedRange(r.Header.Get("Range"))
	if seedRange != "" {
		out.Header.Set("Range", seedRange)
	} else {
		out.Header.Del("Range")
	}

	resp, err := s.roundTrip(out)
	if err != nil {
		entry.Fail(err)
		s.writeUpstreamError(w, r, err)
		return true
	}
	return s.disposition().settle(w, r, dispositionRequest{
		key:      key,
		path:     path,
		resp:     resp,
		writable: true,
		policySeed: &policySeed{
			entry:             entry,
			seedRange:         seedRange,
			useResponseDigest: useResponseDigest,
		},
	}) == settleDone
}

// startActiveFromResponse validates an already-fetched response and hands it to
// the entry when it can be assembled. Every refusal before the adapter is built
// leaves the response untouched, so the caller can deliver it through the
// ordinary chain without a second upstream request. seedRange is the request
// range this response answers when the caller narrowed a multipart Range itself.
// useResponseDigest lets a response supply the Adopt expectation when no policy
// pattern captured one.
func (s *Server) startActiveFromResponse(r *http.Request, key string, entry *activecache.Entry, resp *http.Response, seedRange string, useResponseDigest bool) (activeMetadata, activeSeedOutcome) {
	directives := RelayDirectivesFromHeader(resp.Header)

	if err := immutableActiveStorable(resp, directives); err != nil {
		entry.Fail(err)
		// A 404/416 is the upstream's answer, not a relay defect. For a
		// multipart request the seed asked only whose first interval, so its 416
		// says nothing about the intervals the client actually named: the
		// original request must be evaluated by the ordinary chain.
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && rangeHasMultipleIntervals(r.Header.Get("Range")) {
			_ = resp.Body.Close()
			return activeMetadata{}, activeSeedFallback
		}
		logActiveSeedFallback(key, resp.StatusCode, err)
		return activeMetadata{}, activeSeedOrdinary
	}

	if resp.StatusCode == http.StatusOK && seedRange != "" {
		// The origin ignored Range. The pending body is the complete object, so
		// it is an ordinary cacheable 200: the normal chain delivers and stores
		// it, while the entry is not a byte-addressed assembly.
		entry.Fail(errors.New("200 response to a ranged request"))
		return activeMetadata{}, activeSeedOrdinary
	}

	size := immutableActiveSize(resp)
	if resp.StatusCode == http.StatusPartialContent && size < 0 {
		// A 206 with "Content-Range: bytes a-b/*" states no object extent, so the
		// assembly could never publish and a later Range could not be bounded.
		entry.Fail(errors.New("206 without a total size"))
		return activeMetadata{}, activeSeedOrdinary
	}
	if size <= 0 {
		// An unknown length cannot be reserved up front, and a zero-length body
		// has nothing to assemble. The ordinary chain delivers it.
		entry.Fail(errors.New("unknown or zero-length immutable object"))
		return activeMetadata{}, activeSeedOrdinary
	}
	if budget := s.hashStore.Stats().Capacity; budget > 0 && size > budget {
		// Adopt would reject this body; admitting it would only hold an upstream
		// response and a scratch file for nothing.
		entry.Fail(fmt.Errorf("body of %d bytes exceeds the %d-byte store budget", size, budget))
		return activeMetadata{}, activeSeedOrdinary
	}

	// A response-declared digest is only an Adopt expectation and only when the
	// body is the whole representation: a 206 describes a slice, so its digest
	// cannot name the object, and a non-identity coding is already refused.
	if useResponseDigest && resp.StatusCode == http.StatusOK {
		if algo, digest := directives.Expectation(); algo != "" {
			if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding == "" || strings.EqualFold(encoding, "identity") {
				if err := entry.SetExpectation(algo, digest); err != nil {
					log.Printf("relay active: response digest for %q ignored: %v", key, err)
				}
			}
		}
	}

	storedAt := s.currentTime()
	headers := immutableActiveHeaders(resp.Header)
	stored := headers
	persistent := directives.Immutable
	if persistent {
		// The stored variant declares a persistent lifetime; the delivery keeps
		// the upstream Cache-Control, exactly like the ordinary fallback path.
		stored = headers.Clone()
		withPersistentLifetime(stored)
	}
	meta := activeMetadata{
		headers:    headers,
		stored:     stored,
		persistent: persistent,
		size:       size,
		urlKey:     key,
		storedAt:   storedAt,
		expiresAt:  freshnessExpiry(stored, directives, storedAt),
	}

	// Later range requests must carry the relay host and the selected transport,
	// but never the creator's cookies or credentials. The adapter inherits
	// request headers from resp.Request, so replace that view before handing it
	// the body.
	resp.Request = activeAdapterRequest(resp.Request)

	opts := make([]httpadapter.Option, 0, 1)
	if rt := s.upstreamTransport(); rt != nil {
		opts = append(opts, httpadapter.WithTransport(rt))
	}

	// Admission and the tiered cache are ready before the adapter takes the
	// response body. When they fail, the fetched response is still untouched and
	// is delivered (and stored) by the ordinary chain, so a full segment or a
	// low disk never costs a second upstream read.
	builderCalled := false
	err := entry.InitFactory(meta, size, func() (tiered.UpstreamProvider, error) {
		builderCalled = true
		adapter, err := httpadapter.NewUpstreamFromResponse(resp, opts...)
		if err != nil {
			return nil, err
		}
		return &activeSource{
			upstream: adapter,
			entry:    entry,
			policy:   policyDecisionFromContext(r.Context()),
			first200: resp.StatusCode == http.StatusOK,
			size:     size,
		}, nil
	})
	if err != nil {
		if builderCalled {
			log.Printf("relay active: start entry %q: %v", key, err)
			return activeMetadata{}, activeSeedFallback
		}
		logActiveFallback("init", key, err)
		return activeMetadata{}, activeSeedOrdinary
	}
	return meta, activeSeedUseEntry
}

// responseImmutableEligible decides whether an already-fetched response may seed
// the active segment because it declared X-MDB-Relay: immutable. It only
// inspects request and response facts; nothing here fetches. A policy-declared
// immutable path already made this decision before the request and keeps its own
// key and digest, so it is excluded.
func (s *Server) responseImmutableEligible(r *http.Request, resp *http.Response, directives RelayDirectives, old *hashstore.Variant, writable, cleanupAmbiguous bool) bool {
	if s.active == nil || s.hashStore == nil || resp == nil || resp.Body == nil {
		return false
	}
	if r.Method != http.MethodGet || !writable || old != nil || cleanupAmbiguous {
		return false
	}
	if !directives.Immutable {
		// A digest alone never selects the active path.
		return false
	}
	if immutable, _ := immutableFromContext(r.Context()); immutable {
		return false
	}
	if policy := requestCachePolicyFrom(r); policy.noStore || policy.forceRevalidate {
		return false
	}
	// The assembly is an identity byte range. A client that refuses identity
	// cannot be served from it.
	if !AcceptEncodingFromRequest(r).Accepts(CodingIdentity) {
		return false
	}
	if hasConditionalHeaders(r) {
		return false
	}
	// A multipart range cannot seed one assembly whose extent the origin states
	// in a way the client asked for.
	if rangeHasMultipleIntervals(r.Header.Get("Range")) {
		return false
	}
	switch resp.StatusCode {
	case http.StatusOK:
		// An origin that answers a range request with the full body keeps the
		// ordinary writable path, which stores it as a complete representation.
		if strings.TrimSpace(r.Header.Get("Range")) != "" {
			return false
		}
	case http.StatusPartialContent:
		// A 206 with a concrete total size can seed a byte-addressed assembly.
		// The adapter would reject a body whose Content-Length disagrees with its
		// Content-Range, and that rejection is not recoverable there (the adapter
		// closes the body), so the mismatch is refused here and the same response
		// is delivered ordinarily.
		start, end, _, totalKnown, ok := parseActiveContentRange(resp.Header.Get("Content-Range"))
		if !ok || !totalKnown {
			return false
		}
		if resp.ContentLength >= 0 && resp.ContentLength != end-start+1 {
			return false
		}
	default:
		return false
	}
	// The response-side prohibitions are checked here too, not only in the
	// seed: a response the active segment can never take must go straight to
	// the ordinary chain instead of being acquired and logged as a fallback.
	return immutableActiveStorable(resp, directives) == nil
}

// responseSeedOutcome reports whether a response-declared seed consumed the
// response or left it for the ordinary chain.
type responseSeedOutcome int

const (
	// responseSeedOrdinary: the response is untouched and must be delivered by
	// the ordinary chain.
	responseSeedOrdinary responseSeedOutcome = iota
	// responseSeedConsumed: the response was assembled or the client was answered
	// with an error; nothing is left to deliver.
	responseSeedConsumed
)

// seedResponseActive seeds the active segment from a first MISS whose response
// declared immutability. bodyTaken reports whether the entry (or a failed
// adapter) now owns the body; the caller must not deliver it again. A refusal
// before the handoff returns responseSeedOrdinary with bodyTaken false, so the
// same response can still take the ordinary path.
func (s *Server) seedResponseActive(w http.ResponseWriter, r *http.Request, key string, resp *http.Response, directives RelayDirectives) (responseSeedOutcome, bool) {
	// The response digest is an expectation only: the entry keeps the URL key
	// the response arrived under, so a response can never re-point a cache key.
	entry, created, err := s.active.Acquire(r.Context(), key)
	if err != nil {
		logActiveFallback("response-acquire", key, err)
		return responseSeedOrdinary, false
	}
	defer entry.Release()
	if !created {
		// Another request already owns this URL key. This body is in hand, so it
		// goes to the ordinary chain rather than into someone else's assembly;
		// the existing entry is left untouched.
		return responseSeedOrdinary, false
	}

	meta, outcome := s.startActiveFromResponse(r, key, entry, resp, "", true)
	switch outcome {
	case activeSeedUseEntry:
		if s.deliverImmutableActive(w, r, key, "", entry, meta) {
			return responseSeedConsumed, true
		}
		log.Printf("relay active: response seed %q could not be delivered", key)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return responseSeedConsumed, true
	case activeSeedOrdinary:
		return responseSeedOrdinary, false
	default:
		// The body was handed to an adapter that could not start, so it cannot be
		// delivered here. Nothing has been written to the client yet.
		log.Printf("relay active: response seed %q left no deliverable body", key)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return responseSeedConsumed, true
	}
}

// logActiveSeedFallback downgrades the statuses that are simply the upstream's
// answer. Everything else is a response the active segment cannot represent.
func logActiveSeedFallback(key string, status int, err error) {
	switch status {
	case http.StatusNotFound, http.StatusGone, http.StatusRequestedRangeNotSatisfiable, http.StatusNotModified:
		return
	}
	log.Printf("relay active: %q falls back to the ordinary chain: %v", key, err)
}

// activeSeedRange keeps the first interval of a Range field. It returns "" for
// a non-bytes unit or an unusable field, which makes the seed a full GET.
func activeSeedRange(value string) string {
	value = strings.TrimSpace(value)
	unit, spec, found := strings.Cut(value, "=")
	if !found || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return ""
	}
	first, _, _ := strings.Cut(spec, ",")
	first = strings.TrimSpace(first)
	if first == "" {
		return ""
	}
	return "bytes=" + first
}

// rangeHasMultipleIntervals reports whether a bytes Range field names more than
// one interval. A 416 for a seed that forwarded only the first interval cannot
// stand in for the client's original multipart request.
func rangeHasMultipleIntervals(value string) bool {
	unit, spec, found := strings.Cut(strings.TrimSpace(value), "=")
	if !found || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return false
	}
	_, rest, found := strings.Cut(spec, ",")
	return found && strings.TrimSpace(rest) != ""
}

// immutableActiveStorable applies the response-side prohibitions. They are the
// same facts the ordinary chain checks, plus the identity and Content-Range
// requirements of a byte-addressed assembly cache.
func immutableActiveStorable(resp *http.Response, directives RelayDirectives) error {
	if resp.Body == nil {
		return errors.New("no body")
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if resp.Header.Get("Content-Range") != "" {
			return fmt.Errorf("200 carries Content-Range %q", resp.Header.Get("Content-Range"))
		}
	case http.StatusPartialContent:
		if _, _, _, _, ok := parseActiveContentRange(resp.Header.Get("Content-Range")); !ok {
			return fmt.Errorf("invalid Content-Range %q", resp.Header.Get("Content-Range"))
		}
	default:
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if directives.NoStore {
		return errors.New("X-MDB-Relay: no-store")
	}
	if cacheControlHas(resp.Header.Values("Cache-Control"), "no-store") {
		return errors.New("Cache-Control: no-store")
	}
	if cacheControlHas(resp.Header.Values("Cache-Control"), "private") {
		return errors.New("Cache-Control: private")
	}
	// A shared byte cache has one representation per object. Vary other than
	// Accept-Encoding would let one request's body be served to a reader that
	// asked for another; Accept-Encoding is irrelevant because the body is
	// always identity.
	if vary := withoutAcceptEncoding(varyNames(resp.Header.Values("Vary"))); len(vary) != 0 {
		return fmt.Errorf("Vary %q", strings.Join(vary, ","))
	}
	encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return fmt.Errorf("Content-Encoding %q", encoding)
	}
	return nil
}

// immutableActiveSize returns the complete object size the first response
// states, or -1 when it does not state one.
func immutableActiveSize(resp *http.Response) int64 {
	switch resp.StatusCode {
	case http.StatusOK:
		if resp.ContentLength >= 0 {
			return resp.ContentLength
		}
		return -1
	case http.StatusPartialContent:
		_, _, total, totalKnown, ok := parseActiveContentRange(resp.Header.Get("Content-Range"))
		if !ok || !totalKnown {
			return -1
		}
		return total
	default:
		return -1
	}
}

// parseActiveContentRange parses a bytes Content-Range with a concrete span.
// A 206 never carries an unsatisfied range (that is a 416), so "bytes *\/N"
// is rejected.
func parseActiveContentRange(value string) (start, end, total int64, totalKnown, ok bool) {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "bytes") {
		return 0, 0, 0, false, false
	}
	span, rawTotal, found := strings.Cut(fields[1], "/")
	if !found {
		return 0, 0, 0, false, false
	}
	rawTotal = strings.TrimSpace(rawTotal)
	if rawTotal != "*" {
		parsed, err := strconv.ParseInt(rawTotal, 10, 64)
		if err != nil || parsed < 0 {
			return 0, 0, 0, false, false
		}
		total, totalKnown = parsed, true
	}
	span = strings.TrimSpace(span)
	if span == "*" {
		// A 206 never carries an unsatisfied range; that is a 416.
		return 0, 0, 0, false, false
	}
	rawStart, rawEnd, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, 0, false, false
	}
	parsedStart, startErr := strconv.ParseInt(strings.TrimSpace(rawStart), 10, 64)
	parsedEnd, endErr := strconv.ParseInt(strings.TrimSpace(rawEnd), 10, 64)
	if startErr != nil || endErr != nil || parsedStart < 0 || parsedEnd < parsedStart {
		return 0, 0, 0, false, false
	}
	if totalKnown && parsedEnd >= total {
		return 0, 0, 0, false, false
	}
	return parsedStart, parsedEnd, total, totalKnown, true
}

// immutableActiveHeaders keeps the end-to-end headers a later delivery may
// state. Content-Range belongs to the first response only; every delivery
// computes its own.
func immutableActiveHeaders(h http.Header) http.Header {
	out := storageHeaders(h)
	out.Del("Content-Range")
	return out
}

// activeAdapterRequest replaces the request the HTTP adapter inherits its
// headers from. It keeps only the minimal safe set: the relay host for absolute
// URL generation, the user agent, and Accept. Cookies, Authorization and the
// originating X-Forwarded-* chain never reach a later range request.
func activeAdapterRequest(in *http.Request) *http.Request {
	out := &http.Request{Method: http.MethodGet, Header: make(http.Header)}
	if in == nil {
		return out
	}
	out.URL = in.URL
	for _, name := range []string{"User-Agent", "Accept", RelayHostHeader} {
		if value := in.Header.Get(name); value != "" {
			out.Header.Set(name, value)
		}
	}
	return out
}

// activeKeyFor returns the manager key plus the digest expectation. A digest
// that does not normalise is not a content address: the path is still immutable,
// but deduplication falls back to the URL key and Adopt publishes whatever the
// body really is. An OCI manifest is URL-keyed even when the path names a
// digest: two repositories can address the same manifest bytes, but a manifest's
// media type and headers are repository-specific metadata, not a property of the
// digest. Blob and APT paths keep the global digest key.
func activeKeyFor(urlKey, path string, d contentDigest) (string, hashstore.Algorithm, string) {
	if d.Algorithm == "" || isOCIManifestPath(path) {
		return urlKey, "", ""
	}
	algo, err := hashstore.ParseAlgorithm(d.Algorithm)
	if err != nil {
		return urlKey, "", ""
	}
	normalized, err := hashstore.NormalizeDigest(algo, d.Hex)
	if err != nil {
		return urlKey, "", ""
	}
	return string(algo) + ":" + normalized, algo, normalized
}

// isOCIManifestPath reports whether a mount-relative path addresses a manifest
// rather than a blob. Manifests are media-type-bearing metadata, so the cache
// must not treat one repository's manifest response as a synthetic generic
// representation for another repository's digest.
func isOCIManifestPath(path string) bool {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	return len(segments) >= 2 && segments[len(segments)-2] == "manifests"
}

// activeSource adapts the official HTTP adapter to tiered.UpstreamProvider.
// Three things the adapter cannot know on its own:
//
//   - every request must select the same transport pool as the client request;
//     the tiered cache calls the provider with its own context;
//   - a pending 200 body is the whole representation and is reused even when
//     the origin does not advertise byte ranges. The adapter's ReadSection
//     discards such a body for an origin without Accept-Ranges, but its Open
//     reuses it, so the first full plan runs through Open. Later range plans
//     still go to the adapter, which issues real range requests;
//   - an upstream body failure is terminal for the shared entry. It is reported
//     here, from the source goroutine, rather than waiting for a client read to
//     surface it: a broken entry must leave the active index at once, so a
//     later request falls back before writing response headers.
type activeSource struct {
	upstream *httpadapter.Upstream
	entry    *activecache.Entry
	policy   policyDecision
	first200 bool
	size     int64

	mu       sync.Mutex
	tookBody bool
}

var _ tiered.UpstreamProvider = (*activeSource)(nil)
var _ io.Closer = (*activeSource)(nil)

func (s *activeSource) ReadSection(ctx context.Context, offset, size int64, extents chan<- tiered.Extent) (io.ReadCloser, error) {
	ctx = withPolicyDecision(ctx, s.policy)

	var (
		reader  io.ReadCloser
		err     error
		byRange bool
	)
	if s.first200 {
		s.mu.Lock()
		first := !s.tookBody
		if first && offset == 0 {
			// Only the full plan takes the pending body. A partial plan that
			// loses the race reports errActivePartialFirst and leaves the body
			// for the full reader.
			s.tookBody = true
		}
		s.mu.Unlock()
		if first {
			if s.size >= 0 && offset >= s.size {
				sendActiveExtent(ctx, extents, s.size)
				return nil, s.terminal(ctx, fmt.Errorf("relay active: section [%d,%d) outside extent %d: %w",
					offset, offset+size, s.size, tiered.ErrOutOfRange))
			}
			if offset != 0 {
				// A partial plan raced ahead of the full reader. Taking the
				// pending full body here would discard everything before its
				// offset, so only this plan fails; the full reader still gets
				// the body.
				return nil, errActivePartialFirst
			}
			reader, err = s.upstream.Open(ctx, extents)
		} else {
			reader, err = s.upstream.ReadSection(ctx, offset, size, extents)
			byRange = true
		}
	} else {
		reader, err = s.upstream.ReadSection(ctx, offset, size, extents)
		byRange = true
	}
	if err != nil && byRange && activeRangeFallbackEligible(err) {
		// The range attempt failed before it handed over a single byte: the
		// origin ignored Range, answered an error, or was unreachable. A
		// complete GET can still produce this section, so the entry must not be
		// failed on the strength of the range answer alone. Nothing has been
		// delivered to this section yet, so re-reading from the object start is
		// safe and cannot duplicate bytes.
		reader, err = s.openSectionFromFullGet(ctx, offset, size, extents)
	}
	if err != nil {
		return nil, s.terminal(ctx, err)
	}
	if reader == nil {
		return nil, nil
	}
	return &notifyingReader{ReadCloser: reader, source: s, ctx: ctx}, nil
}

// activeRangeFallbackEligible reports whether a failed range attempt may be
// replaced by a complete GET. Cancellation, a closed adapter, an out-of-range
// decision and a lost race for the pending body all describe the caller rather
// than the object, so they keep their meaning and are never retried.
func activeRangeFallbackEligible(err error) bool {
	return err != nil &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, httpadapter.ErrClosed) &&
		!errors.Is(err, tiered.ErrOutOfRange) &&
		!errors.Is(err, tiered.ErrInvalidRange) &&
		!errors.Is(err, errActivePartialFirst)
}

// openSectionFromFullGet serves one section from a complete GET. The origin has
// already refused to serve a byte range, so the only honest way to fill a hole
// is to stream the object from its start and discard the prefix. The prefix is
// never buffered and never reaches the cache: the tiered core only sees the
// bytes this reader returns, which are exactly size bytes at offset.
func (s *activeSource) openSectionFromFullGet(ctx context.Context, offset, size int64, extents chan<- tiered.Extent) (io.ReadCloser, error) {
	if offset < 0 || size < 0 {
		return nil, fmt.Errorf("relay active: full GET section offset=%d size=%d: %w", offset, size, tiered.ErrInvalidRange)
	}
	if s.size >= 0 {
		if offset >= s.size {
			sendActiveExtent(ctx, extents, s.size)
			return nil, fmt.Errorf("relay active: full GET section [%d,%d) outside extent %d: %w",
				offset, offset+size, s.size, tiered.ErrOutOfRange)
		}
		if size > s.size-offset {
			return nil, fmt.Errorf("relay active: full GET section [%d,%d) past extent %d: %w",
				offset, offset+size, s.size, tiered.ErrInvalidRange)
		}
	}
	reader, err := s.upstream.Open(ctx, extents)
	if err != nil {
		return nil, fmt.Errorf("relay active: full GET for section [%d,%d): %w", offset, offset+size, err)
	}
	if reader == nil {
		return nil, fmt.Errorf("relay active: full GET for section [%d,%d): upstream returned no body", offset, offset+size)
	}
	return &fullGetSection{body: reader, skip: offset, remaining: size}, nil
}

// fullGetSection turns a complete GET body into exactly the requested section by
// discarding the prefix while streaming. A body that ends before the section is
// complete is a malformed response (io.ErrUnexpectedEOF), never a short section.
type fullGetSection struct {
	body      io.ReadCloser
	skip      int64
	remaining int64

	closeOnce sync.Once
	closeErr  error
}

func (r *fullGetSection) Read(p []byte) (int, error) {
	if r.skip > 0 {
		skipped, err := io.CopyN(io.Discard, r.body, r.skip)
		r.skip -= skipped
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, fmt.Errorf("relay active: full GET ended before section offset: %w", io.ErrUnexpectedEOF)
			}
			return 0, err
		}
	}
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.body.Read(p)
	r.remaining -= int64(n)
	if errors.Is(err, io.EOF) && r.remaining > 0 {
		return n, fmt.Errorf("relay active: full GET ended with %d section bytes unread: %w", r.remaining, io.ErrUnexpectedEOF)
	}
	return n, err
}

func (r *fullGetSection) Close() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.body.Close()
	})
	return r.closeErr
}

func (s *activeSource) Close() error {
	return s.upstream.Close()
}

func sendActiveExtent(ctx context.Context, extents chan<- tiered.Extent, end int64) {
	if extents == nil {
		return
	}
	select {
	case extents <- tiered.Extent{End: end}:
	case <-ctx.Done():
	}
}

// terminal reports a source failure to the entry. A cancellation or an
// adapter shutdown is not evidence about the object; everything else is: the
// bytes behind this entry cannot be completed by another reader, so keeping the
// entry for the idle timeout would only serve failures.
func (s *activeSource) terminal(ctx context.Context, err error) error {
	if err == nil ||
		(ctx != nil && ctx.Err() != nil) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, httpadapter.ErrClosed) {
		return err
	}
	if s.entry != nil {
		s.entry.Fail(err)
	}
	return err
}

// notifyingReader reports an asynchronous body failure (an early EOF, a broken
// range response, an extent conflict) to the entry while the tiered core is
// still draining it. A canceled plan is not evidence about the object: the core
// cancels an upstream whenever its readers leave.
type notifyingReader struct {
	io.ReadCloser
	source *activeSource
	ctx    context.Context
}

func (r *notifyingReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		_ = r.source.terminal(r.ctx, err)
	}
	return n, err
}

// activeRepresentationSource serves an immutable assembly. It is the only
// tier-specific part of an active delivery: the tiered entry can serve the
// complete extent or any section of it, and reports an interval outside its
// extent as a 416 rather than as a fallback.
type activeRepresentationSource struct {
	entry *activecache.Entry
	size  int64
}

var _ representationSource = activeRepresentationSource{}

func (s activeRepresentationSource) probe(ctx context.Context) error { return nil }

func (s activeRepresentationSource) open(ctx context.Context) (io.ReadCloser, error) {
	if s.size >= 0 {
		return s.entry.ReadSection(ctx, 0, s.size)
	}
	// An entry admitted without a declared extent is read whole. It is never
	// exposed as a complete body before publication, but the source does not
	// pretend to know its size.
	return s.entry.Read(ctx)
}

func (s activeRepresentationSource) openSection(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	rc, err := s.entry.ReadSection(ctx, offset, length)
	if errors.Is(err, tiered.ErrOutOfRange) {
		return nil, errRangeUnsatisfiable
	}
	return rc, err
}

// deliverImmutableActive serves a GET from an entry that is ready, whether it
// is complete, partially filled, or already published. HTTP representation
// semantics come from deliverRepresentation; this function owns only the
// active tier's body acquisition, its failure classification and its
// publication follow-up.
func (s *Server) deliverImmutableActive(w http.ResponseWriter, r *http.Request, key, path string, entry *activecache.Entry, meta activeMetadata) bool {
	if r.Method != http.MethodGet {
		return false
	}

	res := deliverRepresentation(w, r, representation{
		validators:      meta.headers,
		header:          representationHeaders(meta.headers, meta.storedAt, s.currentTime()),
		size:            meta.size,
		acceptRanges:    true,
		trackCopyErrors: true,
		source:          activeRepresentationSource{entry: entry, size: meta.size},
		label:           key,
	})
	if res.Delivered {
		s.activeDeliveries.Add(1)
		if res.ReadErr != nil {
			if !res.Multipart && r.Context().Err() == nil {
				log.Printf("relay active: stream %q: %v", key, res.ReadErr)
			}
			if res.ClientErr == nil {
				// The reader failed, not the client: the entry cannot complete
				// this object, and a later request must not reuse the hole.
				failActiveDelivery(entry, res.ReadErr)
			}
		}
		if res.BodyComplete {
			switch {
			case res.WholeRepresentation:
				s.commitActiveAfterDelivery(r, key, entry, meta, true)
			case res.Multipart:
				// A partial union never publishes, but the entry may already
				// have been published by another reader.
				s.commitActiveAfterDelivery(r, key, entry, meta, false)
			}
		}
		return true
	}
	if res.Err == nil {
		// A Range without a declared extent cannot be evaluated here; the
		// ordinary chain forwards the request.
		return false
	}
	s.activeReadFailed(r, key, entry, res.Err)
	return false
}

// failActiveDelivery fails an entry after a copy error that came from the
// reader. A client that went away cancels its downstream context; that says
// nothing about the shared bytes and must not evict a healthy entry.
func failActiveDelivery(entry *activecache.Entry, err error) {
	if err == nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, activecache.ErrClosed) ||
		errors.Is(err, errActivePartialFirst) ||
		errors.Is(err, errRangeUnsatisfiable) ||
		errors.Is(err, tiered.ErrOutOfRange) ||
		errors.Is(err, tiered.ErrTooManyDownstreams) ||
		errors.Is(err, tiered.ErrTooManyUpstreams) {
		return
	}
	entry.Fail(err)
}

// activeReadFailed records a cache read that failed before any header was
// written. A dead entry is failed so later requests do not pick it up again;
// a client cancellation, a satisfiable-range decision or a scheduling limit is
// not the entry's fault. Every other error is terminal: an entry that cannot
// produce the bytes its extent promises must leave the index now, not after the
// idle timeout.
func (s *Server) activeReadFailed(r *http.Request, key string, entry *activecache.Entry, err error) {
	if r.Context().Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if errors.Is(err, tiered.ErrOutOfRange) || errors.Is(err, errRangeUnsatisfiable) ||
		errors.Is(err, tiered.ErrTooManyDownstreams) ||
		errors.Is(err, tiered.ErrTooManyUpstreams) || errors.Is(err, errActivePartialFirst) {
		return
	}
	if errors.Is(err, activecache.ErrCapacity) ||
		errors.Is(err, activecache.ErrLowDiskSpace) ||
		errors.Is(err, activecache.ErrClosed) {
		entry.Fail(err)
		return
	}
	log.Printf("relay active: read %q: %v", key, err)
	entry.Fail(err)
}

// commitActiveAfterDelivery waits for (wait) or observes publication, then
// writes the ordinary URL variant. Nothing is written unless Adopt accepted the
// complete body at the exact declared size, so a digest mismatch or a failed
// handoff can never leave metadata naming bytes the store does not have.
func (s *Server) commitActiveAfterDelivery(r *http.Request, key string, entry *activecache.Entry, meta activeMetadata, wait bool) {
	// The entry's headers describe the response of the request that seeded it.
	// A joiner on another URL (the same digest under another mirror or
	// repository) may read the shared bytes, but committing those headers as its
	// own variant would invent metadata its upstream never sent. Only the same
	// URL/metadata scope writes a variant.
	if meta.urlKey != "" && meta.urlKey != key {
		return
	}
	var (
		content hashstore.Content
		ok      bool
	)
	if wait {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), activePublishTimeout)
		published, err := entry.WaitPublished(ctx)
		cancel()
		if err != nil {
			if r.Context().Err() == nil &&
				!errors.Is(err, activecache.ErrCapacity) &&
				!errors.Is(err, activecache.ErrLowDiskSpace) {
				log.Printf("relay active: publish %q: %v", key, err)
			}
			return
		}
		content, ok = published, true
	} else {
		content, ok = entry.Content()
	}
	if !ok {
		return
	}
	if meta.size >= 0 && content.Size != meta.size {
		return
	}
	stored := meta.stored
	if stored == nil {
		stored = meta.headers
	}
	v := hashstore.Variant{
		URL:             key,
		NoVary:          true,
		ResponseHeaders: stored,
		BodySHA256:      content.SHA256,
		BodySize:        content.Size,
		StoredAt:        meta.storedAt,
		ExpiresAt:       meta.expiresAt,
		Persistent:      meta.persistent,
	}
	_ = s.commitVariant(r.Context(), v, nil)
}

// serveImmutableDigestHit answers a digest-addressed immutable path from the
// content store without a URL variant. Any mount that cached those bytes can
// serve them: the digest names the content, not the URL.
func (s *Server) serveImmutableDigestHit(w http.ResponseWriter, r *http.Request, key, path string, digest contentDigest) bool {
	if digest.Algorithm == "" {
		return false
	}
	algo, err := hashstore.ParseAlgorithm(digest.Algorithm)
	if err != nil {
		return false
	}
	normalized, err := hashstore.NormalizeDigest(algo, digest.Hex)
	if err != nil {
		return false
	}
	sha256Hex, sha1Hex, found, err := s.hashStore.Resolve(r.Context(), algo, normalized)
	if err != nil {
		log.Printf("relay active: resolve %s:%s: %v", algo, normalized, err)
		return false
	}
	if !found || sha256Hex == "" {
		return false
	}
	size, exists, err := s.hashStore.Stat(r.Context(), hashstore.SHA256, sha256Hex)
	if err != nil {
		log.Printf("relay active: stat %s: %v", sha256Hex, err)
		return false
	}
	if !exists || size <= 0 {
		return false
	}
	v := hashstore.Variant{
		URL:             key,
		NoVary:          true,
		BodySHA256:      sha256Hex,
		BodySize:        size,
		ResponseHeaders: immutableSyntheticHeaders(path, sha256Hex, sha1Hex),
		StoredAt:        s.currentTime(),
		ExpiresAt:       s.currentTime(),
	}
	return s.serveHit(w, r, key, path, v)
}

// serveImmutableStaleHit serves a stale variant when the merged immutable
// pattern says its bytes cannot have changed. Freshness has no meaning for such
// a path, but the body must still be present at the recorded size. A captured
// digest additionally has to name exactly the stored body.
func (s *Server) serveImmutableStaleHit(w http.ResponseWriter, r *http.Request, key, path string, v hashstore.Variant, digest contentDigest) bool {
	size, exists, err := s.hashStore.Stat(r.Context(), hashstore.SHA256, v.BodySHA256)
	if err != nil {
		log.Printf("relay active: stat stale %q: %v", key, err)
		return false
	}
	if !exists || size != v.BodySize {
		return false
	}
	if digest.Algorithm != "" {
		algo, err := hashstore.ParseAlgorithm(digest.Algorithm)
		if err != nil {
			return false
		}
		normalized, err := hashstore.NormalizeDigest(algo, digest.Hex)
		if err != nil {
			return false
		}
		primary, _, found, err := s.hashStore.Resolve(r.Context(), algo, normalized)
		if err != nil || !found || primary != v.BodySHA256 {
			return false
		}
	}
	return s.serveHit(w, r, key, path, v)
}

// immutableSyntheticHeaders builds the only headers a digest-only delivery may
// state. Nothing is copied from an upstream response, so the digest is the
// validator and the content type comes from the path.
func immutableSyntheticHeaders(path, sha256Hex, sha1Hex string) http.Header {
	h := make(http.Header)
	h.Set("Content-Type", predictedContentType(path))
	h.Set("Accept-Ranges", "bytes")
	if sha256Hex != "" {
		h.Set("ETag", `"sha256:`+sha256Hex+`"`)
	} else if sha1Hex != "" {
		h.Set("ETag", `"sha1:`+sha1Hex+`"`)
	}
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	if digest := digestHeader(sha256Hex, sha1Hex); digest != "" {
		h.Set("Digest", digest)
	}
	return h
}

// hasConditionalHeaders reports whether the first request depends on a
// validator, in which case the upstream must answer it rather than share a body
// assembled for someone else.
func hasConditionalHeaders(r *http.Request) bool {
	for _, name := range []string{
		"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range",
	} {
		if r.Header.Values(name) != nil {
			return true
		}
	}
	return false
}
