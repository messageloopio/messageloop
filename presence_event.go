package messageloop

import (
	"encoding/json"
	"time"
)

// Presence action constants shared by the occupancy live path and the legacy
// companion channel.
const (
	PresenceActionJoin  = "join"
	PresenceActionLeave = "leave"
)

// PresenceEvent is the legacy JSON envelope published on the companion
// `ch/__presence` channel (legacy_presence_channel=true). Occupancy on the
// exact business channel uses clientpb.PresenceEvent and never carries an
// "ml_type" marker (B2): the live bus distinguishes occupancy by message
// type, not by publication metadata.
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
