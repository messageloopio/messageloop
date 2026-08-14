package messageloop

import "time"

// Centralized default constants used across the codebase.
const (
	// DefaultMaxMessageSize is the maximum inbound message size in bytes (64 KB).
	DefaultMaxMessageSize = 64 * 1024

	// DefaultHeartbeatIdleTimeout is the idle timeout before a client is disconnected.
	DefaultHeartbeatIdleTimeout = 300 * time.Second

	// DefaultHistoryLimit is the maximum number of publications returned by History
	// when the caller does not specify a limit.
	DefaultHistoryLimit = 1000

	// MaxRecoveredPublications caps the total number of publications delivered
	// during history recovery for a single Connect or Subscribe request
	// (shared across all channels in that request). Exceeding publications are
	// truncated and surfaced in RecoverResult.truncated.
	MaxRecoveredPublications = 1000

	// DefaultShutdownTimeout is the maximum time to wait for graceful shutdown.
	DefaultShutdownTimeout = 10 * time.Second

	// MaxPresenceSnapshotClients caps the number of PresenceInfo entries in
	// a presence snapshot (Connected.presence / SubscribeAck.presence /
	// PresenceQuery). A channel policy with presence_snapshot_limit > 0 may
	// override this cap up or down.
	MaxPresenceSnapshotClients = 256
)
