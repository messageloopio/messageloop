package messageloop

import "github.com/prometheus/client_golang/prometheus"

// MetricsTransportLabel maps a client protocol to the connections metric's
// transport label value ("ws"/"grpc"). The protocol is set by the transport
// packages at construction; anything unknown (e.g. tests) defaults to "ws".
func MetricsTransportLabel(protocol string) string {
	if protocol == "grpc" {
		return "grpc"
	}
	return "ws"
}

// Metrics holds Prometheus metrics for the MessageLoop server.
type Metrics struct {
	// ConnectionsTotal is labeled by transport ("ws" or "grpc").
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
	ClusterProjectionRepairs        prometheus.Counter
	ClusterProjectionRepairFailures prometheus.Counter
	PresencePublishFailures         prometheus.Counter
	PresenceFailures                *prometheus.CounterVec
	ChannelPolicyTransientForced    prometheus.Counter
	RecoveryTotal                   *prometheus.CounterVec
	RecoveryPublications            *prometheus.HistogramVec
	RecoveryTruncatedTotal          *prometheus.CounterVec
	HeartbeatIdleDisconnects        prometheus.Counter
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
		HeartbeatIdleDisconnects: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "heartbeat_idle_disconnects_total",
			Help:      "Total number of connections disconnected with 3511 by the heartbeat (idle timeout or unresponded server ping).",
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
		m.ClusterProjectionRepairs,
		m.ClusterProjectionRepairFailures,
		m.PresencePublishFailures,
		m.PresenceFailures,
		m.ChannelPolicyTransientForced,
		m.RecoveryTotal,
		m.RecoveryPublications,
		m.RecoveryTruncatedTotal,
		m.HeartbeatIdleDisconnects,
	)
	return m
}
