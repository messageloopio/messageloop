package messageloop

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

func (f *failingLeaseDirectory) PutNodeLease(context.Context, *ClusterNodeLease, time.Duration) error {
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

// --- PR-KA-A1: lease refresh via same-fence CAS (no blind Put) ---

// clusterTestNode wires a node with a fake in-memory session directory.
func clusterTestNode(t *testing.T, directory SessionDirectory) *Node {
	t.Helper()
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)
	node := NewNode(nil)
	node.SetCluster(runtime)
	return node
}

// TestClusterSessionSync_RefreshKeepsVersion verifies §6.1: two consecutive
// syncs of the same client refresh the lease TTL but never bump the version.
func TestClusterSessionSync_RefreshKeepsVersion(t *testing.T) {
	directory := &fakeSessionDirectory{}
	node := clusterTestNode(t, directory)

	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-refresh", "user-refresh", "client-refresh")
	require.NoError(t, node.AddClient(client))

	require.NoError(t, node.syncClusterSessionState(context.Background(), client))
	first, err := directory.GetSessionLease(context.Background(), "sess-refresh")
	require.NoError(t, err)
	require.NotNil(t, first)

	time.Sleep(time.Millisecond)
	require.NoError(t, node.syncClusterSessionState(context.Background(), client))
	second, err := directory.GetSessionLease(context.Background(), "sess-refresh")
	require.NoError(t, err)
	require.NotNil(t, second)

	require.Equal(t, first.LeaseVersion, second.LeaseVersion, "refresh must not bump the lease version")
	require.Equal(t, "node-a", second.NodeID)
	require.True(t, second.ExpiresAt.After(first.ExpiresAt), "refresh must extend the lease TTL")
}

// TestClusterSessionSync_FencedWhenAnotherOwnerWins verifies §6.2 (the core
// case): after node B claims the session with a newer version, node A's sync
// must fail with ErrSessionFenced and must not write A's lease back over B's.
func TestClusterSessionSync_FencedWhenAnotherOwnerWins(t *testing.T) {
	directory := &fakeSessionDirectory{}
	node := clusterTestNode(t, directory)

	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-stolen", "user-stolen", "client-stolen")
	require.NoError(t, node.AddClient(client))

	// Node B claims the session (version N+1) in the directory.
	directory.lease = &ClusterSessionLease{
		SessionID:     "sess-stolen",
		NodeID:        "node-b",
		IncarnationID: "inc-b",
		LeaseVersion:  2,
		ExpiresAt:     time.Now().Add(time.Hour),
	}

	err = node.syncClusterSessionState(context.Background(), client)
	require.ErrorIs(t, err, ErrSessionFenced)

	// The directory still holds B's lease, untouched.
	got, err := directory.GetSessionLease(context.Background(), "sess-stolen")
	require.NoError(t, err)
	require.Equal(t, "node-b", got.NodeID)
	require.Equal(t, uint64(2), got.LeaseVersion)
}

// TestClusterSessionSync_FirstCreate_CASClaimsEmptyDirectory verifies §6.3:
// the first sync on an empty directory creates the lease with version 1 via
// CAS(nil) — never a blind SET.
func TestClusterSessionSync_FirstCreate_CASClaimsEmptyDirectory(t *testing.T) {
	directory := &fakeSessionDirectory{}
	node := clusterTestNode(t, directory)

	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-first", "user-first", "client-first")
	require.NoError(t, node.AddClient(client))

	lease, err := directory.GetSessionLease(context.Background(), "sess-first")
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.Equal(t, "node-a", lease.NodeID)
	require.Equal(t, uint64(1), lease.LeaseVersion)
}

// TestClusterSessionSync_ConcurrentCASNilOnlyOneWins verifies §6.3: two
// concurrent CAS(nil) claims against an empty directory — exactly one wins.
func TestClusterSessionSync_ConcurrentCASNilOnlyOneWins(t *testing.T) {
	directory := &fakeSessionDirectory{}

	var successes atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claim := &ClusterSessionLease{
				SessionID:     "sess-race",
				NodeID:        "node-a",
				IncarnationID: "inc-a",
				LeaseVersion:  1,
				ExpiresAt:     time.Now().Add(time.Minute),
			}
			ok, err := directory.CompareAndSwapSessionLease(context.Background(), nil, claim, time.Minute)
			require.NoError(t, err)
			if ok {
				successes.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	require.Equal(t, int32(1), successes.Load(), "exactly one CAS(nil) claim may win")
	lease, err := directory.GetSessionLease(context.Background(), "sess-race")
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.Equal(t, uint64(1), lease.LeaseVersion)
}

// --- PR-KA-D3: fencing counters (bind_fenced_total / bind_refresh_fail_total) ---

// fencedMetricsTestClient builds a client on a metrics-wired cluster test
// node without registering it, so only the explicit syncClusterSessionState
// call under test can touch the fencing counters.
func fencedMetricsTestClient(t *testing.T, directory SessionDirectory) (*Node, *Metrics, *Client) {
	t.Helper()
	node := clusterTestNode(t, directory)
	metrics := NewMetrics(prometheus.NewRegistry())
	node.SetMetrics(metrics)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-fenced", "user-fenced", "client-fenced")
	return node, metrics, client
}

// TestClusterSessionSync_Metrics_FirstClaimFencedCounted verifies D3: a lost
// CAS(nil) first registration counts towards bind_fenced_total and never
// towards bind_refresh_fail_total.
func TestClusterSessionSync_Metrics_FirstClaimFencedCounted(t *testing.T) {
	directory := &fakeSessionDirectory{forceCasFail: true}
	node, metrics, client := fencedMetricsTestClient(t, directory)

	err := node.syncClusterSessionState(context.Background(), client)
	require.ErrorIs(t, err, ErrSessionFenced)
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.BindFencedTotal))
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.BindRefreshFailTotal))
}

// TestClusterSessionSync_Metrics_RefreshFencedCounted verifies D3: the three
// same-session refresh fencing paths (foreign fencing, newer directory
// version, lost refresh CAS) each count towards bind_refresh_fail_total and
// never towards bind_fenced_total.
func TestClusterSessionSync_Metrics_RefreshFencedCounted(t *testing.T) {
	cases := []struct {
		name      string
		directory *fakeSessionDirectory
	}{
		{
			name: "foreign fencing",
			directory: &fakeSessionDirectory{lease: &ClusterSessionLease{
				SessionID:     "sess-fenced",
				NodeID:        "node-b",
				IncarnationID: "inc-b",
				LeaseVersion:  2,
				ExpiresAt:     time.Now().Add(time.Hour),
			}},
		},
		{
			name: "newer directory version",
			directory: &fakeSessionDirectory{lease: &ClusterSessionLease{
				SessionID:     "sess-fenced",
				NodeID:        "node-a",
				IncarnationID: "inc-a",
				LeaseVersion:  5,
				ExpiresAt:     time.Now().Add(time.Hour),
			}},
		},
		{
			name: "lost refresh CAS",
			directory: &fakeSessionDirectory{
				forceCasFail: true,
				lease: &ClusterSessionLease{
					SessionID:     "sess-fenced",
					NodeID:        "node-a",
					IncarnationID: "inc-a",
					LeaseVersion:  1,
					ExpiresAt:     time.Now().Add(time.Hour),
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node, metrics, client := fencedMetricsTestClient(t, tc.directory)

			err := node.syncClusterSessionState(context.Background(), client)
			require.ErrorIs(t, err, ErrSessionFenced)
			require.Equal(t, float64(1), testutil.ToFloat64(metrics.BindRefreshFailTotal))
			require.Equal(t, float64(0), testutil.ToFloat64(metrics.BindFencedTotal))
		})
	}
}
