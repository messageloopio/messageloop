package sim

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestClock_Advance: Advance moves the clock forward exactly.
func TestClock_Advance(t *testing.T) {
	start := time.Unix(1_700_000_000, 0).UTC()
	clock := NewClock(start)
	require.Equal(t, start, clock.Now())

	require.NoError(t, clock.Advance(5*time.Second))
	require.Equal(t, start.Add(5*time.Second), clock.Now())

	require.NoError(t, clock.Advance(time.Millisecond))
	require.Equal(t, start.Add(5*time.Second+time.Millisecond), clock.Now())
}

// TestClock_NeverRewinds: zero and negative deltas are rejected and the clock
// does not move.
func TestClock_NeverRewinds(t *testing.T) {
	start := time.Unix(1_700_000_000, 0).UTC()
	clock := NewClock(start)

	require.Error(t, clock.Advance(0))
	require.Equal(t, start, clock.Now())

	require.Error(t, clock.Advance(-time.Second))
	require.Equal(t, start, clock.Now())
}
