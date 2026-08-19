package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// membershipFakeDirectory is a SessionDirectory with working node/session
// lease enumeration and CAS semantics, backing the membership/OnLeave tests.
type membershipFakeDirectory struct {
	*fakeSessionDirectory

	mu              sync.Mutex
	memberLeases    map[string]*ClusterNodeLease
	sessionLeases   map[string]*ClusterSessionLease
	deletedSessions []string
}

func newMembershipFakeDirectory() *membershipFakeDirectory {
	return &membershipFakeDirectory{
		fakeSessionDirectory: &fakeSessionDirectory{},
		memberLeases:         make(map[string]*ClusterNodeLease),
		sessionLeases:        make(map[string]*ClusterSessionLease),
	}
}

func nodeLeaseMapKey(nodeID, incarnationID string) string { return nodeID + ":" + incarnationID }

func (d *membershipFakeDirectory) putNodeLease(lease *ClusterNodeLease) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.memberLeases[nodeLeaseMapKey(lease.NodeID, lease.IncarnationID)] = lease
}

func (d *membershipFakeDirectory) deleteNodeLease(nodeID, incarnationID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.memberLeases, nodeLeaseMapKey(nodeID, incarnationID))
}

func (d *membershipFakeDirectory) ListNodeLeases(context.Context) ([]*ClusterNodeLease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	leases := make([]*ClusterNodeLease, 0, len(d.memberLeases))
	for _, lease := range d.memberLeases {
		leases = append(leases, lease)
	}
	return leases, nil
}

func (d *membershipFakeDirectory) ListSessionLeases(context.Context) ([]*ClusterSessionLease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	leases := make([]*ClusterSessionLease, 0, len(d.sessionLeases))
	for _, lease := range d.sessionLeases {
		leases = append(leases, lease)
	}
	return leases, nil
}

func (d *membershipFakeDirectory) putSessionLease(lease *ClusterSessionLease) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessionLeases[lease.SessionID] = lease
}

// DeleteSessionLease mirrors the Redis directory: the lease is removed and
// the user index membership goes with it.
func (d *membershipFakeDirectory) DeleteSessionLease(ctx context.Context, sessionID string) error {
	d.mu.Lock()
	lease := d.sessionLeases[sessionID]
	delete(d.sessionLeases, sessionID)
	d.deletedSessions = append(d.deletedSessions, sessionID)
	d.mu.Unlock()
	if lease != nil && lease.UserID != "" {
		return d.RemoveUserSession(ctx, lease.UserID, sessionID)
	}
	return nil
}

// CompareAndSwapSessionLease applies CAS semantics over the session lease map
// (CAS(nil) is the first-registration claim).
func (d *membershipFakeDirectory) CompareAndSwapSessionLease(_ context.Context, expected, desired *ClusterSessionLease, _ time.Duration) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	current := d.sessionLeases[desired.SessionID]
	if !fakeLeaseEqual(current, expected) {
		return false, nil
	}
	d.sessionLeases[desired.SessionID] = desired
	return true, nil
}

func (d *membershipFakeDirectory) hasSessionLease(sessionID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.sessionLeases[sessionID]
	return ok
}

// TestClusterRepairer_OnLeaveInvalidatesDeadIncarnation is §7.12: once a
// node's lease disappears, the next membership beat deletes that
// incarnation's session leases (no 600s wait) and a CAS(nil) first
// registration succeeds afterwards.
func TestClusterRepairer_OnLeaveInvalidatesDeadIncarnation(t *testing.T) {
	ctx := context.Background()
	directory := newMembershipFakeDirectory()
	directory.putNodeLease(&ClusterNodeLease{NodeID: "node-self", IncarnationID: "inc-self", ExpiresAt: time.Now().Add(time.Minute)})
	directory.putNodeLease(&ClusterNodeLease{NodeID: "node-b", IncarnationID: "inc-b", ExpiresAt: time.Now().Add(time.Minute)})
	directory.putSessionLease(&ClusterSessionLease{
		SessionID: "sess-b", NodeID: "node-b", IncarnationID: "inc-b",
		UserID: "U1", LeaseVersion: 3, ExpiresAt: time.Now().Add(10 * time.Minute),
	})
	directory.putSessionLease(&ClusterSessionLease{
		SessionID: "sess-self", NodeID: "node-self", IncarnationID: "inc-self",
		UserID: "U2", LeaseVersion: 1, ExpiresAt: time.Now().Add(10 * time.Minute),
	})
	store := &repairTestQueryStore{}

	var leaves []string
	repairer := NewClusterRepairer(nil, directory, store, ClusterRepairerConfig{
		NodeID:        "node-self",
		IncarnationID: "inc-self",
		OnLeave:       func(nodeID, incarnationID string) { leaves = append(leaves, nodeID+"/"+incarnationID) },
	}).(*clusterRepairer)

	// First beat only primes the alive set — no OnLeave may fire.
	require.NoError(t, repairer.membershipOnce(ctx))
	assert.Empty(t, leaves)
	assert.True(t, directory.hasSessionLease("sess-b"))

	// node-b dies: its node lease disappears.
	directory.deleteNodeLease("node-b", "inc-b")
	require.NoError(t, repairer.membershipOnce(ctx))

	require.Equal(t, []string{"node-b/inc-b"}, leaves, "the departed incarnation must OnLeave exactly once")
	assert.False(t, directory.hasSessionLease("sess-b"),
		"the dead incarnation's session fencing must be invalidated immediately")
	assert.True(t, directory.hasSessionLease("sess-self"), "our own leases must be untouched")
	assert.Contains(t, directory.removedUsers, userSessionEntry{userID: "U1", sessionID: "sess-b"},
		"deleting the lease must sync the user index")
	assert.Contains(t, store.deleted, ClusterNodeProjection{NodeID: "node-b", IncarnationID: "inc-b"},
		"the dead incarnation's owner projection must be dropped")

	// The fencing is gone: a first-registration CAS(nil) succeeds.
	ok, err := directory.CompareAndSwapSessionLease(ctx, nil, &ClusterSessionLease{
		SessionID: "sess-b", NodeID: "node-c", IncarnationID: "inc-c", LeaseVersion: 1,
	}, time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "CAS(nil) must succeed once the dead incarnation's lease is deleted")

	// A later beat with no change must not OnLeave again.
	require.NoError(t, repairer.membershipOnce(ctx))
	require.Len(t, leaves, 1)
}

// TestClusterRepairer_NeverOnLeavesSelf: even if our own node lease is
// missing from a scan (a SCAN hiccup must not invalidate our own fencing).
func TestClusterRepairer_NeverOnLeavesSelf(t *testing.T) {
	ctx := context.Background()
	directory := newMembershipFakeDirectory()
	directory.putNodeLease(&ClusterNodeLease{NodeID: "node-self", IncarnationID: "inc-self", ExpiresAt: time.Now().Add(time.Minute)})
	directory.putSessionLease(&ClusterSessionLease{
		SessionID: "sess-self", NodeID: "node-self", IncarnationID: "inc-self",
		LeaseVersion: 1, ExpiresAt: time.Now().Add(10 * time.Minute),
	})

	var leaves []string
	repairer := NewClusterRepairer(nil, directory, nil, ClusterRepairerConfig{
		NodeID:        "node-self",
		IncarnationID: "inc-self",
		OnLeave:       func(nodeID, incarnationID string) { leaves = append(leaves, nodeID+"/"+incarnationID) },
	}).(*clusterRepairer)

	require.NoError(t, repairer.membershipOnce(ctx))
	directory.deleteNodeLease("node-self", "inc-self")
	require.NoError(t, repairer.membershipOnce(ctx))

	assert.Empty(t, leaves, "the repairer must never OnLeave its own incarnation")
	assert.True(t, directory.hasSessionLease("sess-self"))
	assert.Empty(t, directory.deletedSessions)
}

// TestClusterRepairer_ExpiredNodeLeaseCountsAsLeft: a lease record whose
// ExpiresAt has passed counts as departed even though the key still exists.
func TestClusterRepairer_ExpiredNodeLeaseCountsAsLeft(t *testing.T) {
	ctx := context.Background()
	directory := newMembershipFakeDirectory()
	directory.putNodeLease(&ClusterNodeLease{NodeID: "node-self", IncarnationID: "inc-self", ExpiresAt: time.Now().Add(time.Minute)})
	directory.putNodeLease(&ClusterNodeLease{NodeID: "node-b", IncarnationID: "inc-b", ExpiresAt: time.Now().Add(time.Minute)})
	directory.putSessionLease(&ClusterSessionLease{
		SessionID: "sess-b", NodeID: "node-b", IncarnationID: "inc-b",
		LeaseVersion: 1, ExpiresAt: time.Now().Add(10 * time.Minute),
	})

	var leaves []string
	repairer := NewClusterRepairer(nil, directory, nil, ClusterRepairerConfig{
		NodeID:        "node-self",
		IncarnationID: "inc-self",
		OnLeave:       func(nodeID, incarnationID string) { leaves = append(leaves, nodeID+"/"+incarnationID) },
	}).(*clusterRepairer)

	require.NoError(t, repairer.membershipOnce(ctx))
	// The lease record lingers but is expired.
	directory.putNodeLease(&ClusterNodeLease{NodeID: "node-b", IncarnationID: "inc-b", ExpiresAt: time.Now().Add(-time.Second)})
	require.NoError(t, repairer.membershipOnce(ctx))

	require.Equal(t, []string{"node-b/inc-b"}, leaves)
	assert.False(t, directory.hasSessionLease("sess-b"))
}

// TestClusterRepairer_Lifecycle covers Start/Shutdown of the single ticker
// (short membership period, long repair period) with no fixed sleeps.
func TestClusterRepairer_Lifecycle(t *testing.T) {
	directory := newMembershipFakeDirectory()
	directory.putNodeLease(&ClusterNodeLease{NodeID: "node-self", IncarnationID: "inc-self", ExpiresAt: time.Now().Add(time.Minute)})

	repairer := NewClusterRepairer(nil, directory, nil, ClusterRepairerConfig{
		NodeID:             "node-self",
		IncarnationID:      "inc-self",
		Interval:           time.Hour,
		MembershipInterval: 5 * time.Millisecond,
	})
	require.IsType(t, &clusterRepairer{}, repairer)
	require.NoError(t, repairer.Start(context.Background()))
	require.NoError(t, repairer.Start(context.Background()), "Start must be idempotent")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, repairer.Shutdown(shutdownCtx))
	require.NoError(t, repairer.Shutdown(shutdownCtx), "Shutdown must be idempotent")
}

// TestNewCluster_DerivesSingleRepairer pins §5.3: NewCluster owns exactly one
// repairer, derived from the session directory when the caller does not wire
// one; a directory without lease enumeration yields the no-op repairer.
func TestNewCluster_DerivesSingleRepairer(t *testing.T) {
	t.Parallel()

	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: &fakeSessionDirectory{},
	})
	require.NoError(t, err)
	require.IsType(t, &noopClusterRepairer{}, runtime.deps.Repairer,
		"a non-listing directory gets the no-op repairer")

	listing, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: newMembershipFakeDirectory(),
	})
	require.NoError(t, err)
	require.IsType(t, &clusterRepairer{}, listing.deps.Repairer,
		"a lease-listing directory gets the real repairer")

	// Exactly one repairer is part of the lifecycle components.
	count := 0
	for _, component := range listing.components() {
		if component == listing.deps.Repairer {
			count++
		}
	}
	require.Equal(t, 1, count, "NewCluster must start exactly one repairer")
}
