package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: MarkMetricsCharged used to call TransportLabel() (RLock) while
// holding c.mu (write lock) on the SessionClosed branch. A Go RWMutex is not
// reentrant, so whenever Close won the race against AddClient the call
// self-deadlocked and the session was wedged forever.

// TestMarkMetricsCharged_CloseCompletedFirst pins the deterministic ordering:
// Close completes before MarkMetricsCharged runs, so the SessionClosed branch
// executes while the write lock is held. The gauge increment AddClient raced
// in must be undone, without re-acquiring the lock this goroutine holds.
func TestMarkMetricsCharged_CloseCompletedFirst(t *testing.T) {
	node := newFakeRuntime()
	sess, _, err := NewClient(context.Background(), node, newScriptedTransport(), JSONMarshaler{})
	require.NoError(t, err)

	// Simulate the racy AddClient increment (node.AddClient Inc's the gauge
	// before the caller reaches MarkMetricsCharged), then close first.
	node.metrics.ConnectionsTotal.WithLabelValues("ws").Inc()
	require.NoError(t, sess.Close(Disconnect{}))
	require.Equal(t, float64(1), testutil.ToFloat64(node.metrics.ConnectionsTotal.WithLabelValues("ws")),
		"Close without metricsCharged must not decrement the gauge")

	// Must return (and undo the drifted increment) instead of self-deadlocking.
	done := make(chan struct{})
	go func() {
		defer close(done)
		sess.MarkMetricsCharged()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("MarkMetricsCharged deadlocked against Close: the SessionClosed branch re-acquired c.mu while holding the write lock")
	}
	assert.Equal(t, float64(0), testutil.ToFloat64(node.metrics.ConnectionsTotal.WithLabelValues("ws")),
		"the closed-connection branch must undo the gauge increment AddClient raced in")
}

// TestMarkMetricsCharged_LiveSession_ChargesAndCloses pins the other branch:
// on a live session the call only marks the connection as charged, and the
// later Close is the one that decrements the gauge.
func TestMarkMetricsCharged_LiveSession_ChargesAndCloses(t *testing.T) {
	node := newFakeRuntime()
	sess, _, err := NewClient(context.Background(), node, newScriptedTransport(), JSONMarshaler{})
	require.NoError(t, err)

	node.metrics.ConnectionsTotal.WithLabelValues("ws").Inc()
	sess.MarkMetricsCharged()

	sess.mu.RLock()
	charged := sess.metricsCharged
	sess.mu.RUnlock()
	assert.True(t, charged, "a live session must be marked as metrics-charged")
	assert.Equal(t, float64(1), testutil.ToFloat64(node.metrics.ConnectionsTotal.WithLabelValues("ws")))

	require.NoError(t, sess.Close(Disconnect{}))
	assert.Equal(t, float64(0), testutil.ToFloat64(node.metrics.ConnectionsTotal.WithLabelValues("ws")),
		"the Close of a charged session must decrement the gauge")
}

// TestMarkMetricsCharged_CloseRace_NoDeadlock interleaves MarkMetricsCharged
// with Close many times: whichever order the two goroutines land in, both
// calls must always complete (race detector exercising the shared session
// state). With the re-entrancy bug the closed-first ordering wedged forever.
func TestMarkMetricsCharged_CloseRace_NoDeadlock(t *testing.T) {
	for i := 0; i < 200; i++ {
		node := newFakeRuntime()
		sess, _, err := NewClient(context.Background(), node, newScriptedTransport(), JSONMarshaler{})
		require.NoError(t, err)

		done := make(chan struct{})
		go func() {
			defer close(done)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				_ = sess.Close(Disconnect{})
			}()
			go func() {
				defer wg.Done()
				sess.MarkMetricsCharged()
			}()
			wg.Wait()
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("MarkMetricsCharged deadlocked against Close at iteration %d", i)
		}
	}
}
