package messageloop

import (
	"context"
	"math/rand"
	"time"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
)

// HeartbeatConfig contains parsed heartbeat configuration durations.
type HeartbeatConfig struct {
	IdleTimeout  time.Duration
	PingInterval time.Duration
	PingTimeout  time.Duration
}

// HeartbeatManager manages client heartbeat monitoring.
type HeartbeatManager struct {
	config HeartbeatConfig
	// jitter scales ping intervals to 0.8~1.2 of their value so that
	// thousands of connections armed at the same moment do not ping in a
	// synchronized burst. Field (not package var) so tests can pin it
	// deterministically before Start; it is only written before the loop
	// goroutine starts and read by that single goroutine.
	jitter func(time.Duration) time.Duration
}

// defaultJitter is the production jitter function: 0.8~1.2 of the interval.
func defaultJitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// NewHeartbeatManager creates a new HeartbeatManager with the given config.
func NewHeartbeatManager(cfg HeartbeatConfig) *HeartbeatManager {
	return &HeartbeatManager{
		config: cfg,
		jitter: defaultJitter,
	}
}

// Start starts the heartbeat goroutine for a client session. It starts
// nothing only when both the idle timeout and server pings are disabled:
// a ping-only configuration (idle=0, ping_interval>0) still needs the loop
// to send pings and arm ping deadlines.
func (hm *HeartbeatManager) Start(ctx context.Context, client *Client) {
	if hm.config.IdleTimeout == 0 && hm.config.PingInterval == 0 {
		return
	}

	heartbeatCtx, cancel := context.WithCancel(ctx)
	client.setHeartbeatCancel(cancel)

	go hm.heartbeatLoop(heartbeatCtx, client)
}

// heartbeatLoop manages the heartbeat timers for a client.
//
// Strategy B (KD-14): every server ping arms a one-shot pingDeadline; the
// deadline fires at ping_timeout and disconnects with 3511 without waiting
// for the next ping tick or the idle check. Any inbound frame stops the
// pending deadline (HandleMessage), so traffic is as good as a pong.
func (hm *HeartbeatManager) heartbeatLoop(ctx context.Context, client *Client) {
	client.ResetActivity()

	// idleTicker exists only when an idle timeout is configured.
	var idleTicker *time.Ticker
	var idleCh <-chan time.Time
	if hm.config.IdleTimeout > 0 {
		idleTicker = time.NewTicker(hm.config.IdleTimeout)
		defer idleTicker.Stop()
		idleCh = idleTicker.C
	}

	// pingTimer fires one (jittered) interval after the last ping. The first
	// ping lands one interval after connect — never immediately — so a
	// connect burst is not amplified into a ping burst.
	var pingTimer *time.Timer
	var pingCh <-chan time.Time
	if hm.config.PingInterval > 0 {
		pingTimer = time.NewTimer(hm.jitter(hm.config.PingInterval))
		pingCh = pingTimer.C
		defer pingTimer.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-pingCh:
			pingTimer.Reset(hm.jitter(hm.config.PingInterval))
			client.mu.Lock()
			closed := client.state == SessionClosed
			client.mu.Unlock()
			if closed {
				return
			}
			// Send the outbound ping, then arm the one-shot deadline. A
			// previous deadline that is still armed (the earlier ping was
			// never answered) is replaced by this ping's deadline.
			if err := client.Send(ctx, MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Ping{
					Ping: &clientpb.Ping{},
				}
			})); err != nil {
				continue
			}
			client.armPingDeadline(hm.config.PingTimeout)

		case <-idleCh:
			client.mu.Lock()
			idle := time.Since(client.lastActivity) > hm.config.IdleTimeout
			if idle {
				client.mu.Unlock()
				client.disconnectHeartbeatTimeout()
				return
			}
			client.mu.Unlock()
		}
	}
}

// armPingDeadline arms the one-shot response deadline for the last outbound
// ping. When no inbound frame (not only a pong) arrives within timeout the
// connection is disconnected with 3511 immediately — strategy B, it does not
// wait for the next ping tick or the idle check.
func (c *Client) armPingDeadline(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	var deadline *time.Timer
	deadline = time.AfterFunc(timeout, func() {
		// The pointer compare disarms a deadline that was replaced by a newer
		// ping or already cancelled by inbound traffic; the status check
		// keeps the callback from firing after any other close path.
		c.mu.Lock()
		fired := c.pingDeadline == deadline && c.state != SessionClosed
		if fired {
			c.pingDeadline = nil
		}
		c.mu.Unlock()
		if !fired {
			return
		}
		c.disconnectHeartbeatTimeout()
	})
	c.mu.Lock()
	if c.pingDeadline != nil {
		c.pingDeadline.Stop()
	}
	c.pingDeadline = deadline
	c.mu.Unlock()
}

// stopPingDeadline disarms the pending ping deadline. Called on every
// inbound frame: any traffic proves the connection is alive, so a pong is
// not the only way to answer a server ping.
func (c *Client) stopPingDeadline() {
	c.mu.Lock()
	if c.pingDeadline != nil {
		c.pingDeadline.Stop()
		c.pingDeadline = nil
	}
	c.mu.Unlock()
}

// disconnectHeartbeatTimeout closes the client with DisconnectIdleTimeout
// (3511) and counts the disconnect in heartbeat_idle_disconnects_total. The
// CAS guard ensures that when the ping deadline and the idle ticker race
// (ping_timeout ≈ idle), exactly one caller issues the close and counts.
func (c *Client) disconnectHeartbeatTimeout() {
	if !c.heartbeatDisconnectOnce.CompareAndSwap(false, true) {
		return
	}
	_ = c.Close(DisconnectIdleTimeout)
	if c.node.metrics != nil {
		c.node.metrics.HeartbeatIdleDisconnects.Inc()
	}
}

// Config returns the heartbeat configuration.
func (hm *HeartbeatManager) Config() HeartbeatConfig {
	return hm.config
}
