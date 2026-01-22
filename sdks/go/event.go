package messageloopgo

import (
	"github.com/cloudevents/sdk-go/binding/format/protobuf/v2"
	"github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	clientpb "github.com/fleetlit/messageloop/genproto/v1"
	"github.com/google/uuid"
)

// Message represents a received message from a subscribed channel.
type Message struct {
	// ID is the unique message identifier
	ID string
	// Channel is the channel the message was published to
	Channel string
	// Offset is the message offset in the channel
	Offset uint64
	// Event is the CloudEvent containing the message data
	Event *cloudevents.Event
	// Data is the raw message data (from BinaryData or TextData)
	Data []byte
	// ContentType is the content type of the data
	ContentType string
}

// wrapPublicationToEvents converts a protobuf Publication to a slice of cloudevents.Event.
func wrapPublicationToEvents(pub *clientpb.Publication) []*cloudevents.Event {
	if pub == nil {
		return nil
	}
	events := make([]*cloudevents.Event, 0, len(pub.GetEnvelopes()))
	for _, env := range pub.GetEnvelopes() {
		if env == nil {
			continue
		}
		pbEvent := env.GetPayload()
		event, err := PbToCloudEvent(pbEvent)
		if err == nil && event != nil {
			// Attach channel and offset as CloudEvent extensions so consumers
			// using OnMessage(fn events []*cloudevents.Event) can access them.
			if ch := env.GetChannel(); ch != "" {
				event.SetExtension("messageloop_channel", ch)
			}
			// Offset is uint64; store as number (will become string when serialized by Pb conversion)
			event.SetExtension("messageloop_offset", env.GetOffset())
			events = append(events, event)
		}
	}
	return events
}

// NewCloudEvent creates a new CloudEvent for publishing.
func NewCloudEvent(id, source, eventType string, data []byte) *cloudevents.Event {
	event := cloudevents.NewEvent()
	event.SetID(id)
	event.SetSource(source)
	event.SetSpecVersion("1.0")
	event.SetType(eventType)

	if len(data) > 0 {
		event.SetDataContentType("application/octet-stream")
		_ = event.SetData("application/octet-stream", data)
	}

	return &event
}

func NewJSONMessage(eventType string, data any) (*cloudevents.Event, error) {
	event := cloudevents.NewEvent()
	event.SetID(uuid.NewString())
	event.SetSource("messageloop/go-sdk")
	event.SetSpecVersion("1.0")
	event.SetType(eventType)
	if err := event.SetData(cloudevents.ApplicationJSON, data); err != nil {
		return nil, err
	}
	return &event, nil
}

func NewProtoMessage(eventType string, data any) (*cloudevents.Event, error) {
	event := cloudevents.NewEvent()
	event.SetID(uuid.NewString())
	event.SetSource("messageloop/go-sdk")
	event.SetSpecVersion("1.0")
	event.SetType(eventType)
	if err := event.SetData(format.ContentTypeProtobuf, data); err != nil {
		return nil, err
	}
	return &event, nil
}

// NewTextCloudEvent creates a new CloudEvent with text data for publishing.
func NewTextCloudEvent(id, source, eventType, textData string) *cloudevents.Event {
	event := cloudevents.NewEvent()
	event.SetID(id)
	event.SetSource(source)
	event.SetSpecVersion("1.0")
	event.SetType(eventType)

	if textData != "" {
		event.SetDataContentType("text/plain")
		_ = event.SetData("text/plain", textData)
	}

	return &event
}

// SetEventAttribute sets an attribute on a CloudEvent.
func SetEventAttribute(event *cloudevents.Event, key, value string) {
	if event == nil {
		return
	}
	event.SetExtension(key, value)
}

// String returns a string representation of the message for debugging.
func (m *Message) String() string {
	if m == nil {
		return ""
	}
	if m.Event != nil {
		return m.Event.String()
	}
	if len(m.Data) > 0 {
		return string(m.Data)
	}
	return ""
}

// PbToCloudEvent converts pb.CloudEvent to cloudevents.Event using the official SDK.
func PbToCloudEvent(pbEvent *pb.CloudEvent) (*cloudevents.Event, error) {
	return format.FromProto(pbEvent)
}

// CloudEventToPb converts cloudevents.Event to pb.CloudEvent using the official SDK.
func CloudEventToPb(event *cloudevents.Event) (*pb.CloudEvent, error) {
	return format.ToProto(event)
}
