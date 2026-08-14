package quicstream

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHeartbeatReadTimeout(t *testing.T) {
	require.Equal(t, 60*time.Second, heartbeatReadTimeout(0, 0, 0))
	require.Equal(t, 45*time.Second, heartbeatReadTimeout(0, 0, 45*time.Second))

	// Floor is max(2*idle, 3*ping, 10s) = 2*30s = 60s; configured 20s cannot lower it.
	require.Equal(t, 60*time.Second, heartbeatReadTimeout(30*time.Second, 0, 20*time.Second))
	// Configured value may raise the floor.
	require.Equal(t, 2*time.Minute, heartbeatReadTimeout(30*time.Second, 0, 2*time.Minute))
	// Ping-dominated floor: 3*20s = 60s.
	require.Equal(t, 60*time.Second, heartbeatReadTimeout(0, 20*time.Second, 0))
}
