package sim

import (
	"context"
	"sync"
	"time"

	"github.com/messageloopio/messageloop/internal/cluster"
)

// Directory is the simulator's authoritative in-memory session directory: one
// instance is shared by every node in a World, exactly like the Redis
// directory in production. It implements cluster.SessionDirectory plus
// the lease listers the repairer needs (ClusterSessionLeaseLister and
// ClusterNodeLeaseLister).
//
// CompareAndSwapSessionLease uses the production equality predicate
// (SessionID, NodeID, IncarnationID, LeaseVersion — see fakeLeaseEqual /
// clusterSessionLeaseEqual) and performs the check and the swap atomically
// under one lock, so two concurrent CAS(nil) claims have exactly one winner.
// There is deliberately no unconditional Put: every lease write goes through
// the CAS, like the production hot paths.
type Directory struct {
	mu sync.Mutex

	sessionLeases map[string]*cluster.ClusterSessionLease
	snapshots     map[string]*cluster.ClusterSessionSnapshot
	nodeLeases    map[nodeIncarnation]*cluster.ClusterNodeLease
	users         map[string]map[string]struct{}

	// deletedSessionLeases records every DeleteSessionLease call, in order,
	// so tests can assert that a fenced node never unbound the new owner.
	deletedSessionLeases []string
}

type nodeIncarnation struct {
	nodeID        string
	incarnationID string
}

// NewDirectory returns an empty shared Directory.
func NewDirectory() *Directory {
	return &Directory{
		sessionLeases: make(map[string]*cluster.ClusterSessionLease),
		snapshots:     make(map[string]*cluster.ClusterSessionSnapshot),
		nodeLeases:    make(map[nodeIncarnation]*cluster.ClusterNodeLease),
		users:         make(map[string]map[string]struct{}),
	}
}

func (d *Directory) Start(context.Context) error    { return nil }
func (d *Directory) Shutdown(context.Context) error { return nil }

// PutNodeLease stores the node liveness record under (NodeID, IncarnationID).
func (d *Directory) PutNodeLease(_ context.Context, lease *cluster.ClusterNodeLease, _ time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	copy := *lease
	d.nodeLeases[nodeIncarnation{nodeID: lease.NodeID, incarnationID: lease.IncarnationID}] = &copy
	return nil
}

// GetNodeLease returns the stored node lease, or nil when absent.
func (d *Directory) GetNodeLease(_ context.Context, nodeID, incarnationID string) (*cluster.ClusterNodeLease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	lease := d.nodeLeases[nodeIncarnation{nodeID: nodeID, incarnationID: incarnationID}]
	if lease == nil {
		return nil, nil
	}
	copy := *lease
	return &copy, nil
}

// DeleteNodeLease drops a node liveness record. It is a fixture helper (the
// production SessionDirectory interface has no node-lease delete: records
// expire by TTL) used to script a dead node without waiting out the TTL.
func (d *Directory) DeleteNodeLease(nodeID, incarnationID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.nodeLeases, nodeIncarnation{nodeID: nodeID, incarnationID: incarnationID})
}

// ListNodeLeases enumerates every stored node lease (ClusterNodeLeaseLister).
func (d *Directory) ListNodeLeases(context.Context) ([]*cluster.ClusterNodeLease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	leases := make([]*cluster.ClusterNodeLease, 0, len(d.nodeLeases))
	for _, lease := range d.nodeLeases {
		copy := *lease
		leases = append(leases, &copy)
	}
	return leases, nil
}

// CompareAndSwapSessionLease atomically swaps the session lease when the
// current record equals expected on the fencing fields (SessionID, NodeID,
// IncarnationID, LeaseVersion). expected == nil matches an absent record
// (first registration). A failed compare leaves the stored lease untouched.
func (d *Directory) CompareAndSwapSessionLease(_ context.Context, expected, desired *cluster.ClusterSessionLease, _ time.Duration) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	current := d.sessionLeases[desired.SessionID]
	if !leaseEqual(current, expected) {
		return false, nil
	}
	copy := *desired
	d.sessionLeases[desired.SessionID] = &copy
	return true, nil
}

// CompareAndSwapSessionState implements the optional atomic lease+snapshot
// write (SessionStateCompareAndSwapper, PR-KA-D10 §1.2): the four-field
// compare, the lease swap and the snapshot store commit under the same lock,
// so a failed compare leaves both records untouched.
func (d *Directory) CompareAndSwapSessionState(_ context.Context, expected, desired *cluster.ClusterSessionLease, snapshot *cluster.ClusterSessionSnapshot, _, _ time.Duration) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	current := d.sessionLeases[desired.SessionID]
	if !leaseEqual(current, expected) {
		return false, nil
	}
	leaseCopy := *desired
	d.sessionLeases[desired.SessionID] = &leaseCopy
	if snapshot != nil {
		d.snapshots[snapshot.SessionID] = copySnapshot(snapshot)
	}
	return true, nil
}

// leaseEqual mirrors the production lease comparison: only the fencing
// fields (SessionID, NodeID, IncarnationID, LeaseVersion) participate.
func leaseEqual(current, expected *cluster.ClusterSessionLease) bool {
	if current == nil || expected == nil {
		return current == nil && expected == nil
	}
	return current.SessionID == expected.SessionID &&
		current.NodeID == expected.NodeID &&
		current.IncarnationID == expected.IncarnationID &&
		current.LeaseVersion == expected.LeaseVersion
}

// GetSessionLease returns the stored session lease, or nil when absent.
func (d *Directory) GetSessionLease(_ context.Context, sessionID string) (*cluster.ClusterSessionLease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	lease := d.sessionLeases[sessionID]
	if lease == nil {
		return nil, nil
	}
	copy := *lease
	return &copy, nil
}

// DeleteSessionLease drops the session lease and, like the Redis directory,
// syncs the user index: a lease with a UserID is removed from that user's
// session set.
func (d *Directory) DeleteSessionLease(_ context.Context, sessionID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if lease := d.sessionLeases[sessionID]; lease != nil && lease.UserID != "" {
		d.removeUserSessionLocked(lease.UserID, sessionID)
	}
	delete(d.sessionLeases, sessionID)
	d.deletedSessionLeases = append(d.deletedSessionLeases, sessionID)
	return nil
}

// DeletedSessionLeases returns the session IDs passed to DeleteSessionLease,
// in call order. Tests use it to prove a fenced node never unbound the new
// owner's lease.
func (d *Directory) DeletedSessionLeases() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.deletedSessionLeases...)
}

// ListSessionLeases enumerates every stored session lease
// (ClusterSessionLeaseLister).
func (d *Directory) ListSessionLeases(context.Context) ([]*cluster.ClusterSessionLease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	leases := make([]*cluster.ClusterSessionLease, 0, len(d.sessionLeases))
	for _, lease := range d.sessionLeases {
		copy := *lease
		leases = append(leases, &copy)
	}
	return leases, nil
}

// PutSessionSnapshot stores the resumable session state under SessionID.
func (d *Directory) PutSessionSnapshot(_ context.Context, snapshot *cluster.ClusterSessionSnapshot, _ time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.snapshots[snapshot.SessionID] = copySnapshot(snapshot)
	return nil
}

// GetSessionSnapshot returns the stored snapshot, or nil when absent.
func (d *Directory) GetSessionSnapshot(_ context.Context, sessionID string) (*cluster.ClusterSessionSnapshot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	snapshot := d.snapshots[sessionID]
	if snapshot == nil {
		return nil, nil
	}
	return copySnapshot(snapshot), nil
}

// DeleteSessionSnapshot drops the stored snapshot.
func (d *Directory) DeleteSessionSnapshot(_ context.Context, sessionID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.snapshots, sessionID)
	return nil
}

// copySnapshot deep-copies the mutable slices/maps so stored state is never
// aliased with caller state.
func copySnapshot(snapshot *cluster.ClusterSessionSnapshot) *cluster.ClusterSessionSnapshot {
	copy := *snapshot
	copy.Subscriptions = append([]cluster.ClusterSubscriptionSnapshot(nil), snapshot.Subscriptions...)
	if snapshot.ChannelOffsets != nil {
		copy.ChannelOffsets = make(map[string]uint64, len(snapshot.ChannelOffsets))
		for channel, offset := range snapshot.ChannelOffsets {
			copy.ChannelOffsets[channel] = offset
		}
	}
	if snapshot.AuthContext != nil {
		copy.AuthContext = make(map[string]string, len(snapshot.AuthContext))
		for key, value := range snapshot.AuthContext {
			copy.AuthContext[key] = value
		}
	}
	return &copy
}

// AddUserSession records that sessionID currently belongs to userID. Empty
// user IDs never enter the index.
func (d *Directory) AddUserSession(_ context.Context, userID, sessionID string, _ time.Duration) error {
	if userID == "" || sessionID == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	sessions := d.users[userID]
	if sessions == nil {
		sessions = make(map[string]struct{})
		d.users[userID] = sessions
	}
	sessions[sessionID] = struct{}{}
	return nil
}

// RemoveUserSession drops a session's membership from a user's index.
func (d *Directory) RemoveUserSession(_ context.Context, userID, sessionID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removeUserSessionLocked(userID, sessionID)
	return nil
}

func (d *Directory) removeUserSessionLocked(userID, sessionID string) {
	sessions := d.users[userID]
	if sessions == nil {
		return
	}
	delete(sessions, sessionID)
	if len(sessions) == 0 {
		delete(d.users, userID)
	}
}

// ListUserSessions returns the indexed session IDs of userID (a hint, never
// authoritative).
func (d *Directory) ListUserSessions(_ context.Context, userID string) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	sessions := d.users[userID]
	result := make([]string, 0, len(sessions))
	for sessionID := range sessions {
		result = append(result, sessionID)
	}
	return result, nil
}

var (
	_ cluster.SessionDirectory              = (*Directory)(nil)
	_ cluster.SessionStateCompareAndSwapper = (*Directory)(nil)
	_ cluster.ClusterSessionLeaseLister     = (*Directory)(nil)
	_ cluster.ClusterNodeLeaseLister        = (*Directory)(nil)
)
