// Package diskspace guards local writes with a minimum free-space watermark.
//
// It is deliberately independent of any one cache implementation: a caller
// constructs a Checker for the directory it writes into, calls Check before
// creating a temporary file and Account as bytes flow, and treats ErrLowDiskSpace
// as "do not store this body". Reads and deletions are never gated.
//
// The free-space probe is platform-specific. When it is unsupported or fails,
// the Checker logs one warning and degrades to "no watermark": explicit capacity
// limits keep the service running instead of turning a measurement problem into
// an outage.
package diskspace

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// ErrLowDiskSpace means the filesystem is below the configured watermark.
// Callers match it with errors.Is and fall back to not storing the body; it is
// never a reason to fail a read.
var ErrLowDiskSpace = errors.New("diskspace: free space below watermark")

// Probe reports the bytes available to unprivileged writers on the filesystem
// holding path.
type Probe func(path string) (int64, error)

const (
	// DefaultRecheckBytes is how many bytes may be written between probes in the
	// middle of one write. Probing on every small write would call statfs far
	// more often than the free-space figure can change.
	DefaultRecheckBytes int64 = 16 << 20
	// DefaultRecheckInterval bounds the time between mid-write probes, so a slow
	// writer still notices the watermark.
	DefaultRecheckInterval = time.Second
)

// Option configures a Checker.
type Option func(*Checker)

// WithProbe replaces the platform probe. It exists for tests and for callers
// that already have a free-space figure; production code uses the default.
func WithProbe(p Probe) Option {
	return func(c *Checker) {
		if p != nil {
			c.probe = p
		}
	}
}

// WithRecheck sets the mid-write cadence. Non-positive values are ignored, so a
// zero value keeps the defaults rather than probing every write.
func WithRecheck(bytes int64, interval time.Duration) Option {
	return func(c *Checker) {
		if bytes > 0 {
			c.recheckBytes = bytes
		}
		if interval > 0 {
			c.recheckInterval = interval
		}
	}
}

// Checker is a concurrency-safe free-space guard for one directory. A zero or
// negative watermark disables it: every call succeeds without probing.
type Checker struct {
	path            string
	watermark       int64
	probe           Probe
	recheckBytes    int64
	recheckInterval time.Duration

	// mu guards the throttling counters shared by every concurrent writer.
	mu         sync.Mutex
	sinceProbe int64
	lastProbe  time.Time

	warnOnce sync.Once
}

// New builds a Checker for path. watermark is the minimum number of free bytes
// that must remain on the filesystem; 0 disables the guard.
func New(path string, watermark int64, opts ...Option) *Checker {
	c := &Checker{
		path:            path,
		watermark:       watermark,
		probe:           systemProbe,
		recheckBytes:    DefaultRecheckBytes,
		recheckInterval: DefaultRecheckInterval,
		lastProbe:       time.Now(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Enabled reports whether a watermark is configured. A disabled Checker never
// probes and never blocks a write.
func (c *Checker) Enabled() bool { return c.watermark > 0 }

// Check probes the filesystem now and returns ErrLowDiskSpace when fewer than
// the watermark bytes are free. It is meant for the moment before a temporary
// file is created. A probe failure is logged once and treated as sufficient
// space, because the explicit capacity limits still bound the store.
func (c *Checker) Check() error {
	if !c.Enabled() {
		return nil
	}
	free, err := c.probe(c.path)
	c.markProbed()
	if err != nil {
		c.warnOnce.Do(func() {
			log.Printf("diskspace: probe %q failed: %v; watermark disabled, explicit capacity limits still apply", c.path, err)
		})
		return nil
	}
	if free < c.watermark {
		return fmt.Errorf("%w: %d bytes free at %s, watermark %d", ErrLowDiskSpace, free, c.path, c.watermark)
	}
	return nil
}

// Account records n freshly written bytes and re-probes only when the mid-write
// cadence is due: every recheckBytes, or once recheckInterval has elapsed since
// the last probe. It is safe to call from concurrent writers. A returned
// ErrLowDiskSpace means the caller should abandon and remove its temporary
// file.
func (c *Checker) Account(n int64) error {
	if !c.Enabled() || n <= 0 {
		return nil
	}
	if !c.recheckDue(n) {
		return nil
	}
	return c.Check()
}

// recheckDue accumulates n and reports whether a probe is due. Bytes are only
// reset by markProbed, so a write that never crosses a threshold is checked on
// the next write after one has.
func (c *Checker) recheckDue(n int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sinceProbe += n
	if c.sinceProbe >= c.recheckBytes {
		return true
	}
	return time.Since(c.lastProbe) >= c.recheckInterval
}

func (c *Checker) markProbed() {
	c.mu.Lock()
	c.sinceProbe = 0
	c.lastProbe = time.Now()
	c.mu.Unlock()
}
