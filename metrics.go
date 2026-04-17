package messageloop

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds Prometheus metrics for the MessageLoop server.
type Metrics struct {
	ConnectionsTotal   prometheus.Gauge
	SubscriptionsTotal prometheus.Gauge
	MessagesPublished  prometheus.Counter
	MessagesDelivered  prometheus.Counter
	PublishDuration    prometheus.Histogram
	DeliveryFailures   prometheus.Counter
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
	}
	reg.MustRegister(
		m.ConnectionsTotal,
		m.SubscriptionsTotal,
		m.MessagesPublished,
		m.MessagesDelivered,
		m.PublishDuration,
		m.DeliveryFailures,
	)
	return m
}
