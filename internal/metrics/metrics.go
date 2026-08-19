// Package metrics holds the Prometheus instrumentation of the MessageLoop
// server (KD-K26 phase three (a), PR-KA-D13), sunk from the root
// messageloop package. Callers import this package directly (D15).
package metrics

import "github.com/prometheus/client_golang/prometheus"

// MetricsTransportLabel maps a client protocol to the connections metric's
// transport label value ("ws"/"grpc"/"quic"). The protocol is set by the
// transport packages at construction; anything unknown (e.g. tests) defaults
// to "ws".
func MetricsTransportLabel(protocol string) string {
	switch protocol {
	case "grpc":
		return "grpc"
	case "quic":
		return "quic"
	default:
		return "ws"
	}
}

// Metrics holds Prometheus metrics for the MessageLoop server.
type Metrics struct {
	// ConnectionsTotal is labeled by transport ("ws", "grpc", or "quic").
	ConnectionsTotal                *prometheus.GaugeVec
	SubscriptionsTotal              prometheus.Gauge
	MessagesPublished               prometheus.Counter
	MessagesDelivered               prometheus.Counter
	PublishDuration                 prometheus.Histogram
	RPCDuration                     prometheus.Histogram
	DeliveryFailures                prometheus.Counter
	ActiveChannels                  prometheus.Gauge
	ClusterCommandDedupeHits        prometheus.Counter
	ClusterCommandTimeouts          prometheus.Counter
	ClusterCommandUnknownFinalState prometheus.Counter
	ClusterCommandHMACRejects       *prometheus.CounterVec
	ClusterProjectionRepairs        prometheus.Counter
	ClusterProjectionRepairFailures prometheus.Counter
	PresencePublishFailures         prometheus.Counter
	PresenceFailures                *prometheus.CounterVec
	ChannelPolicyTransientForced    prometheus.Counter
	RecoveryTotal                   *prometheus.CounterVec
	RecoveryPublications            *prometheus.HistogramVec
	RecoveryTruncatedTotal          *prometheus.CounterVec
	RecoveryGapTotal                *prometheus.CounterVec
	LiveGapNoticeTotal              *prometheus.CounterVec
	HeartbeatIdleDisconnects        prometheus.Counter
	AdminUserFanout                 *prometheus.HistogramVec
	SurveyClientTotal               *prometheus.CounterVec
	BindFencedTotal                 prometheus.Counter
	BindRefreshFailTotal            prometheus.Counter
	EvictLag                        prometheus.Histogram
	SessionDualActivationSeconds    prometheus.Histogram
	OccupancyGenDiscards            prometheus.Counter
	LiveDropTotal                   prometheus.Counter
	LiveDegradedChannels            prometheus.Gauge
}

// NewMetrics creates and registers all Prometheus metrics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		ConnectionsTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "messageloop",
			Name:      "connections_total",
			Help:      "Current number of active client connections.",
		}, []string{"transport"}),
		SubscriptionsTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "messageloop",
			Name:      "subscriptions_total",
			Help:      "Current number of active channel subscriptions.",
		}),
		MessagesPublished: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "messages_published_total",
			Help:      "Total number of messages published.",
		}),
		MessagesDelivered: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "messages_delivered_total",
			Help:      "Total number of messages delivered to subscribers.",
		}),
		PublishDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "messageloop",
			Name:      "message_publish_duration_seconds",
			Help:      "Time taken to publish a message.",
			Buckets:   prometheus.DefBuckets,
		}),
		RPCDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "messageloop",
			Name:      "rpc_duration_seconds",
			Help:      "Time taken to handle an RPC request (proxy round-trip).",
			Buckets:   prometheus.DefBuckets,
		}),
		DeliveryFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "delivery_failures_total",
			Help:      "Total number of message delivery failures (dead letters).",
		}),
		ActiveChannels: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "messageloop",
			Name:      "active_channels",
			Help:      "Current number of channels with at least one subscriber.",
		}),
		ClusterCommandDedupeHits: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "cluster_command_dedupe_hits_total",
			Help:      "Total number of cluster command dedupe hits.",
		}),
		ClusterCommandTimeouts: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "cluster_command_timeouts_total",
			Help:      "Total number of cluster command reply timeouts.",
		}),
		ClusterCommandUnknownFinalState: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "cluster_command_unknown_final_state_total",
			Help:      "Total number of cluster commands that ended in unknown_final_state.",
		}),
		ClusterCommandHMACRejects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "cluster_command_hmac_reject_total",
			Help:      "Total number of cluster command envelopes rejected by HMAC verification, by reason (missing/bad/skew/id).",
		}, []string{"reason"}),
		ClusterProjectionRepairs: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "cluster_projection_repairs_total",
			Help:      "Total number of successful cluster projection repair passes.",
		}),
		ClusterProjectionRepairFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "cluster_projection_repair_failures_total",
			Help:      "Total number of failed cluster projection repair passes.",
		}),
		PresencePublishFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "presence_publish_failures_total",
			Help:      "Total number of failed presence join/leave event publications.",
		}),
		PresenceFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "presence_failures_total",
			Help:      "Total number of presence failures by operation (deliver/store/rewrite/companion/emit).",
		}, []string{"op"}),
		ChannelPolicyTransientForced: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "channel_policy_transient_forced_total",
			Help:      "Total number of client publications forced to transient delivery by channel policy.",
		}),
		RecoveryTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "recovery_total",
			Help:      "Total number of channel recovery attempts by path and result (ok/truncated/failed/skipped).",
		}, []string{"path", "result"}),
		RecoveryPublications: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "messageloop",
			Name:      "recovery_publications",
			Help:      "Number of publications delivered per channel recovery attempt.",
			// Count scale up to MaxRecoveredPublications. DefBuckets is a
			// duration scale and would dump almost every observation into +Inf.
			Buckets: []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000},
		}, []string{"path"}),
		RecoveryTruncatedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "recovery_truncated_total",
			Help:      "Total number of channel recovery attempts truncated by a cap.",
		}, []string{"path"}),
		RecoveryGapTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "recovery_gap_total",
			Help:      "Total number of channel recovery attempts that observed a history gap, by reason (head_trimmed/empty_expired).",
		}, []string{"reason"}),
		LiveGapNoticeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "live_gap_notice_total",
			Help:      "Total number of catch-up gap notices fanned out to local subscribers, by reason (middle/replay_truncated).",
		}, []string{"reason"}),
		HeartbeatIdleDisconnects: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "heartbeat_idle_disconnects_total",
			Help:      "Total number of connections disconnected with 3511 by the heartbeat (idle timeout or unresponded server ping).",
		}),
		AdminUserFanout: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "messageloop",
			Name:      "admin_user_fanout",
			Help:      "Number of sessions fanned out per user-targeted admin operation (publish/disconnect/subscribe/unsubscribe).",
			// Fan-out scale is session counts, so use a count-shaped bucket
			// ladder instead of DefBuckets (a duration scale).
			Buckets: []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000},
		}, []string{"op"}),
		SurveyClientTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "survey_client_total",
			Help:      "Total number of client-initiated surveys by result (ok or the top-level error code).",
		}, []string{"result"}),
		BindFencedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "bind_fenced_total",
			Help:      "Total number of session bind/takeover attempts fenced out by a lost lease claim (CAS-nil first registration or takeover CAS conflict).",
		}),
		BindRefreshFailTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "bind_refresh_fail_total",
			Help:      "Total number of same-fence lease refreshes rejected as fenced (foreign fencing, newer directory version, or lost refresh CAS).",
		}),
		EvictLag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "messageloop",
			Name:      "evict_lag",
			Help:      "Round-trip latency in seconds of a session takeover (evict) command sent to the remote old owner.",
			Buckets:   prometheus.DefBuckets,
		}),
		SessionDualActivationSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "messageloop",
			Name:      "session_dual_activation_seconds",
			Help:      "Duration in seconds of the takeover overlap window (lease CAS win to takeover completion, including the dead-node bypass).",
			Buckets:   prometheus.DefBuckets,
		}),
		OccupancyGenDiscards: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "occupancy_gen_discard_total",
			Help:      "Total number of occupancy events discarded because an equal or newer generation was already applied for the session.",
		}),
		LiveDropTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "live_drop_total",
			Help:      "Total number of live publications lost to dense-seq discontinuities (e.g. a full pub/sub buffer dropping messages silently).",
		}),
		LiveDegradedChannels: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "messageloop",
			Name:      "live_degraded_channels",
			Help:      "Current number of channels marked degraded by live delivery pressure: an occupancy event dropped on a full delivery queue, or a publication dense-seq jump. Cleared on the channel's next successful enqueue and reset on reconnect.",
		}),
	}
	reg.MustRegister(
		m.ConnectionsTotal,
		m.SubscriptionsTotal,
		m.MessagesPublished,
		m.MessagesDelivered,
		m.PublishDuration,
		m.RPCDuration,
		m.DeliveryFailures,
		m.ActiveChannels,
		m.ClusterCommandDedupeHits,
		m.ClusterCommandTimeouts,
		m.ClusterCommandUnknownFinalState,
		m.ClusterCommandHMACRejects,
		m.ClusterProjectionRepairs,
		m.ClusterProjectionRepairFailures,
		m.PresencePublishFailures,
		m.PresenceFailures,
		m.ChannelPolicyTransientForced,
		m.RecoveryTotal,
		m.RecoveryPublications,
		m.RecoveryTruncatedTotal,
		m.RecoveryGapTotal,
		m.LiveGapNoticeTotal,
		m.HeartbeatIdleDisconnects,
		m.AdminUserFanout,
		m.SurveyClientTotal,
		m.BindFencedTotal,
		m.BindRefreshFailTotal,
		m.EvictLag,
		m.SessionDualActivationSeconds,
		m.OccupancyGenDiscards,
		m.LiveDropTotal,
		m.LiveDegradedChannels,
	)
	return m
}
