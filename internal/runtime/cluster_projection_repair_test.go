package messageloop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

type repairTestQueryStore struct {
	err      error
	channels map[string]int64

	// projection reaping bookkeeping (Task 13d).
	projections []ClusterNodeProjection
	deleted     []ClusterNodeProjection
}

func (s *repairTestQueryStore) Start(context.Context) error    { return nil }
func (s *repairTestQueryStore) Shutdown(context.Context) error { return nil }
func (s *repairTestQueryStore) AdjustChannelSubscriptions(context.Context, string, int64, time.Duration) error {
	return nil
}
func (s *repairTestQueryStore) ReplaceNodeChannels(_ context.Context, channels map[string]int64, _ time.Duration) error {
	if s.err != nil {
		return s.err
	}
	s.channels = make(map[string]int64, len(channels))
	for key, value := range channels {
		s.channels[key] = value
	}
	return nil
}
func (s *repairTestQueryStore) ListChannels(context.Context) ([]ClusterChannelInfo, error) {
	return nil, nil
}
func (s *repairTestQueryStore) ListNodeProjections(context.Context) ([]ClusterNodeProjection, error) {
	return s.projections, nil
}
func (s *repairTestQueryStore) DeleteNodeProjection(_ context.Context, nodeID, incarnationID string) error {
	s.deleted = append(s.deleted, ClusterNodeProjection{NodeID: nodeID, IncarnationID: incarnationID})
	return nil
}

func TestClusterProjectionRepairer_RecordsSuccessfulRepairMetrics(t *testing.T) {
	node := NewNode(nil)
	registry := prometheus.NewRegistry()
	node.SetMetrics(NewMetrics(registry))
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-repair-metric", "user-repair-metric", "client-repair-metric")
	require.NoError(t, node.AddClient(client))
	require.NoError(t, node.AddSubscription(context.Background(), "repair.channel", NewSubscriber(client, false)))

	store := &repairTestQueryStore{}
	repairer := NewClusterRepairer(node, nil, store, ClusterRepairerConfig{}).(*clusterRepairer)
	require.NoError(t, repairer.repairOnce(context.Background()))
	require.Equal(t, int64(1), store.channels["repair.channel"])
	require.Equal(t, float64(1), testutil.ToFloat64(node.metrics.ClusterProjectionRepairs))
	require.Equal(t, float64(0), testutil.ToFloat64(node.metrics.ClusterProjectionRepairFailures))
}

func TestClusterProjectionRepairer_RecordsFailureMetrics(t *testing.T) {
	node := NewNode(nil)
	registry := prometheus.NewRegistry()
	node.SetMetrics(NewMetrics(registry))
	store := &repairTestQueryStore{err: errors.New("repair failed")}
	repairer := NewClusterRepairer(node, nil, store, ClusterRepairerConfig{}).(*clusterRepairer)

	err := repairer.repairOnce(context.Background())
	require.EqualError(t, err, "repair failed")
	require.Equal(t, float64(0), testutil.ToFloat64(node.metrics.ClusterProjectionRepairs))
	require.Equal(t, float64(1), testutil.ToFloat64(node.metrics.ClusterProjectionRepairFailures))
}

// Task 13d: owner projections whose node lease has expired are reaped
// immediately instead of lingering until the projection TTL.
func TestClusterProjectionRepairer_ReapsDeadOwnerProjections(t *testing.T) {
	directory := &fakeSessionDirectory{nodeLeases: map[string]*ClusterNodeLease{
		"node-live:inc-live": {NodeID: "node-live", IncarnationID: "inc-live"},
		"node-self:inc-self": {NodeID: "node-self", IncarnationID: "inc-self"},
	}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-self", IncarnationID: "inc-self", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)
	store := &repairTestQueryStore{projections: []ClusterNodeProjection{
		{NodeID: "node-live", IncarnationID: "inc-live"},
		{NodeID: "node-dead", IncarnationID: "inc-dead"},
		{NodeID: "node-self", IncarnationID: "inc-self"},
	}}
	repairer := NewClusterRepairer(node, directory, store, ClusterRepairerConfig{}).(*clusterRepairer)

	require.NoError(t, repairer.repairOnce(context.Background()))

	require.Contains(t, store.deleted, ClusterNodeProjection{NodeID: "node-dead", IncarnationID: "inc-dead"},
		"owner projection without a node lease must be reaped")
	require.NotContains(t, store.deleted, ClusterNodeProjection{NodeID: "node-live", IncarnationID: "inc-live"},
		"owner projection with a live node lease must be kept")
	require.NotContains(t, store.deleted, ClusterNodeProjection{NodeID: "node-self", IncarnationID: "inc-self"},
		"the node's own projection must never be reaped")
}
