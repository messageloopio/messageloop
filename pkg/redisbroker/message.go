package redisbroker

import (
	"encoding/json"

	"github.com/fleetlit/messageloop"
)

const (
	messageTypePublication = "pub"
	messageTypeJoin        = "join"
	messageTypeLeave       = "leave"
)

// redisMessage is the envelope format for messages stored in Redis.
type redisMessage struct {
	Type    string                  `json:"t"`
	Channel string                  `json:"ch"`
	Payload []byte                  `json:"p"`
	Info    *messageloop.ClientDesc `json:"i,omitempty"`
}

func serializeMessage(msg *redisMessage) ([]byte, error) {
	return json.Marshal(msg)
}

func deserializeMessage(data []byte) (*redisMessage, error) {
	var msg redisMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func newPublicationMessage(ch string, payload []byte) *redisMessage {
	return &redisMessage{
		Type:    messageTypePublication,
		Channel: ch,
		Payload: payload,
	}
}

func newJoinMessage(ch string, info *messageloop.ClientDesc) *redisMessage {
	return &redisMessage{
		Type:    messageTypeJoin,
		Channel: ch,
		Info:    info,
	}
}

func newLeaveMessage(ch string, info *messageloop.ClientDesc) *redisMessage {
	return &redisMessage{
		Type:    messageTypeLeave,
		Channel: ch,
		Info:    info,
	}
}
