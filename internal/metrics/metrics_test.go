package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

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
// operation (deliver/store/gen/companion/emit). The old rewrite op is gone
// with the ml.type broadcast rewrite (B2); the late op moved to the dedicated
// occupancy_gen_discard_total counter (D3).
func TestMetrics_PresenceFailuresRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	metrics.PresenceFailures.WithLabelValues("store").Inc()
	metrics.PresenceFailures.WithLabelValues("deliver").Inc()
	metrics.PresenceFailures.WithLabelValues("gen").Inc()
	metrics.PresenceFailures.WithLabelValues("companion").Inc()
	metrics.PresenceFailures.WithLabelValues("emit").Inc()

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.PresenceFailures.WithLabelValues("store")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.PresenceFailures.WithLabelValues("deliver")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.PresenceFailures.WithLabelValues("gen")))
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

// TestMetrics_LiveGapNoticeRegistered verifies the live_gap_notice_total
// counter vector (C6) is registered with the reason label and increments
// through the metrics object.
func TestMetrics_LiveGapNoticeRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	metrics.LiveGapNoticeTotal.WithLabelValues("middle").Inc()
	metrics.LiveGapNoticeTotal.WithLabelValues("replay_truncated").Inc()

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.LiveGapNoticeTotal.WithLabelValues("middle")))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.LiveGapNoticeTotal.WithLabelValues("replay_truncated")))

	families, err := reg.Gather()
	require.NoError(t, err)
	names := make(map[string]bool, len(families))
	for _, family := range families {
		names[family.GetName()] = true
	}
	require.True(t, names["messageloop_live_gap_notice_total"],
		"messageloop_live_gap_notice_total must be registered")
}

// TestMetrics_ContractObservabilityRegistered verifies PR-KA-D3/D4: the
// contract observability metrics from the kernel architecture observability
// section are registered under their exact names and accept Inc/Observe/Set
// through the metrics object.
func TestMetrics_ContractObservabilityRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	metrics.BindFencedTotal.Inc()
	metrics.BindRefreshFailTotal.Inc()
	metrics.EvictLag.Observe(0.5)
	metrics.SessionDualActivationSeconds.Observe(1.5)
	metrics.OccupancyGenDiscards.Inc()
	metrics.LiveDropTotal.Add(3)
	metrics.LiveDegradedChannels.Set(2)

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.BindFencedTotal))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.BindRefreshFailTotal))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.OccupancyGenDiscards))
	require.Equal(t, float64(3), testutil.ToFloat64(metrics.LiveDropTotal))
	require.Equal(t, float64(2), testutil.ToFloat64(metrics.LiveDegradedChannels))

	families, err := reg.Gather()
	require.NoError(t, err)
	names := make(map[string]bool, len(families))
	for _, family := range families {
		names[family.GetName()] = true
	}
	for _, name := range []string{
		"messageloop_bind_fenced_total",
		"messageloop_bind_refresh_fail_total",
		"messageloop_evict_lag",
		"messageloop_session_dual_activation_seconds",
		"messageloop_occupancy_gen_discard_total",
		"messageloop_live_drop_total",
		"messageloop_live_degraded_channels",
	} {
		require.True(t, names[name], "%s must be registered", name)
	}

	// The two histograms recorded exactly one observation each.
	for name, histogram := range map[string]prometheus.Histogram{
		"messageloop_evict_lag":                       metrics.EvictLag,
		"messageloop_session_dual_activation_seconds": metrics.SessionDualActivationSeconds,
	} {
		var metric dto.Metric
		require.NoError(t, histogram.Write(&metric))
		require.Equal(t, uint64(1), metric.GetHistogram().GetSampleCount(), "%s sample count", name)
	}
}
