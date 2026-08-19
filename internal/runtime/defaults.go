package runtime

import "time"

// Centralized default constants used by the Node runtime.
const (
	// DefaultMaxMessageSize is the maximum inbound message size in bytes (64 KB).
	DefaultMaxMessageSize = 64 * 1024

	// DefaultHeartbeatIdleTimeout is the idle timeout before a client is disconnected.
	DefaultHeartbeatIdleTimeout = 300 * time.Second

	// MaxRecoveredPublications caps the total number of publications delivered
	// during history recovery for a single Connect or Subscribe request
	// (shared across all channels in that request). Exceeding publications are
	// truncated and surfaced in RecoverResult.truncated.
	MaxRecoveredPublications = 1000

	// DefaultShutdownTimeout is the maximum time to wait for graceful shutdown.
	DefaultShutdownTimeout = 10 * time.Second
)
