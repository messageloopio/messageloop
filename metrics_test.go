package messageloop

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

// TestMetricsTransportLabel pins the protocol-to-label mapping.
func TestMetricsTransportLabel(t *testing.T) {
	require.Equal(t, "ws", MetricsTransportLabel(""))
	require.Equal(t, "ws", MetricsTransportLabel("ws"))
	require.Equal(t, "grpc", MetricsTransportLabel("grpc"))
	require.Equal(t, "quic", MetricsTransportLabel("quic"))
	require.Equal(t, "ws", MetricsTransportLabel("unknown"))
}

// TestMetrics_PresenceFailuresRegistered verifies the presence_failures_total
// counter vector is registered with the op label and counts each failure
// operation (deliver/store/gen/late/companion/emit). The old rewrite op is
// gone with the ml.type broadcast rewrite (B2).
func TestMetrics_PresenceFailuresRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	metrics.PresenceFailures.WithLabelValues("store").Inc()
	metrics.PresenceFailures.WithLabelValues("deliver").Inc()
	metrics.PresenceFailures.WithLabelValues("gen").Inc()
	metrics.PresenceFailures.WithLabelValues("late").Inc()
	metrics.PresenceFailures.WithLabelValues("companion").Inc()
	metrics.PresenceFailures.WithLabelValues("emit").Inc()

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.PresenceFailures.WithLabelValues("store")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.PresenceFailures.WithLabelValues("deliver")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.PresenceFailures.WithLabelValues("gen")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.PresenceFailures.WithLabelValues("late")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.PresenceFailures.WithLabelValues("companion")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.PresenceFailures.WithLabelValues("emit")))

	families, err := reg.Gather()
	require.NoError(t, err)
	names := make(map[string]bool, len(families))
	for _, family := range families {
		names[family.GetName()] = true
	}
	require.True(t, names["messageloop_presence_failures_total"],
		"messageloop_presence_failures_total must be registered")
}

// TestMetrics_ChannelPolicyTransientForcedRegistered verifies PR-02: the
// channel-policy transient-forced counter is registered under its full name
// and increments through the metrics object.
func TestMetrics_ChannelPolicyTransientForcedRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	metrics.ChannelPolicyTransientForced.Inc()
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ChannelPolicyTransientForced))

	families, err := reg.Gather()
	require.NoError(t, err)
	found := false
	for _, family := range families {
		if family.GetName() == "messageloop_channel_policy_transient_forced_total" {
			found = true
			require.Len(t, family.GetMetric(), 1)
			require.Equal(t, float64(1), family.GetMetric()[0].GetCounter().GetValue())
		}
	}
	require.True(t, found, "messageloop_channel_policy_transient_forced_total must be registered")
}

// TestMetrics_AdminUserFanoutRegistered verifies PR-06: the admin_user_fanout
// histogram vec is registered with the op label and records fan-out sizes for
// each user-targeted operation.
func TestMetrics_AdminUserFanoutRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	metrics.AdminUserFanout.WithLabelValues("publish").Observe(3)
	metrics.AdminUserFanout.WithLabelValues("disconnect").Observe(1)
	metrics.AdminUserFanout.WithLabelValues("subscribe").Observe(0)
	metrics.AdminUserFanout.WithLabelValues("unsubscribe").Observe(2)

	families, err := reg.Gather()
	require.NoError(t, err)
	observed := make(map[string]uint64)
	found := false
	for _, family := range families {
		if family.GetName() != "messageloop_admin_user_fanout" {
			continue
		}
		found = true
		for _, metric := range family.GetMetric() {
			var op string
			for _, label := range metric.GetLabel() {
				if label.GetName() == "op" {
					op = label.GetValue()
				}
			}
			observed[op] = metric.GetHistogram().GetSampleCount()
		}
	}
	require.True(t, found, "messageloop_admin_user_fanout must be registered")
	require.Equal(t, uint64(1), observed["publish"])
	require.Equal(t, uint64(1), observed["disconnect"])
	require.Equal(t, uint64(1), observed["subscribe"])
	require.Equal(t, uint64(1), observed["unsubscribe"])
}

// TestMetrics_SurveyClientTotalRegistered verifies PR-07: the
// survey_client_total counter vector is registered with the result label.
func TestMetrics_SurveyClientTotalRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	metrics.SurveyClientTotal.WithLabelValues("ok").Inc()
	metrics.SurveyClientTotal.WithLabelValues("SURVEY_DISABLED").Inc()
	metrics.SurveyClientTotal.WithLabelValues("SURVEY_TOO_MANY_SUBSCRIBERS").Inc()

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.SurveyClientTotal.WithLabelValues("ok")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.SurveyClientTotal.WithLabelValues("SURVEY_DISABLED")))

	families, err := reg.Gather()
	require.NoError(t, err)
	names := make(map[string]bool, len(families))
	for _, family := range families {
		names[family.GetName()] = true
	}
	require.True(t, names["messageloop_survey_client_total"],
		"messageloop_survey_client_total must be registered")
}

// TestMetrics_HeartbeatIdleDisconnectsRegistered verifies PR-05: the
// heartbeat_idle_disconnects_total counter is registered and incremented
// through the metrics object.
func TestMetrics_HeartbeatIdleDisconnectsRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	metrics.HeartbeatIdleDisconnects.Inc()
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.HeartbeatIdleDisconnects))

	families, err := reg.Gather()
	require.NoError(t, err)
	names := make(map[string]bool, len(families))
	for _, family := range families {
		names[family.GetName()] = true
	}
	require.True(t, names["messageloop_heartbeat_idle_disconnects_total"],
		"messageloop_heartbeat_idle_disconnects_total must be registered")
}

// TestMetrics_RecoveryRegistered verifies PR-03: the recovery metrics
// are registered under their full names and record a truncated recovery.
func TestMetrics_RecoveryRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	metrics.RecoveryTotal.WithLabelValues("connect", "truncated").Inc()
	metrics.RecoveryPublications.WithLabelValues("connect").Observe(1000)
	metrics.RecoveryTruncatedTotal.WithLabelValues("connect").Inc()
	metrics.RecoveryTotal.WithLabelValues("subscribe", "skipped").Inc()
	metrics.RecoveryGapTotal.WithLabelValues("head_trimmed").Inc()

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.RecoveryTotal.WithLabelValues("connect", "truncated")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.RecoveryTruncatedTotal.WithLabelValues("connect")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.RecoveryTotal.WithLabelValues("subscribe", "skipped")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.RecoveryGapTotal.WithLabelValues("head_trimmed")))

	families, err := reg.Gather()
	require.NoError(t, err)
	names := make(map[string]bool, len(families))
	for _, family := range families {
		names[family.GetName()] = true
	}
	require.True(t, names["messageloop_recovery_total"], "messageloop_recovery_total must be registered")
	require.True(t, names["messageloop_recovery_publications"], "messageloop_recovery_publications must be registered")
	require.True(t, names["messageloop_recovery_truncated_total"], "messageloop_recovery_truncated_total must be registered")
	require.True(t, names["messageloop_recovery_gap_total"], "messageloop_recovery_gap_total must be registered")

	foundCapBucket := false
	for _, family := range families {
		if family.GetName() != "messageloop_recovery_publications" {
			continue
		}
		require.NotEmpty(t, family.GetMetric())
		for _, b := range family.GetMetric()[0].GetHistogram().GetBucket() {
			if b.GetUpperBound() == 1000 {
				foundCapBucket = true
				require.Equal(t, uint64(1), b.GetCumulativeCount(),
					"Observe(1000) must land in the 1000 bucket, not +Inf")
			}
		}
	}
	require.True(t, foundCapBucket, "messageloop_recovery_publications must include a 1000 bucket")
}
