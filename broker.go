package messageloop

import (
	"context"
	"encoding/json"

	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// PayloadKind identifies the original Payload oneof variant of a publication.
type PayloadKind int

const (
	// PayloadKindBinary marks a binary payload.
	PayloadKindBinary PayloadKind = iota
	// PayloadKindText marks a text payload.
	PayloadKindText
	// PayloadKindJSON marks a JSON payload (Payload_Payload_Json on the wire).
	PayloadKindJSON
)

// Publication is a message published to a channel.
// Offset is 0 when history is disabled for the channel.
type Publication struct {
	Channel     string
	Payload     []byte // payload bytes (JSON text for JSON kind)
	Kind        PayloadKind
	ContentType string // MIME content type, may be empty
	Id          string // publisher-provided message id, may be empty
	Metadata    map[string]string
	Offset      uint64
	Time        int64 // Unix milliseconds
	Epoch       string
}

// PayloadProto rebuilds the shared Payload message from the publication,
// preserving the original oneof variant (Binary/Text/JSON).
func (p *Publication) PayloadProto() *sharedpb.Payload {
	if p == nil || len(p.Payload) == 0 {
		return nil
	}
	switch p.Kind {
	case PayloadKindText:
		return &sharedpb.Payload{
			ContentType: p.ContentType,
			Data:        &sharedpb.Payload_Text{Text: string(p.Payload)},
		}
	case PayloadKindJSON:
		var object map[string]any
		if err := json.Unmarshal(p.Payload, &object); err == nil {
			if st, err := structpb.NewStruct(object); err == nil {
				return &sharedpb.Payload{
					ContentType: p.ContentType,
					Data:        &sharedpb.Payload_Json{Json: st},
				}
			}
		}
		// Not valid JSON after all: degrade to text and let the caller log.
		return &sharedpb.Payload{
			ContentType: p.ContentType,
			Data:        &sharedpb.Payload_Text{Text: string(p.Payload)},
		}
	default:
		return &sharedpb.Payload{
			ContentType: p.ContentType,
			Data:        &sharedpb.Payload_Binary{Binary: p.Payload},
		}
	}
}

// PublicationHandler is called by the broker for each incoming publication
// on a subscribed channel.
type PublicationHandler func(ch string, pub *Publication) error

// Broker manages pub/sub message routing and optional per-channel history.
//
// Lifecycle: Start must be called once before Publish/Subscribe/History.
// Start blocks until the provided context is cancelled — call it as a goroutine:
//
//	go broker.Start(ctx, handler)
type Broker interface {
	// Start initializes the broker and processes events until ctx is done.
	Start(ctx context.Context, handler PublicationHandler) error

	// Subscribe registers the node's interest in ch.
	// Called when the first local client subscribes to a channel.
	Subscribe(ch string) error

	// Unsubscribe removes the node's interest in ch.
	// Called when the last local client unsubscribes from a channel.
	Unsubscribe(ch string) error

	// Publish sends payload to all subscribers of ch.
	// Returns the offset assigned to this publication (0 if history is
	// disabled). The assigned offset is also written back to pub.Offset.
	Publish(ch string, pub *Publication) (uint64, error)

	// PublishTransient delivers payload to all subscribers of ch in real
	// time without writing history, so the publication never appears in
	// History. Used for events (e.g. presence join/leave) that must not
	// leak into the recovery message stream.
	PublishTransient(ch string, pub *Publication) error

	// History returns publications stored for ch with offset >= sinceOffset.
	// limit <= 0 uses DefaultHistoryLimit as a safety cap.
	// Returns an empty slice (not an error) when history is disabled or empty.
	History(ch string, sinceOffset uint64, limit int) ([]*Publication, error)
}
