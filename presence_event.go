package messageloop

import (
	"encoding/json"
	"time"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// Presence metadata contract shared by the transient wire frame and the
// broadcast rewrite path (Phase 1 recognizes these frames and rewrites them
// into first-class PresenceEvents instead of publications).
const (
	PresenceMetaTypeKey   = "ml.type"
	PresenceMetaTypeValue = "presence"
	PresenceActionJoin    = "join"
	PresenceActionLeave   = "leave"
)

// PresenceEvent is the legacy JSON envelope published on the companion
// `ch/__presence` channel (legacy_presence_channel=true). Phase 1 keeps it
// for the legacy companion path only; the first-class path uses
// clientpb.PresenceEvent.
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

// presencePublication wraps a first-class PresenceEvent in the transient
// wire frame used by the broadcast rewrite path (and PR-04b cross-node
// emit). Phase 1 emit never calls PublishTransient with this frame; it
// exists so the rewrite path and tests share one encoding.
func presencePublication(evt *clientpb.PresenceEvent) *Publication {
	if evt == nil {
		return nil
	}
	payload, err := protojson.Marshal(evt)
	if err != nil {
		return nil
	}
	return &Publication{
		Payload:  payload,
		Kind:     PayloadKindJSON,
		Metadata: map[string]string{PresenceMetaTypeKey: PresenceMetaTypeValue},
	}
}

// parsePresencePublication decodes a transient wire frame into a first-class
// PresenceEvent. A frame that cannot be parsed returns nil: broadcast must
// drop it, never forward it as chat.
func parsePresencePublication(pub *Publication) *clientpb.PresenceEvent {
	if pub == nil || len(pub.Payload) == 0 {
		return nil
	}
	evt := &clientpb.PresenceEvent{}
	if err := protojson.Unmarshal(pub.Payload, evt); err != nil {
		return nil
	}
	return evt
}
