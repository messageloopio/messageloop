package cluster

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSessionDirectory struct {
	mu        sync.Mutex
	lease     *ClusterSessionLease
	snapshot  *ClusterSessionSnapshot
	nodeLease *ClusterNodeLease
	// nodeLeases overrides nodeLease lookups keyed by "nodeID:incarnationID".
	nodeLeases map[string]*ClusterNodeLease
	// leases overrides GetSessionLease lookups keyed by session ID.
	leases map[string]*ClusterSessionLease
	// nodeLeaseErr makes GetNodeLease fail (simulating an unreachable store).
	nodeLeaseErr error

	// CAS bookkeeping (see CompareAndSwapSessionLease).
	casCalls     int
	casExpected  *ClusterSessionLease
	casDesired   *ClusterSessionLease
	forceCasFail bool
	// casErr makes CompareAndSwapSessionLease fail with a store error.
	casErr error

	// user index state: userSessions backs ListUserSessions and is mutated
	// by Add/RemoveUserSession; the call slices record every call for
	// assertions.
	userSessions map[string][]string
	addedUsers   []userSessionEntry
	removedUsers []userSessionEntry
}

type userSessionEntry struct {
	userID    string
	sessionID string
}

func (f *fakeSessionDirectory) Start(context.Context) error    { return nil }
func (f *fakeSessionDirectory) Shutdown(context.Context) error { return nil }
func (f *fakeSessionDirectory) PutNodeLease(context.Context, *ClusterNodeLease, time.Duration) error {
	return nil
}
func (f *fakeSessionDirectory) GetNodeLease(_ context.Context, nodeID, incarnationID string) (*ClusterNodeLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nodeLeaseErr != nil {
		return nil, f.nodeLeaseErr
	}
	if f.nodeLeases != nil {
		return f.nodeLeases[nodeID+":"+incarnationID], nil
	}
	return f.nodeLease, nil
}

// CompareAndSwapSessionLease simulates version-based CAS semantics: the swap
// only succeeds when the current lease matches the expected one. The check
// and the swap are atomic under the fake's mutex so concurrent CAS(nil)
// claims race like the real directory (exactly one wins). A nil current
// lease matches a nil expected lease (first registration).
func (f *fakeSessionDirectory) CompareAndSwapSessionLease(_ context.Context, expected, desired *ClusterSessionLease, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.casCalls++
	f.casExpected = expected
	f.casDesired = desired
	if f.forceCasFail {
		return false, nil
	}
	if f.casErr != nil {
		return false, f.casErr
	}
	if f.lease == nil && expected == nil {
		f.lease = desired
		return true, nil
	}
	if !fakeLeaseEqual(f.lease, expected) {
		return false, nil
	}
	f.lease = desired
	return true, nil
}

// fakeLeaseEqual mirrors the redis session directory's lease comparison
// (SessionID/NodeID/IncarnationID/LeaseVersion).
func fakeLeaseEqual(left, right *ClusterSessionLease) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.SessionID == right.SessionID &&
		left.NodeID == right.NodeID &&
		left.IncarnationID == right.IncarnationID &&
		left.LeaseVersion == right.LeaseVersion
}

func (f *fakeSessionDirectory) GetSessionLease(_ context.Context, sessionID string) (*ClusterSessionLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leases != nil {
		return f.leases[sessionID], nil
	}
	return f.lease, nil
}
func (f *fakeSessionDirectory) DeleteSessionLease(context.Context, string) error { return nil }
func (f *fakeSessionDirectory) PutSessionSnapshot(context.Context, *ClusterSessionSnapshot, time.Duration) error {
	return nil
}
func (f *fakeSessionDirectory) GetSessionSnapshot(context.Context, string) (*ClusterSessionSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot, nil
}
func (f *fakeSessionDirectory) DeleteSessionSnapshot(context.Context, string) error { return nil }

// AddUserSession records the membership and backs ListUserSessions.
func (f *fakeSessionDirectory) AddUserSession(_ context.Context, userID, sessionID string, _ time.Duration) error {
	f.addedUsers = append(f.addedUsers, userSessionEntry{userID: userID, sessionID: sessionID})
	if userID == "" || sessionID == "" {
		return nil
	}
	if f.userSessions == nil {
		f.userSessions = make(map[string][]string)
	}
	f.userSessions[userID] = append(f.userSessions[userID], sessionID)
	return nil
}

// RemoveUserSession drops the membership and backs ListUserSessions.
func (f *fakeSessionDirectory) RemoveUserSession(_ context.Context, userID, sessionID string) error {
	f.removedUsers = append(f.removedUsers, userSessionEntry{userID: userID, sessionID: sessionID})
	if f.userSessions == nil {
		return nil
	}
	sessions := f.userSessions[userID]
	for i, sid := range sessions {
		if sid == sessionID {
			f.userSessions[userID] = append(sessions[:i], sessions[i+1:]...)
			break
		}
	}
	if len(f.userSessions[userID]) == 0 {
		delete(f.userSessions, userID)
	}
	return nil
}

func (f *fakeSessionDirectory) ListUserSessions(_ context.Context, userID string) ([]string, error) {
	if f.userSessions == nil {
		return nil, nil
	}
	return append([]string(nil), f.userSessions[userID]...), nil
}

// TestSyncUserIndex_MigratesOnUserChange verifies the helper's migration
// rule: when a lease changes user (resume + re-authentication), the old
// user's membership is removed and the new user's added; a same-user refresh
// never removes the membership.
func TestSyncUserIndex_MigratesOnUserChange(t *testing.T) {
	ctx := context.Background()
	directory := &fakeSessionDirectory{}
	old := &ClusterSessionLease{SessionID: "sess-1", UserID: "U1"}
	new := &ClusterSessionLease{SessionID: "sess-1", UserID: "U2"}

	require.NoError(t, directory.AddUserSession(ctx, "U1", "sess-1", time.Minute))
	require.NoError(t, SyncUserIndex(ctx, directory, old, new, time.Minute))

	ids, err := directory.ListUserSessions(ctx, "U1")
	require.NoError(t, err)
	assert.NotContains(t, ids, "sess-1", "U1 must no longer list sess-1 after the user change")

	ids, err = directory.ListUserSessions(ctx, "U2")
	require.NoError(t, err)
	assert.Contains(t, ids, "sess-1", "U2 must list sess-1 after the user change")

	// Same-user refresh keeps the membership and never removes it.
	removedBefore := len(directory.removedUsers)
	require.NoError(t, SyncUserIndex(ctx, directory, new, new, time.Minute))
	assert.Equal(t, removedBefore, len(directory.removedUsers), "same-user Put must not remove the membership")
	ids, err = directory.ListUserSessions(ctx, "U2")
	require.NoError(t, err)
	assert.Contains(t, ids, "sess-1")
}

// TestSyncUserIndex_DeleteRemovesMembership verifies the Delete path
// (newLease == nil) and the anonymous-lease rule: an empty UserID only ever
// removes, never adds.
func TestSyncUserIndex_DeleteRemovesMembership(t *testing.T) {
	ctx := context.Background()
	directory := &fakeSessionDirectory{}
	lease := &ClusterSessionLease{SessionID: "sess-1", UserID: "U1"}
	require.NoError(t, directory.AddUserSession(ctx, "U1", "sess-1", time.Minute))

	require.NoError(t, SyncUserIndex(ctx, directory, lease, nil, 0))
	ids, err := directory.ListUserSessions(ctx, "U1")
	require.NoError(t, err)
	assert.NotContains(t, ids, "sess-1", "Delete must remove the membership")

	// A lease that became anonymous must leave the index and never re-enter.
	require.NoError(t, SyncUserIndex(ctx, directory,
		&ClusterSessionLease{SessionID: "sess-2", UserID: "U1"},
		&ClusterSessionLease{SessionID: "sess-2", UserID: ""},
		time.Minute))
	for _, entry := range directory.addedUsers {
		assert.NotEqual(t, "sess-2", entry.sessionID, "anonymous sessions must never enter the index")
	}
	ids, err = directory.ListUserSessions(ctx, "U1")
	require.NoError(t, err)
	assert.NotContains(t, ids, "sess-2", "a lease that became anonymous must leave the index")
}
