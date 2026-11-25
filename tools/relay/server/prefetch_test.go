package server

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
)

func newTestPrefetchScheduler(clock *testClock, concurrency, maxTasks int, run prefetchRunner) *prefetchScheduler {
	if run == nil {
		run = func(context.Context, prefetchTask) prefetchOutcome { return prefetchOutcome{noNext: true} }
	}
	p := newPrefetchScheduler(clock.Now, concurrency, maxTasks, run)
	p.jitter = func(string, time.Duration) time.Duration { return 0 }
	return p
}

func snapshotTask(t *testing.T, p *prefetchScheduler, key string) prefetchTask {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	task := p.tasks[key]
	if task == nil {
		t.Fatalf("task %q is not registered", key)
	}
	return *task
}

func noteNormal(t *testing.T, p *prefetchScheduler, m Mirror, key, relPath, clientPath string, reqHeaders http.Header, directives, cacheControl string, now time.Time) {
	t.Helper()
	h := make(http.Header)
	if directives != "" {
		h.Set(RelayDirectiveHeader, directives)
	}
	if cacheControl != "" {
		h.Set("Cache-Control", cacheControl)
	}
	d := ParseRelayDirectives(h.Values(RelayDirectiveHeader))
	noStore := d.NoStore || cacheControlHas(h.Values("Cache-Control"), "no-store") || cacheControlHas(h.Values("Cache-Control"), "private")
	p.NoteNormalResponse(m, key, relPath, clientPath, clientPath, "", reqHeaders, h, d, now, noStore)
}

func TestPrefetchLazyRegistrationAndRenewal(t *testing.T) {
	clock := newTestClock()
	p := newTestPrefetchScheduler(clock, 4, 4096, nil)
	m := Mirror{Name: "test", Path: "test"}
	req := http.Header{}

	// No directive: no task.
	noteNormal(t, p, m, "mirror://test/x", "/x", "/test/x", req, "", "max-age=60", clock.Now())
	if len(p.tasks) != 0 {
		t.Fatalf("registered without prefetch: %d tasks", len(p.tasks))
	}

	noteNormal(t, p, m, "mirror://test/x", "/x", "/test/x", req, "prefetch", "max-age=60", clock.Now())
	if len(p.tasks) != 1 {
		t.Fatalf("tasks after prefetch = %d", len(p.tasks))
	}
	task := snapshotTask(t, p, normalTaskKey("mirror://test/x", true, nil, http.Header{}))
	if !task.hasNext || !task.nextRun.Equal(clock.Now().Add(60*time.Second)) {
		t.Fatalf("schedule = (%v, %v), want +60s", task.nextRun, task.hasNext)
	}

	// Renewal through a cache hit (no response directive available there).
	clock.Advance(48 * time.Hour)
	v := hashstore.Variant{NoVary: true}
	p.TouchNormalVariant("mirror://test/x", v, clock.Now())
	if got := snapshotTask(t, p, task.key).retainUntil; !got.Equal(clock.Now().Add(prefetchRetention)) {
		t.Fatalf("retainUntil after touch = %v", got)
	}

	// A further 48h without demand still leaves a day of lease.
	clock.Advance(48 * time.Hour)
	p.dispatchDue()
	if got := len(p.tasks); got != 1 {
		t.Fatalf("task removed before the three-day lease ended: %d", got)
	}

	// Past the lease it is dropped even if its next refresh would be due.
	clock.Advance(25 * time.Hour)
	p.dispatchDue()
	if got := len(p.tasks); got != 0 {
		t.Fatalf("expired task was not removed: %d", got)
	}

	// A fresh scheduler after a restart has no tasks.
	restarted := newTestPrefetchScheduler(clock, 4, 4096, nil)
	if len(restarted.tasks) != 0 {
		t.Fatalf("restart reconstructed %d tasks", len(restarted.tasks))
	}
}

func TestPrefetchFreshnessScheduling(t *testing.T) {
	clock := newTestClock()
	p := newTestPrefetchScheduler(clock, 4, 4096, nil)
	m := Mirror{Name: "test", Path: "test"}

	noteNormal(t, p, m, "mirror://test/a", "/a", "/test/a", http.Header{}, "prefetch", "max-age=120", clock.Now())
	a := snapshotTask(t, p, normalTaskKey("mirror://test/a", true, nil, http.Header{}))
	if !a.hasNext || !a.nextRun.Equal(clock.Now().Add(120*time.Second)) {
		t.Fatalf("max-age schedule = (%v, %v)", a.nextRun, a.hasNext)
	}

	noteNormal(t, p, m, "mirror://test/b", "/b", "/test/b", http.Header{}, "prefetch", "no-cache", clock.Now())
	b := snapshotTask(t, p, normalTaskKey("mirror://test/b", true, nil, http.Header{}))
	if b.hasNext {
		t.Fatal("no-cache formed an automatic refresh schedule")
	}

	noteNormal(t, p, m, "mirror://test/c", "/c", "/test/c", http.Header{}, "prefetch", "", clock.Now())
	c := snapshotTask(t, p, normalTaskKey("mirror://test/c", true, nil, http.Header{}))
	if c.hasNext {
		t.Fatal("a response without freshness formed an automatic refresh schedule")
	}
}

func TestPrefetchTaskLimitAndBackoff(t *testing.T) {
	clock := newTestClock()
	fail := func(context.Context, prefetchTask) prefetchOutcome {
		return prefetchOutcome{err: errTestPrefetch}
	}
	p := newTestPrefetchScheduler(clock, 4, 2, fail)
	m := Mirror{Name: "test", Path: "test"}
	hdrs := http.Header{}

	for _, path := range []string{"/a", "/b", "/c"} {
		noteNormal(t, p, m, "mirror://test"+path, path, "/test"+path, hdrs, "prefetch", "max-age=10", clock.Now())
	}
	if got := len(p.tasks); got != 2 {
		t.Fatalf("task limit kept %d tasks, want 2", got)
	}

	clock.Advance(10 * time.Second)
	want := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, delay := range want {
		p.dispatchDue()
		p.wg.Wait()
		task := snapshotTask(t, p, normalTaskKey("mirror://test/a", true, nil, http.Header{}))
		if wantAt := clock.Now().Add(delay); !task.nextRun.Equal(wantAt) {
			t.Fatalf("failure %d nextRun = %v, want %v", i+1, task.nextRun, wantAt)
		}
		clock.Advance(delay)
	}
}

func TestPrefetchConcurrencyBound(t *testing.T) {
	clock := newTestClock()
	var started atomic.Int32
	release := make(chan struct{})
	run := func(ctx context.Context, task prefetchTask) prefetchOutcome {
		started.Add(1)
		<-release
		return prefetchOutcome{noNext: true}
	}
	p := newTestPrefetchScheduler(clock, 4, 4096, run)
	for i := 0; i < 6; i++ {
		key := fmt.Sprintf("mirror://test/%d", i)
		p.mu.Lock()
		p.tasks[key] = &prefetchTask{key: key, kind: prefetchKindNormal, clientPath: "/x", nextRun: clock.Now(), hasNext: true, retainUntil: clock.Now().Add(time.Hour)}
		p.mu.Unlock()
	}

	p.dispatchDue()
	deadline := time.Now().Add(2 * time.Second)
	for started.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := started.Load(); got != 4 {
		t.Fatalf("started %d tasks, want the concurrency bound 4", got)
	}
	close(release)
	p.wg.Wait()

	// The remaining two run only after capacity frees up.
	p.dispatchDue()
	p.wg.Wait()
	if got := started.Load(); got != 6 {
		t.Fatalf("eventually started %d tasks, want 6", got)
	}
}

func TestPrefetchCloseStopsDispatch(t *testing.T) {
	clock := newTestClock()
	var runs atomic.Int32
	ran := make(chan struct{}, 4)
	run := func(ctx context.Context, task prefetchTask) prefetchOutcome {
		runs.Add(1)
		select {
		case ran <- struct{}{}:
		default:
		}
		return prefetchOutcome{noNext: true}
	}
	p := newTestPrefetchScheduler(clock, 1, 8, run)
	key := "mirror://test/a"
	p.mu.Lock()
	p.tasks[key] = &prefetchTask{key: key, kind: prefetchKindNormal, clientPath: "/x", nextRun: clock.Now(), hasNext: true, retainUntil: clock.Now().Add(time.Hour)}
	p.mu.Unlock()

	p.Start()
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not run a due task")
	}
	p.Close()

	key2 := "mirror://test/b"
	p.mu.Lock()
	p.tasks[key2] = &prefetchTask{key: key2, kind: prefetchKindNormal, clientPath: "/y", nextRun: clock.Now(), hasNext: true, retainUntil: clock.Now().Add(time.Hour)}
	p.mu.Unlock()
	p.dispatchDue()
	p.wg.Wait()
	if got := runs.Load(); got != 1 {
		t.Fatalf("ran %d tasks after Close", got)
	}
}

func TestPrefetchNoStoreConflict(t *testing.T) {
	clock := newTestClock()
	p := newTestPrefetchScheduler(clock, 4, 4096, nil)
	m := Mirror{Name: "test", Path: "test"}
	noteNormal(t, p, m, "mirror://test/x", "/x", "/test/x", http.Header{}, "prefetch,no-store", "", clock.Now())
	task := snapshotTask(t, p, normalTaskKey("mirror://test/x", true, nil, http.Header{}))
	if !task.backgroundDisabled {
		t.Fatal("no-store task was not disabled")
	}
	if task.hasNext {
		t.Fatal("no-store task was scheduled for a background write")
	}
}

func TestPrefetchVariantRequestHeaders(t *testing.T) {
	clock := newTestClock()
	p := newTestPrefetchScheduler(clock, 4, 4096, nil)
	m := Mirror{Name: "test", Path: "test"}
	req := http.Header{"Accept-Language": {"zh-CN, en"}, "User-Agent": {"curl/8"}}
	h := http.Header{"Cache-Control": {"max-age=60"}, "Vary": {"Accept-Language"}}
	d := ParseRelayDirectives([]string{"prefetch"})

	p.NoteNormalResponse(m, "mirror://test/x", "/x", "/test/x", "/test/x", "a=1", req, h, d, clock.Now(), false)
	if len(p.tasks) != 1 {
		t.Fatalf("tasks = %d", len(p.tasks))
	}
	for _, task := range p.tasks {
		if len(task.requestHeaders) != 1 {
			t.Fatalf("stored request headers = %v, want only Accept-Language", task.requestHeaders)
		}
		if got := task.requestHeaders["accept-language"]; len(got) != 1 || got[0] != "zh-CN, en" {
			t.Fatalf("Accept-Language = %v", got)
		}
		if task.noVary {
			t.Fatal("Vary response was folded into no-vary")
		}
	}
}

var errTestPrefetch = testError("prefetch test failure")

type testError string

func (e testError) Error() string { return string(e) }

// TestPrefetchLoopRunsAgainAfterBackgroundCompletion drives the real loop
// goroutine: a finished run writes a new nextRun and must wake the loop so that
// wait is recomputed. Without the wake the loop stays blocked on its channel and
// the second run only happens on a new client request.
func TestPrefetchLoopRunsAgainAfterBackgroundCompletion(t *testing.T) {
	var runs atomic.Int32
	ran := make(chan int32, 4)
	run := func(context.Context, prefetchTask) prefetchOutcome {
		n := runs.Add(1)
		ran <- n
		if n == 1 {
			// A real timer short enough for a test, not an injected clock: the
			// goroutine under test is what schedules it.
			return prefetchOutcome{next: time.Now().Add(40 * time.Millisecond)}
		}
		return prefetchOutcome{noNext: true}
	}

	p := newPrefetchScheduler(time.Now, 2, 8, run)
	key := "mirror://test/a"
	p.tasks[key] = &prefetchTask{
		key:         key,
		kind:        prefetchKindNormal,
		clientPath:  "/a",
		nextRun:     time.Now(),
		hasNext:     true,
		retainUntil: time.Now().Add(time.Hour),
	}
	p.Start()
	defer p.Close()

	for want := int32(1); want <= 2; want++ {
		select {
		case got := <-ran:
			if got != want {
				t.Fatalf("run order = %d, want %d", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("background run %d did not happen without a new request", want)
		}
	}
}

// TestPrefetchNextWaitWhenSaturated pins the anti-spin rule: with every slot
// busy, a due task must not shorten the wait to zero, or the loop would keep
// firing timers until a running task finishes.
func TestPrefetchNextWaitWhenSaturated(t *testing.T) {
	clock := newTestClock()
	p := newTestPrefetchScheduler(clock, 1, 8, nil)
	now := clock.Now()
	p.tasks["running"] = &prefetchTask{key: "running", running: true, retainUntil: now.Add(time.Hour)}
	p.tasks["due"] = &prefetchTask{key: "due", nextRun: now, hasNext: true, retainUntil: now.Add(time.Hour)}
	p.running = 1

	if wait, ok := p.nextWait(); ok {
		t.Fatalf("nextWait = (%v, %v), want no timer while saturated", wait, ok)
	}

	p.running = 0
	wait, ok := p.nextWait()
	if !ok || wait > 0 {
		t.Fatalf("nextWait after a slot freed = (%v, %v), want a zero wait", wait, ok)
	}
}

// TestPrefetchLoopDoesNotSpinWhileSaturated is the runtime form of the same
// rule: a saturated scheduler with another due task must be idle, not cycling
// on a zero-length timer. The clock counts calls, which only the loop's wait
// computation (and dispatch) produce.
func TestPrefetchLoopDoesNotSpinWhileSaturated(t *testing.T) {
	clock := newTestClock()
	var nowCalls atomic.Int64
	counted := func() time.Time {
		nowCalls.Add(1)
		return clock.Now()
	}

	release := make(chan struct{})
	var started atomic.Int32
	run := func(context.Context, prefetchTask) prefetchOutcome {
		started.Add(1)
		<-release
		return prefetchOutcome{noNext: true}
	}

	p := newPrefetchScheduler(counted, 1, 8, run)
	p.jitter = func(string, time.Duration) time.Duration { return 0 }
	now := clock.Now()
	for _, key := range []string{"a", "b"} {
		p.tasks[key] = &prefetchTask{
			key: key, kind: prefetchKindNormal, clientPath: "/" + key,
			nextRun: now, hasNext: true, retainUntil: now.Add(time.Hour),
		}
	}
	p.Start()
	defer p.Close()

	deadline := time.Now().Add(2 * time.Second)
	for started.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := started.Load(); got != 1 {
		t.Fatalf("started %d tasks, want the concurrency bound of 1", got)
	}

	before := nowCalls.Load()
	time.Sleep(150 * time.Millisecond)
	spins := nowCalls.Load() - before
	if spins > 1000 {
		t.Fatalf("saturated loop recomputed its wait %d times in 150ms", spins)
	}

	close(release)
	deadline = time.Now().Add(2 * time.Second)
	for started.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := started.Load(); got != 2 {
		t.Fatalf("started %d tasks, want the second due task after capacity freed", got)
	}
}
