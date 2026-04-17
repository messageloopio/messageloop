package messageloop

import (
	"encoding/json"
	"net/http"
)

// HealthStatus represents the server health check response.
type HealthStatus struct {
	Status string `json:"status"`
}

// HealthHandler returns an HTTP handler that reports server health.
func (n *Node) HealthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(HealthStatus{Status: "ok"})
	}
}
