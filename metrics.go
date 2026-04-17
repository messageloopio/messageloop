package messageloop

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds Prometheus metrics for the MessageLoop server.
type Metrics struct {
	ConnectionsTotal                prometheus.Gauge
	SubscriptionsTotal              prometheus.Gauge
	MessagesPublished               prometheus.Counter
	MessagesDelivered               prometheus.Counter
	PublishDuration                 prometheus.Histogram
	DeliveryFailures                prometheus.Counter
	ClusterCommandDedupeHits        prometheus.Counter
	ClusterCommandTimeouts          prometheus.Counter
	ClusterCommandUnknownFinalState prometheus.Counter
	ClusterProjectionRepairs        prometheus.Counter
	ClusterProjectionRepairFailures prometheus.Counter
}

// NewMetrics creates and registers all Prometheus metrics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		ConnectionsTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "messageloop",
			Name:      "connections_total",
			Help:      "Current number of active client connections.",
		}),
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
		DeliveryFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "messageloop",
			Name:      "delivery_failures_total",
			Help:      "Total number of message delivery failures (dead letters).",
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
	}
	reg.MustRegister(
		m.ConnectionsTotal,
		m.SubscriptionsTotal,
		m.MessagesPublished,
		m.MessagesDelivered,
		m.PublishDuration,
		m.DeliveryFailures,
		m.ClusterCommandDedupeHits,
		m.ClusterCommandTimeouts,
		m.ClusterCommandUnknownFinalState,
		m.ClusterProjectionRepairs,
		m.ClusterProjectionRepairFailures,
	)
	return m
}
