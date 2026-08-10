package redisbroker

import (
	"encoding/json"

	"github.com/messageloopio/messageloop"
)

const messageTypePublication = "pub"

// redisMessage is the envelope format for publication messages stored in Redis.
// Kind mirrors messageloop.PayloadKind; older entries without a kind field
// are inferred from IsText during deserialization (rolling-upgrade safe).
type redisMessage struct {
	Type        string            `json:"t"`
	Channel     string            `json:"ch"`
	Payload     []byte            `json:"p"`
	IsText      bool              `json:"isText,omitempty"`
	Kind        messageloop.PayloadKind `json:"kind,omitempty"`
	ContentType string            `json:"ct,omitempty"`
	Id          string            `json:"id,omitempty"`
	Metadata    map[string]string `json:"md,omitempty"`
	Time        int64             `json:"ts,omitempty"`
	Offset      uint64            `json:"off,omitempty"`
	Epoch       string            `json:"epoch,omitempty"`
}

func serializeMessage(msg *redisMessage) ([]byte, error) {
	return json.Marshal(msg)
}

func deserializeMessage(data []byte) (*redisMessage, error) {
	var msg redisMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	if msg.Type == messageTypePublication && msg.Kind == 0 {
		// Backward compatibility: entries written before the kind field
		// existed encode text vs binary via isText only. Binary is the zero
		// value, so re-assigning it is a no-op; only the text fallback is
		// observable.
		if msg.IsText {
			msg.Kind = messageloop.PayloadKindText
		} else {
			msg.Kind = messageloop.PayloadKindBinary
		}
	}
	return &msg, nil
}
