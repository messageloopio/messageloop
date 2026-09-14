package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ownerDeletingDirectory is a fake SessionDirectory that implements the
// atomic compare-and-delete (SessionLeaseOwnerDeleter) over a lease map,
// mirroring the Redis Lua script: the delete only lands while the stored
// lease still names the expected owner. The afterGetSessionLease hook lets a
// test mutate the map between deleteClusterSessionState's ownership read and
// its delete — the exact GET/CAS-DEL interleaving the atomic path must
// survive.
type ownerDeletingDirectory struct {
	*fakeSessionDirectory

	mu            sync.Mutex
	sessionLeases map[string]*ClusterSessionLease

	// recorded calls, in order
	ownerDeletes []ownerDeleteCall
	plainDeletes []string

	afterGetSessionLease func()
}

type ownerDeleteCall struct {
	sessionID     string
	nodeID        string
	incarnationID string
	deleted       bool
}

func newOwnerDeletingDirectory() *ownerDeletingDirectory {
	return &ownerDeletingDirectory{sessionLeases: make(map[string]*ClusterSessionLease)}
}

func (d *ownerDeletingDirectory) putSessionLease(lease *ClusterSessionLease) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessionLeases[lease.SessionID] = lease
}

// GetSessionLease reads the map and then runs the staleness hook, so a test
// can re-own the lease between this read and the subsequent compare-delete.
func (d *ownerDeletingDirectory) GetSessionLease(_ context.Context, sessionID string) (*ClusterSessionLease, error) {
	d.mu.Lock()
	lease := d.sessionLeases[sessionID]
	hook := d.afterGetSessionLease
	d.mu.Unlock()
	if hook != nil {
		hook()
	}
	return lease, nil
}

// DeleteSessionLeaseIfOwner mirrors the Redis Lua compare-and-delete: the
// owner compare and the delete commit atomically under the directory mutex.
func (d *ownerDeletingDirectory) DeleteSessionLeaseIfOwner(_ context.Context, sessionID, nodeID, incarnationID string) (bool, error) {
	d.mu.Lock()
	lease := d.sessionLeases[sessionID]
	deleted := lease != nil && lease.NodeID == nodeID && lease.IncarnationID == incarnationID
	if deleted {
		delete(d.sessionLeases, sessionID)
	}
	d.ownerDeletes = append(d.ownerDeletes, ownerDeleteCall{
		sessionID: sessionID, nodeID: nodeID, incarnationID: incarnationID, deleted: deleted,
	})
	d.mu.Unlock()
	return deleted, nil
}

// DeleteSessionLease records the non-atomic fallback: after this change no
// owner-aware call site may reach it anymore.
func (d *ownerDeletingDirectory) DeleteSessionLease(_ context.Context, sessionID string) error {
	d.mu.Lock()
	d.plainDeletes = append(d.plainDeletes, sessionID)
	d.mu.Unlock()
	return nil
}

func (d *ownerDeletingDirectory) hasSessionLease(sessionID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.sessionLeases[sessionID]
	return ok
}

// TestNode_DeleteClusterSessionState_CASDeleteOwnerMatch verifies the atomic
// path: for a lease the node still owns, the delete goes through
// DeleteSessionLeaseIfOwner with the node's own (nodeID, incarnationID), and
// the plain non-atomic delete is never touched.
func TestNode_DeleteClusterSessionState_CASDeleteOwnerMatch(t *testing.T) {
	directory := newOwnerDeletingDirectory()
	directory.putSessionLease(&ClusterSessionLease{
		SessionID: "sess-own", NodeID: "node-a", IncarnationID: "inc-a",
		LeaseVersion: 2, ExpiresAt: time.Now().Add(time.Hour),
	})
	node := ownerDeleteTestNode(t, directory)

	require.NoError(t, node.deleteClusterSessionState(context.Background(), "sess-own"))

	require.Equal(t, []ownerDeleteCall{{
		sessionID: "sess-own", nodeID: "node-a", incarnationID: "inc-a", deleted: true,
	}}, directory.ownerDeletes, "the delete must CAS on this node's own fencing")
	assert.Empty(t, directory.plainDeletes, "the owner-aware path must not fall back to the plain delete")
	assert.False(t, directory.hasSessionLease("sess-own"))
}

// TestNode_DeleteClusterSessionState_CASDeleteSurvivesTakeoverBetweenReadAndDelete
// is the core #14 regression: the ownership read sees this node's lease, then
// — before the delete — a peer's resume CAS re-owns the lease (bumped
// version, new owner). The stale reader must delete nothing.
func TestNode_DeleteClusterSessionState_CASDeleteSurvivesTakeoverBetweenReadAndDelete(t *testing.T) {
	directory := newOwnerDeletingDirectory()
	directory.putSessionLease(&ClusterSessionLease{
		SessionID: "sess-race", NodeID: "node-a", IncarnationID: "inc-a",
		LeaseVersion: 1, ExpiresAt: time.Now().Add(time.Hour),
	})
	// Re-own the lease right after deleteClusterSessionState's read: the
	// resume CAS bumped the version and handed the session to node-b.
	directory.afterGetSessionLease = func() {
		directory.putSessionLease(&ClusterSessionLease{
			SessionID: "sess-race", NodeID: "node-b", IncarnationID: "inc-b",
			LeaseVersion: 2, ExpiresAt: time.Now().Add(time.Hour),
		})
	}
	node := ownerDeleteTestNode(t, directory)

	require.NoError(t, node.deleteClusterSessionState(context.Background(), "sess-race"))

	require.Len(t, directory.ownerDeletes, 1)
	assert.False(t, directory.ownerDeletes[0].deleted,
		"the stale reader's compare-delete must lose against the new owner")
	assert.Empty(t, directory.plainDeletes)
	lease := directory.sessionLeases["sess-race"]
	require.NotNil(t, lease)
	assert.Equal(t, "node-b", lease.NodeID, "the new owner's lease must stay intact")
	assert.Equal(t, uint64(2), lease.LeaseVersion)
}

// TestNode_DeleteClusterSessionState_CASDeleteMissingLease verifies cleanup
// of an absent lease: the compare-delete is still issued (with the node's own
// fencing, deleting=false) and the snapshot cleanup proceeds.
func TestNode_DeleteClusterSessionState_CASDeleteMissingLease(t *testing.T) {
	directory := newOwnerDeletingDirectory()
	node := ownerDeleteTestNode(t, directory)

	require.NoError(t, node.deleteClusterSessionState(context.Background(), "sess-missing"))

	require.Equal(t, []ownerDeleteCall{{
		sessionID: "sess-missing", nodeID: "node-a", incarnationID: "inc-a", deleted: false,
	}}, directory.ownerDeletes)
	assert.Empty(t, directory.plainDeletes)
}

// TestClusterRepairer_OnLeave_CASDeleteKeepsTakenOverLease verifies the #14
// fix on the membership path: between the lease SCAN and the OnLeave deletes,
// a live node took one of the dead incarnation's sessions over (resume CAS).
// The taken-over lease must survive; only the lease still naming the dead
// incarnation is deleted.
func TestClusterRepairer_OnLeave_CASDeleteKeepsTakenOverLease(t *testing.T) {
	ctx := context.Background()
	directory := newMembershipFakeDirectory()
	directory.putNodeLease(&ClusterNodeLease{NodeID: "node-self", IncarnationID: "inc-self", ExpiresAt: time.Now().Add(time.Minute)})
	directory.putNodeLease(&ClusterNodeLease{NodeID: "node-b", IncarnationID: "inc-b", ExpiresAt: time.Now().Add(time.Minute)})
	// sess-gone stays with the dead incarnation; sess-taken is re-owned by
	// node-c below, between the priming beat and the OnLeave beat.
	directory.putSessionLease(&ClusterSessionLease{
		SessionID: "sess-gone", NodeID: "node-b", IncarnationID: "inc-b",
		UserID: "U1", LeaseVersion: 3, ExpiresAt: time.Now().Add(10 * time.Minute),
	})
	directory.putSessionLease(&ClusterSessionLease{
		SessionID: "sess-taken", NodeID: "node-b", IncarnationID: "inc-b",
		UserID: "U2", LeaseVersion: 5, ExpiresAt: time.Now().Add(10 * time.Minute),
	})

	repairer := NewClusterRepairer(nil, directory, nil, ClusterRepairerConfig{
		NodeID:        "node-self",
		IncarnationID: "inc-self",
	}).(*clusterRepairer)

	// First beat primes the alive set.
	require.NoError(t, repairer.membershipOnce(ctx))

	// node-b's lease disappears AND node-c takes sess-taken over (the same
	// interleaving a live cluster produces during a crash + resume race).
	directory.deleteNodeLease("node-b", "inc-b")
	directory.mu.Lock()
	directory.sessionLeases["sess-taken"] = &ClusterSessionLease{
		SessionID: "sess-taken", NodeID: "node-c", IncarnationID: "inc-c",
		UserID: "U2", LeaseVersion: 6, ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	directory.mu.Unlock()

	require.NoError(t, repairer.membershipOnce(ctx))

	assert.False(t, directory.hasSessionLease("sess-gone"),
		"the lease still naming the dead incarnation must be deleted")
	assert.True(t, directory.hasSessionLease("sess-taken"),
		"the taken-over lease must survive the stale OnLeave snapshot")
	assert.Equal(t, []string{"sess-gone"}, directory.deletedSessions)
	assert.Contains(t, directory.removedUsers, userSessionEntry{userID: "U1", sessionID: "sess-gone"})
	assert.NotContains(t, directory.removedUsers, userSessionEntry{userID: "U2", sessionID: "sess-taken"},
		"the new owner's user index membership must stay")
}

// fallbackListerDirectory is a SessionDirectory with lease enumeration but
// WITHOUT the SessionLeaseOwnerDeleter extension, mirroring third-party
// directories written before the extension: OnLeave must keep the plain
// delete for them (backward compatibility).
type fallbackListerDirectory struct {
	*fakeSessionDirectory

	mu            sync.Mutex
	sessionLeases map[string]*ClusterSessionLease
	nodeLeases    map[string]*ClusterNodeLease
	plainDeletes  []string
}

func (d *fallbackListerDirectory) putSessionLease(lease *ClusterSessionLease) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessionLeases[lease.SessionID] = lease
}

func (d *fallbackListerDirectory) hasSessionLease(sessionID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.sessionLeases[sessionID]
	return ok
}

func (d *fallbackListerDirectory) ListSessionLeases(context.Context) ([]*ClusterSessionLease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	leases := make([]*ClusterSessionLease, 0, len(d.sessionLeases))
	for _, lease := range d.sessionLeases {
		leases = append(leases, lease)
	}
	return leases, nil
}

func (d *fallbackListerDirectory) putNodeLease(lease *ClusterNodeLease) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nodeLeases[lease.NodeID+":"+lease.IncarnationID] = lease
}

func (d *fallbackListerDirectory) deleteNodeLease(nodeID, incarnationID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.nodeLeases, nodeID+":"+incarnationID)
}

func (d *fallbackListerDirectory) ListNodeLeases(context.Context) ([]*ClusterNodeLease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	leases := make([]*ClusterNodeLease, 0, len(d.nodeLeases))
	for _, lease := range d.nodeLeases {
		leases = append(leases, lease)
	}
	return leases, nil
}

func (d *fallbackListerDirectory) DeleteSessionLease(_ context.Context, sessionID string) error {
	d.mu.Lock()
	delete(d.sessionLeases, sessionID)
	d.plainDeletes = append(d.plainDeletes, sessionID)
	d.mu.Unlock()
	return nil
}

// TestClusterRepairer_OnLeave_FallbackWithoutOwnerDeleter pins the backward
// compatibility: a directory that implements the lease listing but NOT
// SessionLeaseOwnerDeleter keeps the plain delete on OnLeave.
func TestClusterRepairer_OnLeave_FallbackWithoutOwnerDeleter(t *testing.T) {
	ctx := context.Background()
	directory := &fallbackListerDirectory{
		fakeSessionDirectory: &fakeSessionDirectory{},
		sessionLeases:        make(map[string]*ClusterSessionLease),
		nodeLeases:           make(map[string]*ClusterNodeLease),
	}
	directory.putSessionLease(&ClusterSessionLease{
		SessionID: "sess-b", NodeID: "node-b", IncarnationID: "inc-b",
		LeaseVersion: 1, ExpiresAt: time.Now().Add(10 * time.Minute),
	})

	var iface SessionDirectory = directory
	_, implements := iface.(SessionLeaseOwnerDeleter)
	require.False(t, implements, "precondition: the fallback fake must not implement the extension")

	repairer := NewClusterRepairer(nil, directory, nil, ClusterRepairerConfig{
		NodeID:        "node-self",
		IncarnationID: "inc-self",
	}).(*clusterRepairer)
	// Prime the membership with node-b alive, then let it depart.
	directory.putNodeLease(&ClusterNodeLease{NodeID: "node-b", IncarnationID: "inc-b", ExpiresAt: time.Now().Add(time.Minute)})
	require.NoError(t, repairer.membershipOnce(ctx))
	directory.deleteNodeLease("node-b", "inc-b")
	require.NoError(t, repairer.membershipOnce(ctx))

	assert.False(t, directory.hasSessionLease("sess-b"),
		"the fallback path must still delete the dead incarnation's lease")
	assert.Equal(t, []string{"sess-b"}, directory.plainDeletes)
}

// ownerDeleteTestNode wires a node with identity node-a/inc-a over the given
// directory (same wiring as clusterTestNode).
func ownerDeleteTestNode(t *testing.T, directory SessionDirectory) *Node {
	t.Helper()
	return clusterTestNode(t, directory)
}
