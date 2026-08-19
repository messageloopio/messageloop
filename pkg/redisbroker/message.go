package redisbroker

import (
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"

	"github.com/messageloopio/messageloop/internal/occupancy"
	"github.com/messageloopio/messageloop/internal/stream"
)

const (
	messageTypePublication = "pub"
	// messageTypeOccupancy is the live-only message type for occupancy
	// events (B2): it never lands in a Stream, so catch-up never replays it.
	messageTypeOccupancy = "occupancy"
)

// redisMessage is the envelope format for publication messages stored in Redis.
// Kind mirrors stream.PayloadKind; older entries without a kind field
// are inferred from IsText during deserialization (rolling-upgrade safe).
// Seq mirrors Offset: both are backfilled after the stream append and only
// travel in the live pub/sub payload — the stream's data JSON carries
// neither (the dense seq lives in the entry's "s" field instead).
type redisMessage struct {
	Type        string             `json:"t"`
	Channel     string             `json:"ch"`
	Payload     []byte             `json:"p"`
	IsText      bool               `json:"isText,omitempty"`
	Kind        stream.PayloadKind `json:"kind,omitempty"`
	ContentType string             `json:"ct,omitempty"`
	Id          string             `json:"id,omitempty"`
	Metadata    map[string]string  `json:"md,omitempty"`
	Time        int64              `json:"ts,omitempty"`
	Offset      uint64             `json:"off,omitempty"`
	Seq         uint64             `json:"seq,omitempty"`
	Epoch       string             `json:"epoch,omitempty"`
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
			msg.Kind = stream.PayloadKindText
		} else {
			msg.Kind = stream.PayloadKindBinary
		}
	}
	return &msg, nil
}

// redisOccupancy is the live-bus envelope for occupancy events (B2). It is
// deliberately separate from redisMessage: occupancy has no stream offset,
// payload or history kind, and must never be deliverOnce'd.
type redisOccupancy struct {
	Type    string          `json:"t"`
	Channel string          `json:"ch"`
	Gen     uint64          `json:"gen"`
	Event   json.RawMessage `json:"evt"` // protojson(clientpb.PresenceEvent)
}

func serializeOccupancy(evt occupancy.OccupancyEvent) ([]byte, error) {
	if evt.Event == nil {
		return nil, errors.New("occupancy event has nil presence event")
	}
	raw, err := protojson.Marshal(evt.Event)
	if err != nil {
		return nil, fmt.Errorf("marshal occupancy presence event: %w", err)
	}
	return json.Marshal(&redisOccupancy{
		Type:    messageTypeOccupancy,
		Channel: evt.Event.GetChannel(),
		Gen:     evt.Gen,
		Event:   raw,
	})
}

func deserializeOccupancy(data []byte) (*occupancy.OccupancyEvent, error) {
	var m redisOccupancy
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Type != messageTypeOccupancy || m.Gen == 0 || len(m.Event) == 0 {
		return nil, errors.New("malformed occupancy envelope")
	}
	evt := &clientpb.PresenceEvent{}
	if err := protojson.Unmarshal(m.Event, evt); err != nil {
		return nil, fmt.Errorf("decode occupancy presence event: %w", err)
	}
	if m.Channel != "" && evt.Channel == "" {
		evt.Channel = m.Channel
	}
	return &occupancy.OccupancyEvent{Event: evt, Gen: m.Gen}, nil
}
