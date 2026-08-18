package sim

import (
	"context"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/stretchr/testify/require"
)

func testLease(sessionID, nodeID, incarnationID string, version uint64) *messageloop.ClusterSessionLease {
	return &messageloop.ClusterSessionLease{
		SessionID:     sessionID,
		NodeID:        nodeID,
		IncarnationID: incarnationID,
		LeaseVersion:  version,
		ExpiresAt:     time.Now().Add(time.Hour),
	}
}

// TestDirectory_CASNilClaimsOnce: the first CAS(nil) claims the empty slot;
// a second CAS(nil) fails and leaves the winner's lease untouched.
func TestDirectory_CASNilClaimsOnce(t *testing.T) {
	dir := NewDirectory()
	ctx := context.Background()

	ok, err := dir.CompareAndSwapSessionLease(ctx, nil, testLease("sess-1", "node-a", "inc-a", 1), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = dir.CompareAndSwapSessionLease(ctx, nil, testLease("sess-1", "node-b", "inc-b", 1), time.Minute)
	require.NoError(t, err)
	require.False(t, ok, "second CAS(nil) must lose")

	lease, err := dir.GetSessionLease(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, "node-a", lease.NodeID)
	require.Equal(t, "inc-a", lease.IncarnationID)
	require.Equal(t, uint64(1), lease.LeaseVersion)
}

// TestDirectory_SameFenceRefresh: a CAS whose expected matches the current
// record on all four fencing fields succeeds (same-fence refresh); a stale
// version fails and changes nothing.
func TestDirectory_SameFenceRefresh(t *testing.T) {
	dir := NewDirectory()
	ctx := context.Background()

	ok, err := dir.CompareAndSwapSessionLease(ctx, nil, testLease("sess-1", "node-a", "inc-a", 1), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	current, err := dir.GetSessionLease(ctx, "sess-1")
	require.NoError(t, err)

	refresh := testLease("sess-1", "node-a", "inc-a", 1)
	refresh.UserID = "user-1"
	refresh.LastActivityAt = 42
	ok, err = dir.CompareAndSwapSessionLease(ctx, current, refresh, time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "same-fence refresh must succeed")

	lease, err := dir.GetSessionLease(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, "user-1", lease.UserID)
	require.Equal(t, int64(42), lease.LastActivityAt)
	require.Equal(t, uint64(1), lease.LeaseVersion, "refresh keeps the version")

	// A stale expected record (wrong version) must lose, value unchanged.
	stale := testLease("sess-1", "node-a", "inc-a", 99)
	ok, err = dir.CompareAndSwapSessionLease(ctx, stale, testLease("sess-1", "node-a", "inc-a", 100), time.Minute)
	require.NoError(t, err)
	require.False(t, ok)
	lease, err = dir.GetSessionLease(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, uint64(1), lease.LeaseVersion)
}

// TestDirectory_TakeoverBumpsVersion: the winner's CAS(expected=A v1,
// desired=B v2) succeeds exactly like production resumeRemoteSession.
func TestDirectory_TakeoverBumpsVersion(t *testing.T) {
	dir := NewDirectory()
	ctx := context.Background()

	ok, err := dir.CompareAndSwapSessionLease(ctx, nil, testLease("sess-1", "node-a", "inc-a", 1), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	current, err := dir.GetSessionLease(ctx, "sess-1")
	require.NoError(t, err)
	ok, err = dir.CompareAndSwapSessionLease(ctx, current, testLease("sess-1", "node-b", "inc-b", 2), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	lease, err := dir.GetSessionLease(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, "node-b", lease.NodeID)
	require.Equal(t, uint64(2), lease.LeaseVersion)

	// The loser cannot write its stale fencing back.
	ok, err = dir.CompareAndSwapSessionLease(ctx, testLease("sess-1", "node-a", "inc-a", 1), testLease("sess-1", "node-a", "inc-a", 1), time.Minute)
	require.NoError(t, err)
	require.False(t, ok)
	lease, err = dir.GetSessionLease(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, "node-b", lease.NodeID)
}

// TestDirectory_MultiSessionIsolation: leases are keyed per session; claiming
// one session must not disturb another.
func TestDirectory_MultiSessionIsolation(t *testing.T) {
	dir := NewDirectory()
	ctx := context.Background()

	ok, err := dir.CompareAndSwapSessionLease(ctx, nil, testLease("sess-1", "node-a", "inc-a", 1), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = dir.CompareAndSwapSessionLease(ctx, nil, testLease("sess-2", "node-b", "inc-b", 1), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	// CAS(nil) on sess-1 fails even though sess-2 exists elsewhere.
	ok, err = dir.CompareAndSwapSessionLease(ctx, nil, testLease("sess-1", "node-b", "inc-b", 1), time.Minute)
	require.NoError(t, err)
	require.False(t, ok)

	leases, err := dir.ListSessionLeases(ctx)
	require.NoError(t, err)
	require.Len(t, leases, 2)
}

// TestDirectory_DeleteSessionLeaseSyncsUserIndex: deleting a lease drops its
// user-index membership, like the Redis directory.
func TestDirectory_DeleteSessionLeaseSyncsUserIndex(t *testing.T) {
	dir := NewDirectory()
	ctx := context.Background()

	lease := testLease("sess-1", "node-a", "inc-a", 1)
	lease.UserID = "user-1"
	ok, err := dir.CompareAndSwapSessionLease(ctx, nil, lease, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, dir.AddUserSession(ctx, "user-1", "sess-1", time.Minute))

	sessions, err := dir.ListUserSessions(ctx, "user-1")
	require.NoError(t, err)
	require.Equal(t, []string{"sess-1"}, sessions)

	require.NoError(t, dir.DeleteSessionLease(ctx, "sess-1"))

	stored, err := dir.GetSessionLease(ctx, "sess-1")
	require.NoError(t, err)
	require.Nil(t, stored)
	sessions, err = dir.ListUserSessions(ctx, "user-1")
	require.NoError(t, err)
	require.Empty(t, sessions)
	require.Equal(t, []string{"sess-1"}, dir.DeletedSessionLeases())
}

// TestDirectory_NodeLeases: node leases key on (NodeID, IncarnationID) and
// the fixture delete removes exactly one record.
func TestDirectory_NodeLeases(t *testing.T) {
	dir := NewDirectory()
	ctx := context.Background()

	put := func(nodeID, incarnationID string) {
		require.NoError(t, dir.PutNodeLease(ctx, &messageloop.ClusterNodeLease{
			NodeID:        nodeID,
			IncarnationID: incarnationID,
			ExpiresAt:     time.Now().Add(time.Hour),
		}, time.Minute))
	}
	put("node-a", "inc-a")
	put("node-a", "inc-a2")
	put("node-b", "inc-b")

	leases, err := dir.ListNodeLeases(ctx)
	require.NoError(t, err)
	require.Len(t, leases, 3)

	dir.DeleteNodeLease("node-a", "inc-a")
	lease, err := dir.GetNodeLease(ctx, "node-a", "inc-a")
	require.NoError(t, err)
	require.Nil(t, lease)
	lease, err = dir.GetNodeLease(ctx, "node-a", "inc-a2")
	require.NoError(t, err)
	require.NotNil(t, lease, "same node, other incarnation must survive")
}

// TestDirectory_SnapshotRoundTrip: snapshots store per session and deep-copy
// on the way in and out.
func TestDirectory_SnapshotRoundTrip(t *testing.T) {
	dir := NewDirectory()
	ctx := context.Background()

	snapshot := &messageloop.ClusterSessionSnapshot{
		SessionID:      "sess-1",
		UserID:         "user-1",
		Subscriptions:  []messageloop.ClusterSubscriptionSnapshot{{Channel: "news"}},
		ChannelOffsets: map[string]uint64{"news": 7},
	}
	require.NoError(t, dir.PutSessionSnapshot(ctx, snapshot, time.Minute))
	snapshot.ChannelOffsets["news"] = 99 // mutate caller state after Put

	stored, err := dir.GetSessionSnapshot(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, "user-1", stored.UserID)
	require.Equal(t, uint64(7), stored.ChannelOffsets["news"], "stored snapshot must not alias caller state")

	require.NoError(t, dir.DeleteSessionSnapshot(ctx, "sess-1"))
	stored, err = dir.GetSessionSnapshot(ctx, "sess-1")
	require.NoError(t, err)
	require.Nil(t, stored)
}

// TestDirectory_CompareAndSwapSessionStateAtomic: the combined CAS writes the
// lease and the snapshot under one lock (PR-KA-D10 §1.2) — a won compare
// stores both, a lost compare stores neither.
func TestDirectory_CompareAndSwapSessionStateAtomic(t *testing.T) {
	dir := NewDirectory()
	ctx := context.Background()

	// First registration: expected == nil on an empty slot writes both.
	ok, err := dir.CompareAndSwapSessionState(ctx, nil, testLease("sess-1", "node-a", "inc-a", 1),
		&messageloop.ClusterSessionSnapshot{SessionID: "sess-1", UserID: "user-1"}, time.Minute, time.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	snapshot, err := dir.GetSessionSnapshot(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, "user-1", snapshot.UserID, "the snapshot lands with the winning CAS")

	// Same-fence refresh: lease and snapshot move together.
	current, err := dir.GetSessionLease(ctx, "sess-1")
	require.NoError(t, err)
	refresh := testLease("sess-1", "node-a", "inc-a", 1)
	refresh.LastActivityAt = 42
	ok, err = dir.CompareAndSwapSessionState(ctx, current, refresh,
		&messageloop.ClusterSessionSnapshot{SessionID: "sess-1", UserID: "user-2"}, time.Minute, time.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	snapshot, err = dir.GetSessionSnapshot(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, "user-2", snapshot.UserID)

	// A stale compare writes neither key: the lease keeps the winner's record
	// and the snapshot keeps the last won view.
	ok, err = dir.CompareAndSwapSessionState(ctx, testLease("sess-1", "node-a", "inc-a", 99),
		testLease("sess-1", "node-a", "inc-a", 100),
		&messageloop.ClusterSessionSnapshot{SessionID: "sess-1", UserID: "user-stale"}, time.Minute, time.Hour)
	require.NoError(t, err)
	require.False(t, ok)
	lease, err := dir.GetSessionLease(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, uint64(1), lease.LeaseVersion)
	snapshot, err = dir.GetSessionSnapshot(ctx, "sess-1")
	require.NoError(t, err)
	require.Equal(t, "user-2", snapshot.UserID, "a lost compare must not touch the snapshot")
}
