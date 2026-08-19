package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// healthCheckTimeout bounds each probe (e.g. a Redis ping) invoked by the
// health endpoint in cluster mode, so a blackholed backend cannot hang the
// endpoint indefinitely.
const healthCheckTimeout = 2 * time.Second

// HealthStatus represents the server health check response.
type HealthStatus struct {
	// Status is "ok" when the server is healthy, otherwise a description
	// of the degraded state (e.g. "not ready").
	Status string `json:"status"`
	// Broker describes the broker readiness: "ready", "not ready", or
	// "not applicable" when the broker does not report readiness.
	Broker string `json:"broker"`
	// Redis describes the cluster-mode connectivity probe result: "ok",
	// "unreachable", or "not applicable" when no cluster health check is
	// configured.
	Redis string `json:"redis"`
}

// healthReadyBroker is implemented by brokers that expose a readiness signal
// (e.g. *memoryBroker). The returned channel is closed once the broker is
// ready to serve. Brokers without this method (e.g. the Redis broker) are
// assumed healthy.
type healthReadyBroker interface {
	Ready() <-chan struct{}
}

// HealthHandler returns an HTTP handler that reports server health.
// When the broker implements Ready, the handler returns 503 until the broker
// is ready; brokers without a Ready method are always reported healthy.
// In cluster mode, an injected HealthCheck (e.g. a Redis ping) is probed
// with a short timeout; a failed probe also yields 503.
func (n *Node) HealthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := HealthStatus{Status: "ok", Broker: "not applicable", Redis: "not applicable"}
		code := http.StatusOK

		if ready, ok := n.broker.(healthReadyBroker); ok {
			select {
			case <-ready.Ready():
				status.Broker = "ready"
			default:
				status.Status = "not ready"
				status.Broker = "not ready"
				code = http.StatusServiceUnavailable
			}
		}

		if n.ClusterEnabled() && n.healthCheck != nil {
			checkCtx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
			defer cancel()
			if err := n.healthCheck(checkCtx); err != nil {
				status.Status = "not ready"
				status.Redis = "unreachable"
				code = http.StatusServiceUnavailable
			} else {
				status.Redis = "ok"
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(status)
	}
}
