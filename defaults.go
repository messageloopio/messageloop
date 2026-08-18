package messageloop

import "time"

// Centralized default constants used across the codebase.
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

	// MaxPresenceSnapshotClients caps the number of PresenceInfo entries in
	// a presence snapshot (Connected.presence / SubscribeAck.presence /
	// PresenceQuery). A channel policy with presence_snapshot_limit > 0 may
	// override this cap up or down.
	MaxPresenceSnapshotClients = 256

	// MaxSurveyAnswerBytes caps a single survey answer payload. A larger
	// answer is delivered as a SURVEY_ANSWER_TOO_LARGE error with an empty
	// payload (PR-07).
	MaxSurveyAnswerBytes = 4096

	// MaxSurveyResultBytes caps the encoded size of an outbound client
	// SurveyResult; answers beyond the cap are stripped of their payload and
	// turned into errors (PR-07).
	MaxSurveyResultBytes = 256 * 1024
)
