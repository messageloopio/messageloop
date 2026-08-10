package messageloop

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBrokerNoReady implements Broker without a Ready method to exercise the
// fallback path of HealthHandler (brokers without a readiness signal are
// always reported healthy).
type fakeBrokerNoReady struct{}

func (fakeBrokerNoReady) Start(context.Context, PublicationHandler) error { return nil }
func (fakeBrokerNoReady) Subscribe(string) error                          { return nil }
func (fakeBrokerNoReady) Unsubscribe(string) error                        { return nil }
func (fakeBrokerNoReady) Publish(string, *Publication) (uint64, error) { return 0, nil }
func (fakeBrokerNoReady) PublishTransient(string, *Publication) error  { return nil }
func (fakeBrokerNoReady) History(string, uint64, int) ([]*Publication, error) {
	return nil, nil
}

func doHealthRequest(t *testing.T, node *Node) (*httptest.ResponseRecorder, HealthStatus) {
	t.Helper()
	rr := httptest.NewRecorder()
	node.HealthHandler()(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	var hs HealthStatus
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &hs))
	return rr, hs
}

// --- P2-5: health check must reflect broker readiness ---

func TestHealthHandler_BrokerNotReady_Returns503(t *testing.T) {
	node := NewNode(nil)
	// The default memory broker exists but Start has not been called, so its
	// Ready channel is not closed.
	rr, hs := doHealthRequest(t, node)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Equal(t, "not ready", hs.Status)
	assert.Equal(t, "not ready", hs.Broker)
}

func TestHealthHandler_BrokerReady_Returns200(t *testing.T) {
	node := NewNode(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = node.Broker().Start(ctx, func(_ string, _ *Publication) error { return nil }) }()
	waitBrokerReady(t, node)

	rr, hs := doHealthRequest(t, node)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "ok", hs.Status)
	assert.Equal(t, "ready", hs.Broker)
}

func TestHealthHandler_BrokerWithoutReady_Returns200(t *testing.T) {
	node := NewNode(nil)
	node.SetBroker(fakeBrokerNoReady{})

	rr, hs := doHealthRequest(t, node)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "ok", hs.Status)
	assert.Equal(t, "not applicable", hs.Broker)
}

func waitBrokerReady(t *testing.T, node *Node) {
	t.Helper()
	ready, ok := node.Broker().(interface{ Ready() <-chan struct{} })
	require.True(t, ok, "broker should implement Ready")
	select {
	case <-ready.Ready():
	case <-time.After(time.Second):
		t.Fatal("broker did not become ready")
	}
}

// --- P2-5: cluster-mode health check must surface Redis ping failures ---

func TestHealthHandler_ClusterEnabled_HealthCheckFailure_Returns503(t *testing.T) {
	node := NewNode(nil)
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{})
	require.NoError(t, err)
	node.SetCluster(runtime)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = node.Broker().Start(ctx, func(_ string, _ *Publication) error { return nil }) }()
	waitBrokerReady(t, node)

	node.SetHealthCheck(func(context.Context) error { return errors.New("redis unreachable") })

	rr, hs := doHealthRequest(t, node)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Equal(t, "not ready", hs.Status)
	assert.Equal(t, "unreachable", hs.Redis)
}

func TestHealthHandler_ClusterEnabled_HealthCheckOK_Returns200(t *testing.T) {
	node := NewNode(nil)
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{})
	require.NoError(t, err)
	node.SetCluster(runtime)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = node.Broker().Start(ctx, func(_ string, _ *Publication) error { return nil }) }()
	waitBrokerReady(t, node)

	node.SetHealthCheck(func(context.Context) error { return nil })

	rr, hs := doHealthRequest(t, node)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "ok", hs.Status)
	assert.Equal(t, "ok", hs.Redis)
}

func TestHealthHandler_ClusterEnabled_NoHealthCheck_Returns200(t *testing.T) {
	node := NewNode(nil)
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{})
	require.NoError(t, err)
	node.SetCluster(runtime)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = node.Broker().Start(ctx, func(_ string, _ *Publication) error { return nil }) }()
	waitBrokerReady(t, node)

	rr, hs := doHealthRequest(t, node)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "ok", hs.Status)
	assert.Equal(t, "not applicable", hs.Redis)
}
