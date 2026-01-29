package messageloop

import (
	"context"
	"time"
)

// HeartbeatConfig contains parsed heartbeat configuration durations.
type HeartbeatConfig struct {
	IdleTimeout time.Duration
}

// HeartbeatManager manages client heartbeat monitoring.
type HeartbeatManager struct {
	config HeartbeatConfig
}

// NewHeartbeatManager creates a new HeartbeatManager with the given config.
func NewHeartbeatManager(cfg HeartbeatConfig) *HeartbeatManager {
	return &HeartbeatManager{
		config: cfg,
	}
}

// Start starts the heartbeat goroutine for a client session.
func (hm *HeartbeatManager) Start(ctx context.Context, client *ClientSession) {
	if hm.config.IdleTimeout == 0 {
		return
	}

	heartbeatCtx, cancel := context.WithCancel(ctx)
	client.setHeartbeatCancel(cancel)

	go hm.heartbeatLoop(heartbeatCtx, client)
}

// heartbeatLoop manages the heartbeat timers for a client.
func (hm *HeartbeatManager) heartbeatLoop(ctx context.Context, client *ClientSession) {
	idleTimer := time.NewTimer(hm.config.IdleTimeout)
	defer idleTimer.Stop()

	client.ResetActivity()

	for {
		select {
		case <-ctx.Done():
			return

		case <-idleTimer.C:
			client.mu.Lock()
			idle := time.Since(client.lastActivity) > hm.config.IdleTimeout
			if idle {
				client.mu.Unlock()
				_ = client.close(DisconnectIdleTimeout)
				return
			}
			client.mu.Unlock()
			idleTimer.Reset(hm.config.IdleTimeout)
		}
	}
}

// Config returns the heartbeat configuration.
func (hm *HeartbeatManager) Config() HeartbeatConfig {
	return hm.config
}
