package server

import (
	"log"
	"net/http"
	"strings"

	"github.com/mirror-db/mirror-db/tools/relay/activecache"
	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// Response disposition (ADR 0011, 0014, 0016, 0017).
//
// One upstream response has exactly one disposition: deliver it untouched, file
// it as a stored variant, or assemble it into a hash object. This module owns
// that decision and the facts around it:
//
//   - the response-side store prohibitions and the active-admission rules;
//   - exactly one observation per response, in the shape that response
//     supports (headers for a body-bearing response, the folded variant for a
//     304);
//   - body ownership: the module closes the body unless an active entry or its
//     adapter took it;
//   - the fallback contract: a disposition that refuses before taking the body
//     delivers that same response through the ordinary chain, so a refusal
//     never asks the upstream again. A response that reached an adapter is
//     never replayed through the ordinary chain.
//
// The pure decision (chooseDisposition) is separate from execution so the
// decision table is testable without I/O. Callers only hand an already-fetched
// response to responseDisposer.settle; they never choose among store, assembly
// and raw themselves.

// responseDisposer settles one upstream response. It is the only seam the cache
// chain uses for that decision.
type responseDisposer interface {
	settle(w http.ResponseWriter, r *http.Request, req dispositionRequest) settleOutcome
}

// dispositioner implements responseDisposer for one server.
type dispositioner struct{ server *Server }

var _ responseDisposer = dispositioner{}

// disposition returns the module that settles upstream responses for this
// server. It is stateless, so every cache path can ask for it directly.
func (s *Server) disposition() responseDisposer { return dispositioner{server: s} }

// settleOutcome reports what the caller still has to do.
type settleOutcome int

const (
	// settleDone: the client response is finished (delivered, or an error was
	// written). The body is closed or owned by an active entry.
	settleDone settleOutcome = iota
	// settleUnusable: the body was handed to an adapter that could not start,
	// so it cannot be delivered here. Nothing has been written to the client; a
	// policy seed may start a fresh exchange, a response seed answers 502 (and
	// does so itself).
	settleUnusable
)

// dispositionRequest is one already-fetched upstream response plus the facts its
// disposition depends on.
type dispositionRequest struct {
	key  string
	path string
	resp *http.Response

	// old is the stale variant this response replaces or invalidates.
	old *hashstore.Variant
	// writable is false when the request must not populate the store (a client
	// Cache-Control: no-store exchange).
	writable bool
	// cleanupAmbiguous marks a refetch that resolves several matching variants:
	// it may store but must not seed an assembly.
	cleanupAmbiguous bool
	// allowResponseActive lets this response seed a URL-keyed active entry when
	// it declares immutability. It is false for a policy-seeded response and for
	// every fallback, so a refusal can never recurse.
	allowResponseActive bool
	// policySeed is the entry the policy path already acquired. Nil when the
	// request was not policy-immutable.
	policySeed *policySeed
	// revalidated marks a response fetched to revalidate a stale variant. A 304
	// folds metadata instead of handing a body off, and a 200 evaluates the
	// client's own preconditions before a body is stored.
	revalidated     bool
	revalidatedFrom *hashstore.Variant
}

// policySeed is an entry the policy path acquired before its first fetch.
type policySeed struct {
	entry             *activecache.Entry
	seedRange         string
	useResponseDigest bool
}

// dispositionInput is the pure input of the decision. The caller has already
// evaluated the response-side and eligibility predicates; nothing here reads
// server state.
type dispositionInput struct {
	revalidated        bool
	status             int
	clientPrecondition precondition
	// foldMetadata is true only when a 304 names the stored variant to fold. A
	// 304 without one is a cold response, not a metadata update.
	foldMetadata bool
	// writable and responseStorable are kept apart so the table can state why a
	// response is not stored: the request forbade it, or the response did.
	writable         bool
	responseStorable bool
	policySeed       bool
	responseSeed     bool
}

// dispositionPlan is the verdict for one response. A policy or response seed
// plan may still refuse during execution; the executor then settles the same
// response through planOrdinaryStore/planOrdinaryRaw.
type dispositionPlan int

const (
	planOrdinaryStore dispositionPlan = iota
	planOrdinaryRaw
	planPolicySeed
	planResponseSeed
	planRevalidatedMetadata
	planClientCondition
)

// chooseDisposition is the decision table. Order matters: a 304 never hands a
// body off, a revalidated 200 answers the client's own conditions before
// storage, and a policy seed precedes a response seed.
func chooseDisposition(in dispositionInput) dispositionPlan {
	switch {
	case in.revalidated && in.foldMetadata && in.status == http.StatusNotModified:
		return planRevalidatedMetadata
	case in.revalidated && in.status == http.StatusOK && in.clientPrecondition != preconditionProceed:
		return planClientCondition
	case in.policySeed:
		return planPolicySeed
	case in.responseSeed:
		return planResponseSeed
	case in.writable && in.responseStorable:
		return planOrdinaryStore
	default:
		return planOrdinaryRaw
	}
}

// settle decides and executes the disposition of one response.
func (d dispositioner) settle(w http.ResponseWriter, r *http.Request, req dispositionRequest) settleOutcome {
	resp := req.resp
	if resp == nil {
		// No response exists to deliver or store. Answer explicitly instead of
		// reporting a delivery that never happened.
		http.Error(w, "relay: disposition without an upstream response", http.StatusBadGateway)
		return settleDone
	}
	ownsBody := resp.Body != nil
	defer func() {
		if ownsBody {
			_ = resp.Body.Close()
		}
	}()

	s := d.server
	if s == nil {
		// A server-less disposition cannot decide or store; release the body and
		// answer explicitly rather than leaking it behind a false completion.
		http.Error(w, "relay: disposition without a server", http.StatusBadGateway)
		return settleDone
	}

	directives := RelayDirectivesFromHeader(resp.Header)
	precondition := preconditionProceed
	if req.revalidated && resp.StatusCode == http.StatusOK {
		precondition = evaluatePreconditions(r, resp.Header)
	}
	responseStorable := s.storable(resp, r, directives)
	storable := req.writable && responseStorable
	plan := chooseDisposition(dispositionInput{
		revalidated:        req.revalidated,
		status:             resp.StatusCode,
		clientPrecondition: precondition,
		foldMetadata:       req.revalidatedFrom != nil,
		writable:           req.writable,
		responseStorable:   responseStorable,
		policySeed:         req.policySeed != nil,
		responseSeed: req.policySeed == nil && req.allowResponseActive &&
			s.responseImmutableEligible(r, resp, directives, req.old, req.writable, req.cleanupAmbiguous),
	})

	if plan == planRevalidatedMetadata {
		s.settleRevalidatedMetadata(w, r, req, directives)
		return settleDone
	}
	// Exactly one observation per upstream response. A 304 uses the folded
	// metadata shape above; every other response is observed from its headers.
	s.observeHeaders(r, resp.Header)

	switch plan {
	case planClientCondition:
		s.settleClientCondition(w, r, resp, precondition)
		return settleDone
	case planPolicySeed:
		switch d.settlePolicySeed(w, r, req) {
		case activeSeedUseEntry:
			ownsBody = false
			return settleDone
		case activeSeedOrdinary:
			// The same response enters the ordinary chain below.
		default:
			// The adapter took the body and could not start.
			ownsBody = false
			return settleUnusable
		}
	case planResponseSeed:
		outcome, taken := s.seedResponseActive(w, r, req.key, resp, directives)
		if taken {
			ownsBody = false
		}
		if outcome == responseSeedConsumed {
			return settleDone
		}
	}

	s.settleOrdinary(w, r, req, directives, storable)
	return settleDone
}

// settleRevalidatedMetadata folds a 304 into the stored variant and delivers
// from the verified body. A 304 has no body to hand off, so it never enters the
// store, active or raw paths.
func (s *Server) settleRevalidatedMetadata(w http.ResponseWriter, r *http.Request, req dispositionRequest, directives RelayDirectives) {
	v := *req.revalidatedFrom
	next := s.revalidatedVariant(v, req.resp)
	if storableMetadata(next.ResponseHeaders, directives, next.NoVary) {
		if next.Persistent {
			withPersistentLifetime(next.ResponseHeaders)
		}
		_ = s.commitVariant(r.Context(), next, nil)
	} else {
		// The 304 verified this body but also declared it must no longer be
		// stored. Serve from the verified copy, drop the old record, and let the
		// next request start over as a MISS.
		s.dropVariant(v)
	}
	s.observeRevalidated(r, req.key, next, directives, revalidationNoStore(req.resp.Header, directives))
	if !s.serveHit(w, r, req.key, req.path, next) {
		s.proxy.ServeHTTP(w, r)
	}
}

// settleClientCondition answers a revalidated 200 with the client's own
// preconditions. The upstream body was fetched, so it is observed once, but the
// 304/412 answer stores nothing new.
func (s *Server) settleClientCondition(w http.ResponseWriter, r *http.Request, resp *http.Response, p precondition) {
	switch p {
	case preconditionNotModified:
		writeNotModifiedFromHeaders(w, resp.Header)
	case preconditionFailed:
		w.WriteHeader(http.StatusPreconditionFailed)
	}
}

// settlePolicySeed hands an already-fetched response to the policy-seeded entry.
// Every refusal before the adapter leaves the body untouched; activeSeedOrdinary
// then settles the same response through the ordinary chain.
func (d dispositioner) settlePolicySeed(w http.ResponseWriter, r *http.Request, req dispositionRequest) activeSeedOutcome {
	s := d.server
	seed := req.policySeed
	meta, outcome := s.startActiveFromResponse(r, req.key, seed.entry, req.resp, seed.seedRange, seed.useResponseDigest)
	if outcome != activeSeedUseEntry {
		return outcome
	}
	if s.deliverImmutableActive(w, r, req.key, req.path, seed.entry, meta) {
		return activeSeedUseEntry
	}
	// The entry stays available for later readers, but this request cannot be
	// served from it: the caller starts a fresh exchange instead.
	return activeSeedFallback
}

// settleOrdinary delivers the response, or tees it into the store when it is
// storable. It never observes: the caller already did.
func (s *Server) settleOrdinary(w http.ResponseWriter, r *http.Request, req dispositionRequest, directives RelayDirectives, storable bool) {
	resp := req.resp
	// A ranged request that received a full 200 means upstream ignored Range.
	// The full response may be cached, but the operator should be able to see
	// that a Range MISS turned into a full-body transfer.
	if r.Method == http.MethodGet && resp.StatusCode == http.StatusOK && strings.TrimSpace(r.Header.Get("Range")) != "" {
		log.Printf("relay cache warning: %s %s ignored Range and returned a full 200", r.Method, r.URL.Path)
	}

	if storable {
		s.streamAndStore(w, r, resp, req.key, directives, req.old, req.cleanupAmbiguous)
		return
	}
	if req.old != nil {
		switch resp.StatusCode {
		case http.StatusOK, http.StatusNotFound, http.StatusGone:
			// The stored copy is known stale and the upstream no longer agrees
			// with it. Nothing new may be written, so drop the old record.
			s.dropVariant(*req.old)
		}
	}
	deliverRaw(w, resp)
}

// revalidationNoStore reports whether a response forbids background storage.
func revalidationNoStore(header http.Header, directives RelayDirectives) bool {
	return directives.NoStore ||
		cacheControlHas(header.Values("Cache-Control"), "no-store") ||
		cacheControlHas(header.Values("Cache-Control"), "private")
}

// responseObservation receives exactly one observation of one upstream
// response. Production is the prefetch lease table; tests substitute a counting
// sink so observation counts are measured, not inferred from registered tasks.
type responseObservation interface {
	observeHeaders(r *http.Request, header http.Header)
	observeRevalidated(r *http.Request, key string, v hashstore.Variant, directives RelayDirectives, noStore bool)
}

// prefetchObservation is the production sink: the lease table learns from the
// response headers (or the folded 304 variant).
type prefetchObservation struct{ server *Server }

func (o prefetchObservation) observeHeaders(r *http.Request, header http.Header) {
	o.server.notePrefetchResponse(r, header)
}

func (o prefetchObservation) observeRevalidated(r *http.Request, key string, v hashstore.Variant, directives RelayDirectives, noStore bool) {
	o.server.noteRevalidatedResponse(r, key, v, directives, noStore)
}

func (s *Server) observeHeaders(r *http.Request, header http.Header) {
	if s.observer != nil {
		s.observer.observeHeaders(r, header)
		return
	}
	prefetchObservation{s}.observeHeaders(r, header)
}

func (s *Server) observeRevalidated(r *http.Request, key string, v hashstore.Variant, directives RelayDirectives, noStore bool) {
	if s.observer != nil {
		s.observer.observeRevalidated(r, key, v, directives, noStore)
		return
	}
	prefetchObservation{s}.observeRevalidated(r, key, v, directives, noStore)
}
