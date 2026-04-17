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
	repairer := NewClusterProjectionRepairer(node, store, ClusterProjectionRepairerConfig{}).(*clusterProjectionRepairer)
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
	repairer := NewClusterProjectionRepairer(node, store, ClusterProjectionRepairerConfig{}).(*clusterProjectionRepairer)

	err := repairer.repairOnce(context.Background())
	require.EqualError(t, err, "repair failed")
	require.Equal(t, float64(0), testutil.ToFloat64(node.metrics.ClusterProjectionRepairs))
	require.Equal(t, float64(1), testutil.ToFloat64(node.metrics.ClusterProjectionRepairFailures))
}
