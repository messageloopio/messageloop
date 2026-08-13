package messageloop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// recordingNodeLeaseDirectory records the node leases written by the lease
// manager.
type recordingNodeLeaseDirectory struct {
	fakeSessionDirectory
	mu    sync.Mutex
	lease *ClusterNodeLease
	ttl   time.Duration
	puts  int
}

func (d *recordingNodeLeaseDirectory) PutNodeLease(_ context.Context, lease *ClusterNodeLease, ttl time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lease = lease
	d.ttl = ttl
	d.puts++
	return nil
}

func (d *recordingNodeLeaseDirectory) last() (*ClusterNodeLease, time.Duration, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lease, d.ttl, d.puts
}

// TestClusterNodeLeaseManager_Lifecycle verifies the lease manager writes a
// node lease on Start and keeps renewing it.
func TestClusterNodeLeaseManager_Lifecycle(t *testing.T) {
	directory := &recordingNodeLeaseDirectory{}
	manager := NewClusterNodeLeaseManager(directory, ClusterNodeLeaseManagerConfig{
		NodeID:        "node-a",
		IncarnationID: "inc-a",
		TTL:           200 * time.Millisecond,
		RenewInterval: 20 * time.Millisecond,
	})
	require.NoError(t, manager.Start(context.Background()))

	require.Eventually(t, func() bool {
		_, _, puts := directory.last()
		return puts >= 2
	}, 2*time.Second, 10*time.Millisecond)

	lease, ttl, _ := directory.last()
	require.NotNil(t, lease)
	require.Equal(t, "node-a", lease.NodeID)
	require.Equal(t, "inc-a", lease.IncarnationID)
	require.Equal(t, 200*time.Millisecond, ttl)
	require.False(t, lease.ExpiresAt.IsZero())

	require.NoError(t, manager.Shutdown(context.Background()))
	// Shutdown is idempotent.
	require.NoError(t, manager.Shutdown(context.Background()))
}

// failingLeaseDirectory fails every PutNodeLease.
type failingLeaseDirectory struct {
	fakeSessionDirectory
}

func (failingLeaseDirectory) PutNodeLease(context.Context, *ClusterNodeLease, time.Duration) error {
	return errors.New("injected lease write failure")
}

// TestClusterNodeLeaseManager_StartFailsOnInitialRenewalError verifies the
// start path: when the initial lease write fails, Start returns the error
// and leaves no background goroutine behind.
func TestClusterNodeLeaseManager_StartFailsOnInitialRenewalError(t *testing.T) {
	manager := NewClusterNodeLeaseManager(&failingLeaseDirectory{}, ClusterNodeLeaseManagerConfig{
		NodeID:        "node-a",
		IncarnationID: "inc-a",
	})
	err := manager.Start(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "injected lease write failure")
	require.NoError(t, manager.Shutdown(context.Background()))
}

// flakyLeaseDirectory fails PutNodeLease after the first success.
type flakyLeaseDirectory struct {
	fakeSessionDirectory
	mu    sync.Mutex
	calls int
}

func (d *flakyLeaseDirectory) PutNodeLease(context.Context, *ClusterNodeLease, time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.calls > 1 {
		return errors.New("injected lease renewal failure")
	}
	return nil
}

// TestClusterNodeLeaseManager_RenewalFailureDoesNotStopManager verifies the
// renewal-failure path: a failed renewal is logged but must not stop the
// manager; Shutdown still completes promptly.
func TestClusterNodeLeaseManager_RenewalFailureDoesNotStopManager(t *testing.T) {
	directory := &flakyLeaseDirectory{}
	manager := NewClusterNodeLeaseManager(directory, ClusterNodeLeaseManagerConfig{
		NodeID:        "node-a",
		IncarnationID: "inc-a",
		TTL:           200 * time.Millisecond,
		RenewInterval: 10 * time.Millisecond,
	})
	require.NoError(t, manager.Start(context.Background()))

	// Let several renewal ticks fail (each failure is logged, not fatal).
	time.Sleep(80 * time.Millisecond)
	require.NoError(t, manager.Shutdown(context.Background()))
	directory.mu.Lock()
	calls := directory.calls
	directory.mu.Unlock()
	require.GreaterOrEqual(t, calls, 3, "renewals must keep ticking after failures")
}

// TestNode_ClusterSessionSnapshot_IncludesBrokerEpoch verifies that the
// snapshot carries the broker epoch so a resuming node can detect history
// invalidation.
func TestNode_ClusterSessionSnapshot_IncludesBrokerEpoch(t *testing.T) {
	node := NewNode(nil)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-epoch", "user-epoch", "client-epoch")
	require.NoError(t, node.AddClient(client))

	snapshot := node.clusterSessionSnapshot(client)
	require.NotEmpty(t, snapshot.BrokerEpoch, "snapshot must carry the broker epoch")
	epochBroker, ok := node.broker.(interface{ Epoch() string })
	require.True(t, ok)
	require.Equal(t, epochBroker.Epoch(), snapshot.BrokerEpoch)
}
