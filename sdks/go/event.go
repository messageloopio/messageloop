package messageloopgo

import (
	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/google/uuid"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/v1"
	"google.golang.org/protobuf/types/known/structpb"
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
	events := make([]*cloudevents.Event, 0, len(pub.GetMessages()))
	for _, env := range pub.GetMessages() {
		if env == nil {
			continue
		}
		payload := env.GetPayload()
		event, err := PayloadToCloudEvent(payload, env.GetChannel())
		if err == nil && event != nil {
			// Attach channel and offset as CloudEvent extensions so consumers
			// using OnMessage(fn events []*cloudevents.Event) can access them.
			if ch := env.GetChannel(); ch != "" {
				event.SetExtension("messageloop_channel", ch)
			}
			// Offset is uint64; store as number
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
	if err := event.SetData(cloudevents.ApplicationJSON, data); err != nil {
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

// PayloadToCloudEvent converts sharedpb.Payload to cloudevents.Event.
func PayloadToCloudEvent(payload *sharedpb.Payload, source string) (*cloudevents.Event, error) {
	if payload == nil {
		return nil, nil
	}

	event := cloudevents.NewEvent()
	event.SetID(uuid.NewString())
	event.SetSource(source)
	event.SetSpecVersion("1.0")
	event.SetType("messageloop.message")

	switch data := payload.GetData().(type) {
	case *sharedpb.Payload_Json:
		event.SetDataContentType(cloudevents.ApplicationJSON)
		jsonBytes, err := data.Json.MarshalJSON()
		if err != nil {
			return nil, err
		}
		_ = event.SetData(cloudevents.ApplicationJSON, jsonBytes)
	case *sharedpb.Payload_Binary:
		event.SetDataContentType("application/octet-stream")
		_ = event.SetData("application/octet-stream", data.Binary)
	}

	return &event, nil
}

// CloudEventToPayload converts cloudevents.Event to sharedpb.Payload.
func CloudEventToPayload(event *cloudevents.Event) (*sharedpb.Payload, error) {
	if event == nil {
		return nil, nil
	}

	payload := &sharedpb.Payload{}

	contentType := event.DataContentType()
	data := event.Data()

	if contentType == cloudevents.ApplicationJSON {
		// Parse JSON data into Struct
		var jsonData map[string]interface{}
		if err := event.DataAs(&jsonData); err != nil {
			// If parsing fails, try as raw bytes
			payload.Data = &sharedpb.Payload_Binary{
				Binary: data,
			}
		} else {
			s, err := structpb.NewStruct(jsonData)
			if err != nil {
				return nil, err
			}
			payload.Data = &sharedpb.Payload_Json{
				Json: s,
			}
		}
	} else {
		// Use binary payload for other content types
		payload.Data = &sharedpb.Payload_Binary{
			Binary: data,
		}
	}

	return payload, nil
}

// RpcReplyToPayload extracts payload from RpcReply message.
func RpcReplyToPayload(reply *clientpb.RpcReply) (*sharedpb.Payload, *sharedpb.Error) {
	if reply == nil {
		return nil, nil
	}
	return reply.GetPayload(), reply.GetError()
}
