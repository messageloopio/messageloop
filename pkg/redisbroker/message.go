package redisbroker

import "encoding/json"

const messageTypePublication = "pub"

// redisMessage is the envelope format for publication messages stored in Redis.
type redisMessage struct {
	Type    string `json:"t"`
	Channel string `json:"ch"`
	Payload []byte `json:"p"`
	IsText  bool   `json:"isText,omitempty"`
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
