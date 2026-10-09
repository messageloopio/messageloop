package survey

import "time"

// MaxSurveyAnswerBytes caps a single survey answer payload. A larger
// answer is delivered as a SURVEY_ANSWER_TOO_LARGE error with an empty
// payload (PR-07).
const MaxSurveyAnswerBytes = 4096

// MaxSurveyResultBytes caps the encoded size of an outbound client
// SurveyResult; answers beyond the cap are stripped of their payload and
// turned into errors (PR-07).
const MaxSurveyResultBytes = 256 * 1024

// Survey timeout clamp contract (one implementation for the client path and
// the Server API path; previously duplicated verbatim in
// internal/session/client.go and internal/serverapi/api_handler.go).
const (
	// DefaultSurveyTimeout is the effective cap when the channel policy sets
	// no MaxSurveyTimeout.
	DefaultSurveyTimeout = 5 * time.Second
	// MaxSurveyTimeoutCeiling bounds any effective survey timeout.
	MaxSurveyTimeoutCeiling = 10 * time.Second
	// MinSurveyTimeout floors an explicitly requested timeout.
	MinSurveyTimeout = 100 * time.Millisecond
)

// ClampTimeout resolves one survey's effective timeout: the policy cap
// (policyCap, DefaultSurveyTimeout when <= 0) bounded by the hard ceiling;
// an explicit request (requestedMs > 0) is clamped into
// [MinSurveyTimeout, cap]. requestedMs <= 0 keeps the cap.
func ClampTimeout(policyCap time.Duration, requestedMs int64) time.Duration {
	timeout := policyCap
	if timeout <= 0 {
		timeout = DefaultSurveyTimeout
	}
	if timeout > MaxSurveyTimeoutCeiling {
		timeout = MaxSurveyTimeoutCeiling
	}
	if requestedMs > 0 {
		requested := time.Duration(requestedMs) * time.Millisecond
		if requested > timeout {
			requested = timeout
		}
		if requested < MinSurveyTimeout {
			requested = MinSurveyTimeout
		}
		timeout = requested
	}
	return timeout
}
