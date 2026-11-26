package diskspace

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProbe is safe for concurrent callers, which the race detector relies on
// when several writers share one Checker.
type fakeProbe struct {
	mu    sync.Mutex
	calls int
	free  int64
	err   error
}

func (p *fakeProbe) probe(string) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.free, p.err
}

func (p *fakeProbe) setFree(free int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.free = free
}

func (p *fakeProbe) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestDisabledNeverProbes(t *testing.T) {
	var calls atomic.Int64
	c := New("/tmp/cache", 0, WithProbe(func(string) (int64, error) {
		calls.Add(1)
		return 0, errors.New("must not be called")
	}))

	if c.Enabled() {
		t.Fatal("watermark 0 must be disabled")
	}
	if err := c.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if err := c.Account(1 << 30); err != nil {
		t.Fatalf("Account: %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("probe called %d times while disabled", n)
	}
}

func TestCheckSufficientAndLow(t *testing.T) {
	p := &fakeProbe{free: 1024}
	c := New("/tmp/cache", 1024, WithProbe(p.probe))

	if err := c.Check(); err != nil {
		t.Fatalf("free == watermark must pass: %v", err)
	}

	p.setFree(1023)
	err := c.Check()
	if !errors.Is(err, ErrLowDiskSpace) {
		t.Fatalf("got %v, want ErrLowDiskSpace", err)
	}
	if !strings.Contains(err.Error(), "/tmp/cache") {
		t.Errorf("error lacks the path: %v", err)
	}
}

func TestProbeFailureDegrades(t *testing.T) {
	p := &fakeProbe{err: errors.New("statfs unavailable")}
	c := New("/tmp/cache", 1, WithProbe(p.probe))

	if err := c.Check(); err != nil {
		t.Fatalf("probe failure must not block the write: %v", err)
	}
	if err := c.Check(); err != nil {
		t.Fatalf("second Check: %v", err)
	}
	// A failed probe is not cached as a verdict: the next Check tries again and
	// can recover.
	if got := p.callCount(); got != 2 {
		t.Fatalf("probe calls = %d, want 2", got)
	}
}

func TestAccountThrottlesByBytes(t *testing.T) {
	p := &fakeProbe{free: 1024}
	c := New("/tmp/cache", 512, WithProbe(p.probe), WithRecheck(100, time.Hour))

	if err := c.Check(); err != nil {
		t.Fatalf("initial check: %v", err)
	}
	if got := p.callCount(); got != 1 {
		t.Fatalf("probe calls = %d, want 1", got)
	}

	p.setFree(10)
	if err := c.Account(40); err != nil {
		t.Fatalf("below threshold must not probe: %v", err)
	}
	if got := p.callCount(); got != 1 {
		t.Fatalf("small write probed: calls = %d", got)
	}

	err := c.Account(70) // 40+70 >= 100
	if !errors.Is(err, ErrLowDiskSpace) {
		t.Fatalf("got %v, want ErrLowDiskSpace", err)
	}
	if got := p.callCount(); got != 2 {
		t.Fatalf("probe calls = %d, want 2", got)
	}

	// The probe reset the byte counter; the next small write does not re-probe.
	if err := c.Account(1); err != nil {
		t.Fatalf("post-probe write: %v", err)
	}
	if got := p.callCount(); got != 2 {
		t.Fatalf("byte counter was not reset: calls = %d", got)
	}
}

func TestAccountThrottlesByTime(t *testing.T) {
	p := &fakeProbe{free: 1024}
	c := New("/tmp/cache", 512, WithProbe(p.probe), WithRecheck(1<<30, 10*time.Millisecond))

	if err := c.Check(); err != nil {
		t.Fatalf("initial check: %v", err)
	}
	p.setFree(1)
	time.Sleep(25 * time.Millisecond)

	if err := c.Account(1); !errors.Is(err, ErrLowDiskSpace) {
		t.Fatalf("elapsed interval must re-probe, got %v", err)
	}
	if got := p.callCount(); got != 2 {
		t.Fatalf("probe calls = %d, want 2", got)
	}
}

func TestConcurrentCheckAndAccount(t *testing.T) {
	p := &fakeProbe{free: 1 << 40}
	c := New("/tmp/cache", 1024, WithProbe(p.probe), WithRecheck(64, time.Microsecond))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if err := c.Check(); err != nil {
					t.Errorf("Check: %v", err)
					return
				}
				if err := c.Account(32); err != nil {
					t.Errorf("Account: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
