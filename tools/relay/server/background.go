package server

import (
	"log"
	"runtime/debug"
	"sync"

	"github.com/mirror-db/mirror-db/tools/relay/activecache"
)

// bgTasks tracks detached post-delivery work: the ordinary chain's store tee and
// the active segment's publication waiter. A handler must return as soon as the
// client has every byte, so the work that outlives it may not be a bare
// fire-and-forget goroutine: Close stops new registrations and waits for the
// running ones before the hash store is closed, so nothing writes to the index
// or the bucket after that point.
type bgTasks struct {
	mu      sync.Mutex
	closed  bool
	pending int
	// idle is closed when pending drops to zero. It is replaced, not reused,
	// each time work starts again.
	idle chan struct{}
	// closing is closed when Close begins. A task that is waiting for its
	// handler (the ordinary tee) selects on it instead of blocking on a slow
	// client, so shutdown is bounded by the work and not by the network.
	closing chan struct{}
}

func newBgTasks() *bgTasks {
	b := &bgTasks{idle: make(chan struct{}), closing: make(chan struct{})}
	close(b.idle)
	return b
}

// start registers one task and runs it on its own goroutine. It returns false
// when the server is already closing; the caller then does nothing, because the
// response it just delivered is complete. Registration and the closed check are
// one critical section, so Close can never wait for a task it did not see.
func (b *bgTasks) start(fn func()) bool {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return false
	}
	if b.pending == 0 {
		b.idle = make(chan struct{})
	}
	b.pending++
	b.mu.Unlock()
	go func() {
		// A detached task has no caller to return an error to. A panic in
		// variant building, metadata cleanup or a publication hook must not
		// take down a relay that has already answered the client, so it is
		// logged with its stack and the task is settled normally.
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("relay background task panic: %v\n%s", recovered, debug.Stack())
			}
			b.done()
		}()
		fn()
	}()
	return true
}

func (b *bgTasks) done() {
	b.mu.Lock()
	b.pending--
	if b.pending == 0 {
		close(b.idle)
	}
	b.mu.Unlock()
}

// drain waits until no task is registered. A task that starts after an
// observation of zero is the next caller's work, not part of this batch.
func (b *bgTasks) drain() {
	for {
		b.mu.Lock()
		if b.pending == 0 {
			b.mu.Unlock()
			return
		}
		idle := b.idle
		b.mu.Unlock()
		<-idle
	}
}

// close refuses new tasks, tells the running ones to stop waiting for their
// handler, and waits for them to finish.
func (b *bgTasks) close() {
	b.mu.Lock()
	if !b.closed {
		b.closed = true
		close(b.closing)
	}
	b.mu.Unlock()
	b.drain()
}

// stopping returns the channel closed when Close begins. It is stable for the
// lifetime of the set, so a task can capture it once and select on it.
func (b *bgTasks) stopping() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closing
}

// background returns the server's task set, creating it on first use. A Server
// built directly in a test may leave the field nil.
func (s *Server) background() *bgTasks {
	s.bgOnce.Do(func() {
		if s.bg == nil {
			s.bg = newBgTasks()
		}
	})
	return s.bg
}

// activeCommitSet deduplicates one publication waiter per active entry. A
// complete delivery from every reader of the same assembly would otherwise
// schedule its own wait and its own variant commit; one entry only ever needs
// one. The entry pointer is safe as a key: the manager keeps it alive until it
// settles, and the waiter observes settlement rather than cache state.
type activeCommitSet struct {
	mu   sync.Mutex
	seen map[*activecache.Entry]struct{}
}

// begin registers entry and reports whether this caller now owns its waiter.
func (c *activeCommitSet) begin(entry *activecache.Entry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = make(map[*activecache.Entry]struct{})
	}
	if _, ok := c.seen[entry]; ok {
		return false
	}
	c.seen[entry] = struct{}{}
	return true
}

func (c *activeCommitSet) finish(entry *activecache.Entry) {
	c.mu.Lock()
	delete(c.seen, entry)
	c.mu.Unlock()
}

// size reports the waiters currently registered. It is a test observation.
func (c *activeCommitSet) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}
