package server

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

// Metadata prefetch (M3).
//
// A prefetch lease is learned from a real response that carried
// X-MDB-Relay: prefetch. It lives only in this process: nothing is persisted
// and a restart starts with no tasks, waiting for real demand to rediscover
// them. A lease is renewed by later GET/HEAD requests for the same resource and
// disappears after three days without one.
//
// A normal resource is refreshed with a conditional GET at its freshness
// expiry. An APT mount gets one index task per mirror instead of a task per
// Release/Packages: the index is fetched from the Worker's fixed endpoint and
// its content hashes drive direct package delivery.
const (
	prefetchRetention  = 72 * time.Hour
	prefetchRunTimeout = 60 * time.Second
)

// prefetchBackoff is the retry cadence after a failed background refresh. The
// last value is the cap; old usable data is never deleted because a refresh
// failed.
var prefetchBackoff = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	30 * time.Minute,
}

type prefetchKind int

const (
	prefetchKindNormal prefetchKind = iota
	prefetchKindAPT
)

// prefetchTask is one in-memory lease. The fields below the scheduling block
// describe the resource; the scheduler owns the rest.
type prefetchTask struct {
	key  string
	kind prefetchKind

	mirror Mirror
	// relPath is mount-relative (the same path policies see); clientPath is what
	// the client addressed and includes a path mount's prefix. clientEscapedPath
	// keeps the client's exact escaping so a background replay addresses the
	// same resource (%2F must not become a separator).
	relPath           string
	clientPath        string
	clientEscapedPath string
	rawQuery          string

	// Normal resources: the cache URL plus the Vary identity, with only the
	// request headers that identity needs.
	cacheKey       string
	noVary         bool
	vary           []string
	requestHeaders http.Header

	// APT index task.
	aptEtag            string
	aptConflictLogged  bool
	backgroundDisabled bool

	retainUntil time.Time
	nextRun     time.Time
	hasNext     bool
	backoff     int
	running     bool
}

// prefetchOutcome is what one background run reports back to the scheduler.
type prefetchOutcome struct {
	// next is the raw moment the resource next becomes due. The scheduler adds
	// a small per-key jitter before storing it.
	next time.Time
	// noNext means the resource must not be refreshed automatically any more
	// (definite absence, or an expiry that is not in the future).
	noNext bool
	// etag replaces the APT index task's validator when set.
	etag string
	// etagSet distinguishes "no new validator" (304 without ETag) from a 200
	// generation that genuinely has none and must clear the old one.
	etagSet bool
	err     error
}

type prefetchRunner func(context.Context, prefetchTask) prefetchOutcome

type prefetchScheduler struct {
	mu        sync.Mutex
	tasks     map[string]*prefetchTask
	running   int
	closed    bool
	started   bool
	stopCh    chan struct{}
	doneCh    chan struct{}
	wake      chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	wg        sync.WaitGroup

	now func() time.Time
	// jitter spreads production refreshes; nil disables it, which also keeps
	// tests that drive the injected clock deterministic.
	jitter func(key string, window time.Duration) time.Duration

	// ctx is canceled by Close so an in-flight background request stops instead
	// of making shutdown wait for its own timeout.
	ctx    context.Context
	cancel context.CancelFunc

	concurrency int
	maxTasks    int
	run         prefetchRunner
}

func newPrefetchScheduler(now func() time.Time, concurrency, maxTasks int, run prefetchRunner) *prefetchScheduler {
	if now == nil {
		now = time.Now
	}
	if concurrency <= 0 {
		concurrency = 4
	}
	if maxTasks <= 0 {
		maxTasks = 4096
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &prefetchScheduler{
		tasks:       make(map[string]*prefetchTask),
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
		wake:        make(chan struct{}, 1),
		now:         now,
		ctx:         ctx,
		cancel:      cancel,
		concurrency: concurrency,
		maxTasks:    maxTasks,
		run:         run,
	}
}

// Start launches the scheduling loop. It is idempotent.
func (p *prefetchScheduler) Start() {
	p.startOnce.Do(func() {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		p.started = true
		p.mu.Unlock()
		go p.loop()
	})
}

// Close stops dispatch and waits for in-flight work. A scheduler that was never
// started closes without waiting.
func (p *prefetchScheduler) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		started := p.started
		close(p.stopCh)
		p.mu.Unlock()
		p.cancel()

		if started {
			<-p.doneCh
		} else {
			close(p.doneCh)
		}
		p.wg.Wait()
	})
}

func (p *prefetchScheduler) loop() {
	defer close(p.doneCh)
	for {
		wait, ok := p.nextWait()
		var timer *time.Timer
		var timerC <-chan time.Time
		if ok {
			if wait < 0 {
				wait = 0
			}
			timer = time.NewTimer(wait)
			timerC = timer.C
		}
		select {
		case <-p.stopCh:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-p.wake:
		case <-timerC:
		}
		if timer != nil {
			timer.Stop()
		}
		p.dispatchDue()
	}
}

func (p *prefetchScheduler) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *prefetchScheduler) nextWait() (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, false
	}
	// Every slot is busy, so nothing can be dispatched no matter how due a task
	// is. Returning a zero wait here would make the loop spin until a running
	// task finishes; finish() signals instead, so wait for that.
	if p.running >= p.concurrency {
		return 0, false
	}
	now := p.now()
	var next time.Time
	found := false
	for _, t := range p.tasks {
		if t.running || !t.hasNext {
			continue
		}
		if !found || t.nextRun.Before(next) {
			next = t.nextRun
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return next.Sub(now), true
}

// dispatchDue expires idle leases and starts every task that is due, within the
// concurrency bound. It is exported to tests in this package through the
// unexported name; production calls it from loop.
func (p *prefetchScheduler) dispatchDue() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}

	now := p.now()
	for key, t := range p.tasks {
		if now.Before(t.retainUntil) {
			continue
		}
		// Cleanup must not be blocked by a running task or a backoff: drop the
		// lease now. A running goroutine checks pointer identity before
		// rescheduling, so it cannot resurrect the task.
		delete(p.tasks, key)
	}

	var due []*prefetchTask
	for _, t := range p.tasks {
		if t.running || !t.hasNext || t.nextRun.After(now) {
			continue
		}
		due = append(due, t)
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].nextRun.Equal(due[j].nextRun) {
			return due[i].key < due[j].key
		}
		return due[i].nextRun.Before(due[j].nextRun)
	})

	for _, t := range due {
		if p.running >= p.concurrency {
			break
		}
		t.running = true
		p.running++
		snapshot := *t
		if snapshot.requestHeaders != nil {
			snapshot.requestHeaders = snapshot.requestHeaders.Clone()
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			ctx, cancel := context.WithTimeout(p.ctx, prefetchRunTimeout)
			defer cancel()
			outcome := p.run(ctx, snapshot)
			p.finish(t, outcome)
		}()
	}
}

func (p *prefetchScheduler) finish(t *prefetchTask, outcome prefetchOutcome) {
	p.mu.Lock()
	// A finished run usually writes a new nextRun, which may be earlier than the
	// moment the loop is currently sleeping until; waking it makes the wait
	// reflect the new schedule instead of the old one.
	defer func() {
		p.mu.Unlock()
		p.signal()
	}()

	t.running = false
	if p.running > 0 {
		p.running--
	}
	if p.closed || p.tasks[t.key] != t {
		// Expired or removed while running, or shutting down: never reinsert.
		return
	}
	if outcome.err != nil {
		log.Printf("relay prefetch: refresh %q failed: %v", t.cacheKey, outcome.err)
		if !t.hasNext {
			return
		}
		delay := prefetchBackoff[minInt(t.backoff, len(prefetchBackoff)-1)]
		if t.backoff < len(prefetchBackoff)-1 {
			t.backoff++
		}
		t.nextRun = p.now().Add(delay)
		t.hasNext = true
		return
	}
	if outcome.etagSet {
		t.aptEtag = outcome.etag
	} else if outcome.etag != "" {
		t.aptEtag = outcome.etag
	}
	if outcome.noNext {
		t.hasNext = false
		return
	}
	if !outcome.next.After(p.now()) {
		t.hasNext = false
		return
	}
	t.backoff = 0
	t.nextRun = p.jittered(t.key, outcome.next, p.now())
	t.hasNext = true
}

func (p *prefetchScheduler) jittered(key string, at, now time.Time) time.Time {
	if !at.After(now) {
		return at
	}
	if p.jitter == nil {
		return at
	}
	window := at.Sub(now)
	if window <= 0 {
		return at
	}
	return at.Add(p.jitter(key, window))
}

func (p *prefetchScheduler) touchLocked(key string, now time.Time) *prefetchTask {
	t := p.tasks[key]
	if t == nil {
		return nil
	}
	t.retainUntil = now.Add(prefetchRetention)
	return t
}

// TouchNormalVariant renews the lease of an existing normal task from a cache
// hit. It never creates one: a restart has no tasks until a response carrying
// the directive is seen again.
func (p *prefetchScheduler) TouchNormalVariant(cacheKey string, v hashstore.Variant, now time.Time) {
	key := normalTaskKey(cacheKey, v.NoVary, withoutAcceptEncoding(v.Vary), v.RequestHeaders)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.touchLocked(key, now)
}

// ObserveNormalVariant renews or creates a normal lease from a 304
// revalidation. A prefetch directive on that response can create a missing task
// (for example after a restart, when the variant persisted but the lease did
// not) and refreshes the next run from the folded variant expiry.
func (p *prefetchScheduler) ObserveNormalVariant(m Mirror, cacheKey, relPath, clientPath, clientEscapedPath, rawQuery string, v hashstore.Variant, now time.Time, prefetch, noStore bool) bool {
	key := normalTaskKey(cacheKey, v.NoVary, withoutAcceptEncoding(v.Vary), v.RequestHeaders)
	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.signal()
	if p.closed {
		return false
	}

	t := p.tasks[key]
	if t == nil {
		if !prefetch {
			return false
		}
		if len(p.tasks) >= p.maxTasks {
			log.Printf("relay prefetch warning: task limit %d reached, refusing %q", p.maxTasks, cacheKey)
			return false
		}
		t = &prefetchTask{
			key:               key,
			kind:              prefetchKindNormal,
			mirror:            m,
			relPath:           relPath,
			clientPath:        clientPath,
			clientEscapedPath: clientEscapedPath,
			rawQuery:          rawQuery,
			cacheKey:          cacheKey,
			noVary:            v.NoVary,
			vary:              withoutAcceptEncoding(v.Vary),
			requestHeaders:    v.RequestHeaders.Clone(),
		}
		p.tasks[key] = t
	} else {
		t.clientPath = clientPath
		t.clientEscapedPath = clientEscapedPath
		t.rawQuery = rawQuery
	}
	t.retainUntil = now.Add(prefetchRetention)
	if prefetch {
		p.applyNormalSchedule(t, v, now, noStore)
	}
	return true
}

func (p *prefetchScheduler) applyNormalSchedule(t *prefetchTask, v hashstore.Variant, now time.Time, noStore bool) {
	if noStore {
		if !t.backgroundDisabled {
			log.Printf("relay prefetch warning: %q declares no-store together with prefetch; no background write", t.cacheKey)
		}
		t.backgroundDisabled = true
		t.hasNext = false
		return
	}
	t.backgroundDisabled = false
	if v.ExpiresAt.After(now) {
		t.backoff = 0
		t.nextRun = p.jittered(t.key, v.ExpiresAt, now)
		t.hasNext = true
		return
	}
	// No freshness, no-cache, or an expiry already behind us: only a real
	// request may drive this resource. Never turn it into a tight loop.
	t.backoff = 0
	t.hasNext = false
}

// NoteNormalResponse registers or renews a normal lease from an upstream
// response. prefetch reports whether this response actually carried the
// directive; a response without it only renews an existing lease.
func (p *prefetchScheduler) NoteNormalResponse(m Mirror, cacheKey, relPath, clientPath, clientEscapedPath, rawQuery string, reqHeaders, respHeaders http.Header, directives RelayDirectives, now time.Time, noStore bool) {
	noVary, vary, matched := responseVariantIdentity(respHeaders, reqHeaders, directives)
	key := normalTaskKey(cacheKey, noVary, vary, matched)

	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.signal()
	if p.closed {
		return
	}

	t := p.tasks[key]
	if t == nil {
		if !directives.Prefetch {
			return
		}
		if len(p.tasks) >= p.maxTasks {
			log.Printf("relay prefetch warning: task limit %d reached, refusing %q", p.maxTasks, cacheKey)
			return
		}
		t = &prefetchTask{
			key:               key,
			kind:              prefetchKindNormal,
			mirror:            m,
			relPath:           relPath,
			clientPath:        clientPath,
			clientEscapedPath: clientEscapedPath,
			rawQuery:          rawQuery,
			cacheKey:          cacheKey,
			noVary:            noVary,
			vary:              vary,
			requestHeaders:    matched,
			retainUntil:       now.Add(prefetchRetention),
		}
		p.tasks[key] = t
	} else {
		t.retainUntil = now.Add(prefetchRetention)
		t.clientPath = clientPath
		t.clientEscapedPath = clientEscapedPath
		t.rawQuery = rawQuery
	}

	if !directives.Prefetch {
		return
	}
	v := hashstore.Variant{NoVary: noVary, Vary: vary, RequestHeaders: matched, ExpiresAt: prefetchExpiry(respHeaders, directives, now)}
	p.applyNormalSchedule(t, v, now, noStore)
}

// prefetchExpiry is the moment a response becomes eligible for background
// refresh. A response that declared its own immutability keeps the persistent
// lifetime even when the upstream stated a zero or short max-age: the bytes
// cannot change, so that must neither disable refresh silently nor turn it into
// a short cycle. It only chooses the schedule; a no-store response is still
// refused by the caller's noStore argument.
func prefetchExpiry(headers http.Header, directives RelayDirectives, now time.Time) time.Time {
	if directives.Immutable {
		return now.Add(persistentVariantLifetime)
	}
	return freshnessExpiry(headers, directives, now)
}

// NoteAPTResponse registers or renews the one index task for an apt-tagged
// mirror. noStore is accepted (the index task is independent of the client
// cache) but logged once per conflict state transition. gen is the current
// generation, whose effective deadline drives the next refresh.
func (p *prefetchScheduler) NoteAPTResponse(m Mirror, now time.Time, noStore bool, gen *aptGeneration) {
	key := aptTaskKey(m.Name)
	target, rel := aptIndexTarget(m)

	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.signal()
	if p.closed {
		return
	}

	t := p.tasks[key]
	if t == nil {
		if len(p.tasks) >= p.maxTasks {
			log.Printf("relay prefetch warning: task limit %d reached, refusing apt index for %q", p.maxTasks, m.Name)
			return
		}
		t = &prefetchTask{
			key:         key,
			kind:        prefetchKindAPT,
			mirror:      m,
			relPath:     rel,
			clientPath:  target,
			retainUntil: now.Add(prefetchRetention),
		}
		p.tasks[key] = t
	} else {
		t.retainUntil = now.Add(prefetchRetention)
		if t.clientPath != target || t.relPath != rel {
			// The manifest moved this mirror's mount. The stored validator and
			// the retry delay describe the old address, so neither may be reused:
			// fetch the index at the new target immediately. Replacing the task
			// (instead of mutating it) also discards an in-flight run: finish
			// validates the pointer under the key before rescheduling, so an
			// outcome for the old target cannot take the new one over.
			log.Printf("relay prefetch: apt mirror %q moved to %q; refetching the index", m.Name, target)
			p.tasks[key] = &prefetchTask{
				key:         key,
				kind:        prefetchKindAPT,
				mirror:      m,
				relPath:     rel,
				clientPath:  target,
				retainUntil: now.Add(prefetchRetention),
				nextRun:     now,
				hasNext:     true,
			}
			return
		}
		t.mirror = m
	}

	if noStore != t.aptConflictLogged {
		if noStore {
			log.Printf("relay prefetch warning: apt mirror %q declares no-store together with prefetch; keeping the independent index task", m.Name)
		}
		t.aptConflictLogged = noStore
	}
	if t.running {
		return
	}
	if gen != nil && gen.until.After(now) {
		if !t.hasNext {
			t.nextRun = p.jittered(t.key, gen.until, now)
			t.hasNext = true
		} else if t.nextRun.After(p.jittered(t.key, gen.until, now)) {
			// A fresh index that expires earlier than the current schedule should
			// pull the refresh forward; never push it later.
			t.nextRun = p.jittered(t.key, gen.until, now)
		}
		return
	}
	// No fresh index: fetch it immediately. Multiple Release requests share this
	// task and this lease.
	if t.backoff > 0 && t.nextRun.After(now) {
		t.hasNext = true
		return
	}
	if !t.hasNext || t.nextRun.After(now) {
		t.nextRun = now
		t.hasNext = true
	}
}

// noteRevalidatedResponse feeds a 304 revalidation into the lease table. It is a
// separate shape from notePrefetchResponse because a 304 has no body: the folded
// variant carries the metadata. An APT Release still forms an ordinary lease
// here; the per-mirror index task is learned from a full Release response.
func (s *Server) noteRevalidatedResponse(r *http.Request, key string, v hashstore.Variant, directives RelayDirectives, noStore bool) {
	if s.prefetch == nil {
		return
	}
	mirror, path, ok := s.resolveMirror(s.manifest.Load(), r)
	if !ok || isRelaytestPath(path) {
		return
	}
	s.prefetch.ObserveNormalVariant(mirror, key, path, r.URL.Path, r.URL.EscapedPath(), r.URL.RawQuery,
		v, s.currentTime(), directives.Prefetch, noStore)
}

// notePrefetchResponse feeds one real upstream response into the lease table.
// A response without the directive only renews an existing lease; it never
// creates one. Passthrough never reaches this path.
func (s *Server) notePrefetchResponse(r *http.Request, responseHeaders http.Header) {
	if s.prefetch == nil || s.hashStore == nil {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return
	}
	st := s.manifest.Load()
	mirror, relPath, ok := s.resolveMirror(st, r)
	if !ok || isRelaytestPath(relPath) {
		return
	}

	directives := ParseRelayDirectives(responseHeaders.Values(RelayDirectiveHeader))
	now := s.currentTime()
	noStore := directives.NoStore ||
		cacheControlHas(responseHeaders.Values("Cache-Control"), "no-store") ||
		cacheControlHas(responseHeaders.Values("Cache-Control"), "private")

	if isAptMirror(mirror) && isAptReleasePath(relPath) {
		// APT registers one shared index task per mirror, never one task per
		// Packages or Release object.
		if !directives.Prefetch {
			return
		}
		s.prefetch.NoteAPTResponse(mirror, now, noStore, s.aptIndexes.generation(mirror.Name))
		return
	}

	key := cacheKey(mirror, relPath, r.URL)
	s.prefetch.NoteNormalResponse(mirror, key, relPath, r.URL.Path, r.URL.EscapedPath(), r.URL.RawQuery,
		r.Header, responseHeaders, directives, now, noStore)
}

// -- background execution --

// runPrefetchTask is the scheduler's runner bound to this Server.
func (s *Server) runPrefetchTask(ctx context.Context, task prefetchTask) prefetchOutcome {
	switch task.kind {
	case prefetchKindAPT:
		return s.runAPTIndexTask(ctx, task)
	default:
		return s.runNormalPrefetch(ctx, task)
	}
}

// runNormalPrefetch refreshes one cached representation. It is the background
// kernel: it never uses a ResponseWriter and never buffers a body in memory.
// Only a complete 200 GET is stored; a 304 updates metadata; 404/410 drop the
// variant; HEAD never writes (the background request is always GET).
func (s *Server) runNormalPrefetch(ctx context.Context, task prefetchTask) prefetchOutcome {
	var old *hashstore.Variant
	variants, err := s.hashStore.Variants(ctx, task.cacheKey)
	if err != nil {
		return prefetchOutcome{err: err}
	}
	matches := matchingVariants(variants, task.requestHeaders)
	if len(matches) == 1 {
		v := matches[0]
		old = &v
	} else if len(matches) > 1 {
		// Ambiguity is not something a background task may resolve by picking
		// one; leave it for a real request, which re-fetches with cleanup.
		return prefetchOutcome{noNext: true}
	}

	req := s.syntheticRequest(ctx, task.mirror, task.clientPath, task.clientEscapedPath, task.rawQuery, http.MethodGet)
	req.Header.Set("Accept-Encoding", "identity")
	for name, values := range task.requestHeaders {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	stripConditionalHeaders(req.Header)
	req.Header.Del("Range")
	if old != nil {
		if etag := old.ResponseHeaders.Get("ETag"); etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		if modified := old.ResponseHeaders.Get("Last-Modified"); modified != "" {
			req.Header.Set("If-Modified-Since", modified)
		}
	}

	out, err := s.buildUpstreamRequest(ctx, req)
	if err != nil {
		return prefetchOutcome{err: err}
	}
	out = s.withMirrorPolicy(out, task.mirror, task.relPath)
	resp, err := s.roundTrip(out)
	if err != nil {
		return prefetchOutcome{err: err}
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		if old == nil {
			return prefetchOutcome{err: errors.New("unexpected 304 without a stored variant")}
		}
		next := s.revalidatedVariant(*old, resp)
		directives := RelayDirectivesFromHeader(resp.Header)
		if storableMetadata(next.ResponseHeaders, directives, next.NoVary) {
			if next.Persistent {
				withPersistentLifetime(next.ResponseHeaders)
			}
			if err := s.commitVariant(ctx, next, old); err != nil {
				return prefetchOutcome{err: err}
			}
		} else {
			s.dropVariant(*old)
		}
		return prefetchOutcome{next: next.ExpiresAt, noNext: !next.ExpiresAt.After(s.currentTime())}
	case http.StatusNotFound, http.StatusGone:
		if old != nil {
			s.dropVariant(*old)
		}
		return prefetchOutcome{noNext: true}
	case http.StatusOK:
		directives := RelayDirectivesFromHeader(resp.Header)
		if !s.storable(resp, req, directives) {
			// The upstream answered with a new representation it forbids us to
			// store. The old entry is known stale, so drop it rather than keep
			// serving a copy upstream no longer agrees with.
			if old != nil {
				s.dropVariant(*old)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			return prefetchOutcome{noNext: true}
		}
		stored, persistent := responseVariantHeaders(resp.Header, directives)
		storedAt := s.currentTime()
		expiresAt := freshnessExpiry(stored, directives, storedAt)
		putSize := resp.ContentLength
		budget := s.hashStore.Stats().Capacity
		if putSize > budget {
			_, _ = io.Copy(io.Discard, resp.Body)
			return prefetchOutcome{noNext: true}
		}
		content, err := s.hashStore.Put(ctx, resp.Body, putSize)
		if err != nil {
			if errors.Is(err, hashstore.ErrOversized) {
				// The stream declared no length and turned out to exceed the
				// budget. Retrying would download the same oversized body again
				// on every backoff step, so stop refreshing automatically.
				log.Printf("relay prefetch: %q exceeds the store budget; not retrying", task.cacheKey)
				return prefetchOutcome{noNext: true}
			}
			return prefetchOutcome{err: err}
		}
		next := buildVariant(task.cacheKey, stored, directives, task.requestHeaders, storedAt, expiresAt, content)
		next.Persistent = persistent
		if err := s.commitVariant(ctx, next, old); err != nil {
			return prefetchOutcome{err: err}
		}
		return prefetchOutcome{next: expiresAt, noNext: !expiresAt.After(s.currentTime())}
	default:
		_, _ = io.Copy(io.Discard, resp.Body)
		return prefetchOutcome{err: errors.New("prefetch refresh returned " + resp.Status)}
	}
}

// syntheticRequest builds the client-facing request shape a background task
// replays. buildUpstreamRequest then applies the same host and base-path rules
// as a real request. clientEscapedPath keeps the client's original escaping, so
// an escaped separator stays escaped instead of silently addressing another
// resource.
func (s *Server) syntheticRequest(ctx context.Context, m Mirror, clientPath, clientEscapedPath, rawQuery, method string) *http.Request {
	host := ""
	if len(s.baseDomains) > 0 {
		host = s.baseDomains[0]
	}
	if m.Host != "" && host != "" {
		host = m.Host + "." + host
	}
	u := &url.URL{Path: clientPath, RawQuery: rawQuery}
	if clientEscapedPath != "" {
		// net/url only uses RawPath when it decodes back to Path; a task built
		// without the matching decoded form falls back to Path escaping.
		u.RawPath = clientEscapedPath
	}
	req := &http.Request{
		Method:     method,
		URL:        u,
		Host:       host,
		Header:     make(http.Header),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}
	return req.WithContext(ctx)
}

// withMirrorPolicy attaches the manifest's transport preference for the
// background path. Passthrough is a client-response decision and is not applied
// to a background fetch: there is no response to pass through.
func (s *Server) withMirrorPolicy(req *http.Request, m Mirror, relPath string) *http.Request {
	st := s.manifest.Load()
	if st == nil {
		return req
	}
	d := st.policyFor(m, relPath)
	return req.WithContext(withPolicyDecision(req.Context(), policyDecision{TrafficPolicy: d.TrafficPolicy}))
}

// responseVariantIdentity derives the same identity the cache store uses for a
// response, keeping only the request headers named by Vary.
func responseVariantIdentity(respHeaders, reqHeaders http.Header, directives RelayDirectives) (bool, []string, http.Header) {
	if directives.NoVary {
		return true, nil, http.Header{}
	}
	if varyContainsStar(respHeaders.Values("Vary")) {
		return true, nil, http.Header{}
	}
	vary := withoutAcceptEncoding(varyNames(respHeaders.Values("Vary")))
	if len(vary) == 0 {
		return true, nil, http.Header{}
	}
	sort.Strings(vary)
	return false, vary, selectRequestHeaders(reqHeaders, vary)
}

func selectRequestHeaders(h http.Header, vary []string) http.Header {
	type entry struct {
		name   string
		values []string
	}
	grouped := make(map[string][]entry, len(h))
	for key, values := range h {
		name := strings.ToLower(strings.TrimSpace(key))
		if name == "" {
			continue
		}
		grouped[name] = append(grouped[name], entry{name: key, values: values})
	}

	out := make(http.Header, len(vary))
	for _, name := range vary {
		entries := grouped[strings.ToLower(name)]
		if len(entries) == 0 {
			continue
		}
		if len(entries) > 1 {
			sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
		}
		var parts []string
		for _, e := range entries {
			for _, value := range e.values {
				if value = strings.TrimSpace(value); value != "" {
					parts = append(parts, value)
				}
			}
		}
		out[name] = []string{strings.Join(parts, ", ")}
	}
	return out
}

func normalTaskKey(cacheKey string, noVary bool, vary []string, matched http.Header) string {
	var b strings.Builder
	b.WriteString("normal\x00")
	b.WriteString(cacheKey)
	if noVary {
		b.WriteString("\x00nv")
		return b.String()
	}
	for _, name := range withoutAcceptEncoding(vary) {
		b.WriteString("\x00v\x00")
		b.WriteString(name)
		b.WriteString("\x00")
		values, ok := matchedValueLower(matched, name)
		if !ok {
			b.WriteString("-\x00")
			continue
		}
		b.WriteString("=")
		b.WriteString(values)
		b.WriteString("\x00")
	}
	return b.String()
}

func matchedValueLower(h http.Header, name string) (string, bool) {
	for key, values := range h {
		if !strings.EqualFold(key, name) {
			continue
		}
		if len(values) == 0 {
			return "", true
		}
		return values[0], true
	}
	return "", false
}

func aptTaskKey(mirror string) string {
	return "apt-index\x00" + mirror
}

// aptIndexTarget returns the client-facing path and the policy-relative path of
// the fixed index endpoint under a mirror's mount.
func aptIndexTarget(m Mirror) (string, string) {
	const rel = "/.mdb/apt-index.json"
	if m.Host != "" {
		return rel, rel
	}
	if m.Path != "" {
		return "/" + m.Path + rel, rel
	}
	return rel, rel
}

func isAptMirror(m Mirror) bool {
	return slices.Contains(m.PolicyTags, "apt")
}

func isAptReleasePath(relPath string) bool {
	name := relPath
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name == "Release" || name == "InRelease"
}

// defaultPrefetchJitter spreads refreshes that share an expiry. It is
// deterministic per task so tests and restarts do not introduce randomness.
func defaultPrefetchJitter(key string, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	max := window / 10
	if max > 30*time.Second {
		max = 30 * time.Second
	}
	if max <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	step := time.Millisecond
	slots := int64(max/step) + 1
	return time.Duration(int64(h.Sum32())%slots) * step
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
