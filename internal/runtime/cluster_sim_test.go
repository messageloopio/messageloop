package runtime_test

// PR-KA-C1 constitution scenarios (spec §5): the fencing contract locked in
// on the deterministic two-node simulator (internal/cluster/sim). These tests
// never touch Redis and never sleep: delivery is synchronous, lost Evicts are
// scripted with Bus.DropNext, and membership beats are driven by direct
// SimMembershipOnce calls.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/internal/cluster"
	"github.com/messageloopio/messageloop/internal/cluster/sim"
	"github.com/messageloopio/messageloop/internal/protocol"
	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/internal/session"
	"github.com/messageloopio/messageloop/shared"
)

// simNoopTransport backs the re-attachment in the LocalDetachAttach scenario.
type simNoopTransport struct{}

func (simNoopTransport) Write([]byte) error              { return nil }
func (simNoopTransport) WriteMany(...[]byte) error       { return nil }
func (simNoopTransport) Close(protocol.Disconnect) error { return nil }
func (simNoopTransport) RemoteAddr() string              { return "sim" }

// attachedCount counts, across both nodes, how many hubs hold sessionID in
// the Attached state. The fencing contract allows at most one at any time.
func attachedCount(w *sim.World, sessionID string) int {
	count := 0
	for _, node := range []*runtime.Node{w.A, w.B} {
		if s := node.Hub().LookupSession(sessionID); s != nil && s.State() == session.SessionAttached {
			count++
		}
	}
	return count
}

// requireLease asserts the Directory's current fencing for sessionID.
func requireLease(t *testing.T, w *sim.World, sessionID, nodeID, incarnationID string, version uint64) {
	t.Helper()
	lease, err := w.Dir.GetSessionLease(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.Equal(t, nodeID, lease.NodeID)
	require.Equal(t, incarnationID, lease.IncarnationID)
	require.Equal(t, version, lease.LeaseVersion)
}

// TestSim_StealThenPing (§5.1): A owns sess-1; B wins the cross-node resume
// (CAS v1→v2 + takeover). A's next sync — the ping refresh — must be fenced
// and must not write A's lease back over B's.
func TestSim_StealThenPing(t *testing.T) {
	w := sim.NewWorld()
	ctx := context.Background()

	aClient, err := w.AddClient(w.A, "sess-1", "user-1", "client-1")
	require.NoError(t, err)
	requireLease(t, w, "sess-1", "node-a", "inc-a", 1)

	// B binds the same session: the real resume path claims the lease and the
	// synchronous bus delivers the takeover to A.
	bClient, err := w.NewResumeClient(w.B)
	require.NoError(t, err)
	_, resumed, err := runtime.SimResumeRemoteSession(w.B, ctx, bClient, "sess-1", "user-1")
	require.NoError(t, err)
	require.True(t, resumed)
	requireLease(t, w, "sess-1", "node-b", "inc-b", 2)

	// A's next ping refresh is fenced and writes nothing back.
	err = runtime.SimSyncClusterSessionState(w.A, ctx, aClient)
	require.ErrorIs(t, err, cluster.ErrSessionFenced)
	requireLease(t, w, "sess-1", "node-b", "inc-b", 2)
}

// TestSim_BindThenEvictFences (§5.2): with the bus in default synchronous
// mode, B's Bind delivers the Evict to A: A's session is Fenced (Closed, not
// Detached), evicted from A's hub, and A never unbinds B's lease.
func TestSim_BindThenEvictFences(t *testing.T) {
	w := sim.NewWorld()
	ctx := context.Background()

	aClient, err := w.AddClient(w.A, "sess-1", "user-1", "client-1")
	require.NoError(t, err)
	require.Equal(t, session.SessionAttached, aClient.State())

	bClient, err := w.NewResumeClient(w.B)
	require.NoError(t, err)
	_, resumed, err := runtime.SimResumeRemoteSession(w.B, ctx, bClient, "sess-1", "user-1")
	require.NoError(t, err)
	require.True(t, resumed)

	require.Equal(t, session.SessionClosed, aClient.State(), "evicted session is Fenced, not Detached")
	require.Nil(t, w.A.Hub().LookupSession("sess-1"), "fenced session leaves A's hub")
	requireLease(t, w, "sess-1", "node-b", "inc-b", 2)
	require.Empty(t, w.Dir.DeletedSessionLeases(), "a fenced node must not unbind the new owner's lease")
}

// TestSim_LostEvictNoDual (§5.3): the Evict is dropped, so A keeps serving
// until its next sync observes the fencing loss — but the Directory already
// belongs to B, and at no step is the session Attached on both nodes.
func TestSim_LostEvictNoDual(t *testing.T) {
	w := sim.NewWorld()
	ctx := context.Background()

	aClient, err := w.AddClient(w.A, "sess-1", "user-1", "client-1")
	require.NoError(t, err)

	w.Bus.DropNext() // the takeover Evict to A vanishes
	bClient, err := w.NewResumeClient(w.B)
	require.NoError(t, err)
	_, resumed, err := runtime.SimResumeRemoteSession(w.B, ctx, bClient, "sess-1", "user-1")
	require.NoError(t, err)
	require.True(t, resumed, "the CAS claim survives the lost Evict (the old node has no live node lease)")

	// The Directory is authoritative: B owns the fencing even though A's
	// local attachment is still up (the Evict never arrived).
	requireLease(t, w, "sess-1", "node-b", "inc-b", 2)
	require.Equal(t, session.SessionAttached, aClient.State())
	require.LessOrEqual(t, attachedCount(w, "sess-1"), 1)

	// A learns of the fencing loss on its next sync and fences itself.
	err = runtime.SimSyncClusterSessionState(w.A, ctx, aClient)
	require.ErrorIs(t, err, cluster.ErrSessionFenced)
	require.NoError(t, aClient.Fence(protocol.DisconnectStale))
	require.Equal(t, session.SessionClosed, aClient.State())
	require.Nil(t, w.A.Hub().LookupSession("sess-1"))

	// Only now does B attach the resumed session: the single Attached copy.
	require.NoError(t, w.AttachResumed(w.B, bClient, "sess-1", "user-1", "client-1"))
	require.Equal(t, 1, attachedCount(w, "sess-1"))
	require.Same(t, bClient, w.B.Hub().LookupSession("sess-1"))
	requireLease(t, w, "sess-1", "node-b", "inc-b", 2)
	require.Empty(t, w.Dir.DeletedSessionLeases())
}

// TestSim_LocalDetachAttach (§5.4): a same-node resume window (Detach →
// Attach) keeps the Session pointer, the hub entry, and the Directory
// fencing; it is not a Fence.
func TestSim_LocalDetachAttach(t *testing.T) {
	w := sim.NewWorld()

	client, err := w.AddClient(w.A, "sess-4", "user-4", "client-4")
	require.NoError(t, err)
	require.Equal(t, session.SessionAttached, client.State())

	client.Detach(protocol.Disconnect{Code: 3000, Reason: "transport swap"})
	require.Equal(t, session.SessionDetached, client.State(), "local handover is Detach, never Fence")
	requireLease(t, w, "sess-4", "node-a", "inc-a", 1)

	require.NoError(t, client.Attach(&session.Attachment{
		Transport: simNoopTransport{},
		Marshaler: shared.JSONMarshaler{},
		Protocol:  "ws",
	}))
	require.Equal(t, session.SessionAttached, client.State())

	// The hub entry is the very same Session object, and the Directory
	// fencing never changed hands or version.
	require.Same(t, client, w.A.Hub().LookupSession("sess-4"))
	requireLease(t, w, "sess-4", "node-a", "inc-a", 1)
	require.Empty(t, w.Dir.DeletedSessionLeases())
}

// TestSim_DeadNodeOnLeave (§5.5): once A's node lease is gone, the second
// membership beat (the first only primes the alive set) fires OnLeave and
// invalidates A's session fencing, so B can claim sess-1 with CAS(nil).
func TestSim_DeadNodeOnLeave(t *testing.T) {
	w := sim.NewWorld()
	ctx := context.Background()

	_, err := w.AddClient(w.A, "sess-1", "user-1", "client-1")
	require.NoError(t, err)
	require.NoError(t, w.Dir.AddUserSession(ctx, "user-1", "sess-1", time.Hour))

	// Both incarnations are alive in the Directory.
	putNodeLease := func(nodeID, incarnationID string) {
		require.NoError(t, w.Dir.PutNodeLease(ctx, &cluster.ClusterNodeLease{
			NodeID:        nodeID,
			IncarnationID: incarnationID,
			StartedAt:     time.Now(),
			ExpiresAt:     time.Now().Add(time.Hour),
		}, time.Hour))
	}
	putNodeLease("node-a", "inc-a")
	putNodeLease("node-b", "inc-b")

	// First beat only primes the alive set.
	require.NoError(t, runtime.SimMembershipOnce(w.RepairerB, ctx))
	requireLease(t, w, "sess-1", "node-a", "inc-a", 1)

	// A dies; the next beat fires OnLeave and deletes A's session fencing.
	w.Dir.DeleteNodeLease("node-a", "inc-a")
	require.NoError(t, runtime.SimMembershipOnce(w.RepairerB, ctx))

	lease, err := w.Dir.GetSessionLease(ctx, "sess-1")
	require.NoError(t, err)
	require.Nil(t, lease, "OnLeave invalidates the dead incarnation's fencing")
	sessions, err := w.Dir.ListUserSessions(ctx, "user-1")
	require.NoError(t, err)
	require.Empty(t, sessions, "DeleteSessionLease syncs the user index")

	// B may claim the session immediately — no 600s TTL wait.
	ok, err := w.Dir.CompareAndSwapSessionLease(ctx, nil, &cluster.ClusterSessionLease{
		SessionID:     "sess-1",
		NodeID:        "node-b",
		IncarnationID: "inc-b",
		LeaseVersion:  1,
		ExpiresAt:     time.Now().Add(time.Hour),
	}, time.Hour)
	require.NoError(t, err)
	require.True(t, ok, "CAS(nil) succeeds after OnLeave")
	requireLease(t, w, "sess-1", "node-b", "inc-b", 1)
}

// TestSim_CasNilOnlyOneWins (§5.6): two nodes race CAS(nil) for the same
// session on the shared Directory; exactly one claim succeeds and the loser
// cannot overwrite the winner's lease.
func TestSim_CasNilOnlyOneWins(t *testing.T) {
	w := sim.NewWorld()
	ctx := context.Background()

	claim := func(nodeID, incarnationID string) *cluster.ClusterSessionLease {
		return &cluster.ClusterSessionLease{
			SessionID:     "sess-1",
			NodeID:        nodeID,
			IncarnationID: incarnationID,
			LeaseVersion:  1,
			ExpiresAt:     time.Now().Add(time.Hour),
		}
	}

	results := make([]bool, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, owner := range [][2]string{{"node-a", "inc-a"}, {"node-b", "inc-b"}} {
		wg.Add(1)
		go func(i int, nodeID, incarnationID string) {
			defer wg.Done()
			<-start
			ok, err := w.Dir.CompareAndSwapSessionLease(ctx, nil, claim(nodeID, incarnationID), time.Hour)
			require.NoError(t, err)
			results[i] = ok
		}(i, owner[0], owner[1])
	}
	close(start)
	wg.Wait()

	require.True(t, results[0] != results[1], "exactly one CAS(nil) may win: %v", results)
	winner := "node-a"
	if results[1] {
		winner = "node-b"
	}
	lease, err := w.Dir.GetSessionLease(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, winner, lease.NodeID, "the loser must not overwrite the winner's lease")
	require.Equal(t, uint64(1), lease.LeaseVersion)
}
