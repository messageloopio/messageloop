package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestHeartbeatConfigReadDeadline pins the single read-deadline formula the
// WS, QUIC and KCP read loops resolve through ReadDeadline (previously three
// hand copies, each with its own per-transport test).
func TestHeartbeatConfigReadDeadline(t *testing.T) {
	assert := assert.New(t)
	cfg := func(idle, ping time.Duration) HeartbeatConfig {
		return HeartbeatConfig{IdleTimeout: idle, PingInterval: ping}
	}

	// Fully disabled heartbeat: 60s, configured wins.
	assert.Equal(60*time.Second, cfg(0, 0).ReadDeadline(0))
	assert.Equal(45*time.Second, cfg(0, 0).ReadDeadline(45*time.Second))

	// Floor is max(2*idle, 3*ping, 10s); configured may raise, never lower.
	assert.Equal(60*time.Second, cfg(30*time.Second, 0).ReadDeadline(20*time.Second))
	assert.Equal(2*time.Minute, cfg(30*time.Second, 0).ReadDeadline(2*time.Minute))
	assert.Equal(60*time.Second, cfg(0, 20*time.Second).ReadDeadline(0))
	assert.Equal(30*time.Second, cfg(15*time.Second, 5*time.Second).ReadDeadline(5*time.Second))
	assert.Equal(45*time.Second, cfg(15*time.Second, 5*time.Second).ReadDeadline(45*time.Second))
	assert.Equal(10*time.Second, cfg(0, time.Second).ReadDeadline(0))
}
