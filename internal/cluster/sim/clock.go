// Package sim is an in-process, deterministic simulator for the cluster
// fencing contract (PR-KA-C1, KD-K20). It wires two real *messageloop.Node
// instances to a shared in-memory Directory (real CAS semantics) and a
// scriptable in-memory command bus (synchronous delivery with explicit
// Hold/Drop/Flush orchestration), so the Bind/Evict/Fence scenarios can be
// locked in as regression tests without Redis, without time.Sleep, and
// without random scheduling.
//
// The simulator never changes the production fencing algorithm: nodes in a
// World run the same syncClusterSessionState / resumeRemoteSession / Fence
// code paths as production. Incarnation IDs are scripted (inc-a / inc-b),
// never uuid.New.
package sim

import (
	"fmt"
	"sync"
	"time"
)

// Clock is a manually advanced wall clock for simulator fixtures. It is
// monotonic by construction: Advance rejects non-positive deltas, so time
// never moves backwards.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a Clock fixed at start.
func NewClock(start time.Time) *Clock {
	return &Clock{now: start}
}

// Now returns the current simulated time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d. d must be positive: a zero or
// negative delta is rejected (the clock is not moved), so fixtures can never
// rewind time.
func (c *Clock) Advance(d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("sim clock: advance delta must be positive, got %s", d)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return nil
}
