package messageloop

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExpandUserSessions_SkipsMismatchedLease verifies KD-13: cluster index
// entries are only hints. A session whose lease user no longer matches (or
// whose lease is gone) must not appear in the expansion and must not be
// acted upon.
func TestExpandUserSessions_SkipsMismatchedLease(t *testing.T) {
	ctx := context.Background()
	directory := &fakeSessionDirectory{
		userSessions: map[string][]string{
			"U": {"sess-ok", "sess-poisoned", "sess-gone"},
		},
		leases: map[string]*ClusterSessionLease{
			"sess-ok":       {SessionID: "sess-ok", UserID: "U"},
			"sess-poisoned": {SessionID: "sess-poisoned", UserID: "X"},
			// sess-gone intentionally has no lease.
		},
	}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)
	node := NewNode(nil)
	node.SetCluster(runtime)

	expanded := node.ExpandUserSessions(ctx, "U")
	assert.Equal(t, []string{"sess-ok"}, expanded,
		"mismatched and missing leases must be skipped, no full-cluster SCAN")

	// Expansion must not close or mutate anything: it only resolves IDs.
	assert.Empty(t, directory.removedUsers)
	assert.Len(t, directory.addedUsers, 0)
}

// TestExpandUserSessions_LocalOnlyWithoutCluster verifies that without a
// cluster the expansion relies solely on the local Hub.SessionsByUser and
// never touches a directory.
func TestExpandUserSessions_LocalOnlyWithoutCluster(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	client := newTestClient(t, "sess-local", "U")
	require.NoError(t, node.AddClient(client))

	expanded := node.ExpandUserSessions(ctx, "U")
	assert.Equal(t, []string{"sess-local"}, expanded)

	// Local hub entries are still trusted: a client whose UserID() no longer
	// matches the request (stale shard entry) is filtered by the authoritative
	// client check.
	client.mu.Lock()
	client.user = "other"
	client.mu.Unlock()
	assert.Empty(t, node.ExpandUserSessions(ctx, "U"),
		"Client.UserID is authoritative even for local hub entries")

	assert.Empty(t, node.ExpandUserSessions(ctx, ""), "empty user ID must not scan anything")
}

// repairListerDirectory backs the repairer test with lease enumeration.
type repairListerDirectory struct {
	*fakeSessionDirectory
	leases []*ClusterSessionLease
}

func (d *repairListerDirectory) ListSessionLeases(context.Context) ([]*ClusterSessionLease, error) {
	return d.leases, nil
}

// TestClusterUserIndexRepairer_RebuildsMemberships verifies the repairer's
// user-index pass: it SCANs the lease prefix and re-adds memberships for
// non-empty users, skipping anonymous and expired leases.
func TestClusterUserIndexRepairer_RebuildsMemberships(t *testing.T) {
	ctx := context.Background()
	directory := &repairListerDirectory{
		fakeSessionDirectory: &fakeSessionDirectory{},
		leases: []*ClusterSessionLease{
			{SessionID: "sess-1", UserID: "U1", ExpiresAt: time.Now().Add(time.Minute)},
			{SessionID: "sess-anon", UserID: "", ExpiresAt: time.Now().Add(time.Minute)},
			{SessionID: "sess-expired", UserID: "U3", ExpiresAt: time.Now().Add(-time.Second)},
		},
	}
	repairer := NewClusterRepairer(nil, directory, nil, ClusterRepairerConfig{})
	require.IsType(t, &clusterRepairer{}, repairer, "a lease-listing directory must not get the no-op repairer")

	// Drive the repair pass directly instead of waiting on the ticker.
	require.NoError(t, repairer.(*clusterRepairer).repairOnce(ctx))

	ids, err := directory.ListUserSessions(ctx, "U1")
	require.NoError(t, err)
	assert.Equal(t, []string{"sess-1"}, ids)
	ids, err = directory.ListUserSessions(ctx, "U3")
	require.NoError(t, err)
	assert.Empty(t, ids, "expired leases must not be re-added")
	for _, entry := range directory.addedUsers {
		assert.NotEqual(t, "sess-anon", entry.sessionID, "anonymous leases must never enter the index")
	}

	// A directory without lease enumeration (and no node/store) gets a no-op
	// repairer.
	noop := NewClusterRepairer(nil, &fakeSessionDirectory{}, nil, ClusterRepairerConfig{})
	require.IsType(t, &noopClusterRepairer{}, noop)
}
