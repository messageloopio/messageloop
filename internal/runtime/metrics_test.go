package runtime

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// --- A3: connections_total carries a transport label (ws/grpc) ---

// TestMetrics_ConnectionsTotal_TransportLabels verifies that Inc/Dec balance
// per transport label: a default (ws) client and a grpc client each count
// under their own label and close back to zero.
func TestMetrics_ConnectionsTotal_TransportLabels(t *testing.T) {
	ctx := context.Background()
	metrics := NewMetrics(prometheus.NewRegistry())
	node := NewNode(nil)
	node.SetMetrics(metrics)

	wsClient, _, err := NewClient(ctx, node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	wsClient.ForceTestIDs("sess-ws", "user-ws", "client-ws")
	grpcClient, _, err := NewClient(ctx, node, noopTransport{}, JSONMarshaler{}, WithProtocol("grpc"))
	require.NoError(t, err)
	grpcClient.ForceTestIDs("sess-grpc", "user-grpc", "client-grpc")
	quicClient, _, err := NewClient(ctx, node, noopTransport{}, JSONMarshaler{}, WithProtocol("quic"))
	require.NoError(t, err)
	quicClient.ForceTestIDs("sess-quic", "user-quic", "client-quic")

	require.NoError(t, node.AddClient(wsClient))
	require.NoError(t, node.AddClient(grpcClient))
	require.NoError(t, node.AddClient(quicClient))
	// Mirror the production connect path: only clients that passed AddClient
	// are counted, and MarkMetricsCharged arms the close() decrement.
	wsClient.MarkMetricsCharged()
	grpcClient.MarkMetricsCharged()
	quicClient.MarkMetricsCharged()
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ConnectionsTotal.WithLabelValues("ws")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ConnectionsTotal.WithLabelValues("grpc")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ConnectionsTotal.WithLabelValues("quic")))

	require.NoError(t, wsClient.Close(Disconnect{}))
	require.NoError(t, grpcClient.Close(Disconnect{}))
	require.NoError(t, quicClient.Close(Disconnect{}))
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.ConnectionsTotal.WithLabelValues("ws")))
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.ConnectionsTotal.WithLabelValues("grpc")))
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.ConnectionsTotal.WithLabelValues("quic")))
}
