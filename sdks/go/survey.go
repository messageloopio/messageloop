package messageloopgo

import (
	"fmt"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
)

// SurveyAnswer is one answered result of a client-initiated Survey.
type SurveyAnswer struct {
	// SessionID identifies the answering session.
	SessionID string
	// UserID is the answering user, read from metadata.entries["user_id"].
	// Empty when the server did not attach the entry (proto has no user_id
	// field on SurveyAnswer).
	UserID string
	// Payload is the answer payload; nil when the answer carries an error.
	Payload *Message
	// Error is the per-answer error, e.g. SURVEY_ANSWER_TOO_LARGE or
	// SURVEY_FAILED; nil for a healthy answer.
	Error error
}

// surveyAnswerFromPB converts a protocol SurveyAnswer to the SDK type.
func surveyAnswerFromPB(answer *clientpb.SurveyAnswer) SurveyAnswer {
	out := SurveyAnswer{
		SessionID: answer.GetSessionId(),
	}
	if answer.GetMetadata() != nil {
		out.UserID = answer.GetMetadata().GetEntries()["user_id"]
	}
	if answer.GetPayload() != nil {
		out.Payload = PayloadToMessage(answer.GetPayload(), "")
	}
	if err := answer.GetError(); err != nil {
		out.Error = fmt.Errorf("%s: %s", err.GetCode(), err.GetMessage())
	}
	return out
}
