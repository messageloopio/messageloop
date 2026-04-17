package messageloop

import (
	"encoding/json"
	"time"
)

// PresenceEvent represents a join or leave event published to a channel.
type PresenceEvent struct {
	Type      string `json:"__type"`
	Action    string `json:"action"`
	Channel   string `json:"channel"`
	ClientID  string `json:"client_id"`
	UserID    string `json:"user_id"`
	Timestamp int64  `json:"timestamp"`
}

func newPresenceEvent(action, channel, clientID, userID string) *PresenceEvent {
	return &PresenceEvent{
		Type:      "presence",
		Action:    action,
		Channel:   channel,
		ClientID:  clientID,
		UserID:    userID,
		Timestamp: time.Now().UnixMilli(),
	}
}

func marshalPresenceEvent(e *PresenceEvent) ([]byte, error) {
	return json.Marshal(e)
}
