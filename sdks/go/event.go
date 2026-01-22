package messageloopgo

import (
	"fmt"
	"time"

	"github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	clientpb "github.com/fleetlit/messageloop/genproto/v1"
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

// wrapPublication converts a protobuf Publication to a slice of Message.
func wrapPublication(pub *clientpb.Publication) []*Message {
	if pub == nil {
		return nil
	}
	messages := make([]*Message, 0, len(pub.GetEnvelopes()))
	for _, env := range pub.GetEnvelopes() {
		msg := wrapMessage(env)
		if msg != nil {
			messages = append(messages, msg)
		}
	}
	return messages
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

// wrapMessage converts a protobuf Message to a Message.
func wrapMessage(m *clientpb.Message) *Message {
	if m == nil {
		return nil
	}
	pbEvent := m.GetPayload()
	event, err := PbToCloudEvent(pbEvent)
	if err != nil {
		// If conversion fails, return nil or handle error
		return nil
	}
	// Attach channel/offset metadata as CloudEvent extensions
	if event != nil {
		if ch := m.GetChannel(); ch != "" {
			event.SetExtension("messageloop_channel", ch)
		}
		event.SetExtension("messageloop_offset", m.GetOffset())
	}
	data, contentType := extractData(pbEvent)
	return &Message{
		ID:          m.GetId(),
		Channel:     m.GetChannel(),
		Offset:      m.GetOffset(),
		Event:       event,
		Data:        data,
		ContentType: contentType,
	}
}

// extractData extracts the data from a CloudEvent.
func extractData(event *pb.CloudEvent) ([]byte, string) {
	if event == nil {
		return nil, ""
	}

	// Try BinaryData first
	if bd := event.GetBinaryData(); len(bd) > 0 {
		contentType := "application/octet-stream"
		if attrs := event.GetAttributes(); attrs != nil {
			if val := attrs["datacontenttype"]; val != nil {
				if ct := val.GetCeString(); ct != "" {
					contentType = ct
				}
			}
		}
		return bd, contentType
	}

	// Try TextData
	if td := event.GetTextData(); td != "" {
		contentType := "text/plain"
		if attrs := event.GetAttributes(); attrs != nil {
			if val := attrs["datacontenttype"]; val != nil {
				if ct := val.GetCeString(); ct != "" {
					contentType = ct
				}
			}
		}
		return []byte(td), contentType
	}

	// Try ProtoData (any protobuf message)
	if pd := event.GetProtoData(); pd != nil {
		// Return as JSON representation for logging
		// Users should access Event.ProtoData directly for typed data
		return []byte(pd.String()), "application/protobuf"
	}

	return nil, ""
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

// PbToCloudEvent converts pb.CloudEvent to cloudevents.Event
func PbToCloudEvent(pbEvent *pb.CloudEvent) (*cloudevents.Event, error) {
	if pbEvent == nil {
		return nil, fmt.Errorf("pbEvent is nil")
	}

	event := cloudevents.NewEvent()
	event.SetID(pbEvent.GetId())
	event.SetSource(pbEvent.GetSource())
	event.SetType(pbEvent.GetType())
	event.SetSpecVersion(pbEvent.GetSpecVersion())

	// Set attributes/extensions
	for key, attr := range pbEvent.GetAttributes() {
		if attr == nil {
			continue
		}

		switch v := attr.GetAttr().(type) {
		case *pb.CloudEventAttributeValue_CeString:
			if key == "datacontenttype" {
				event.SetDataContentType(v.CeString)
			} else if key == "time" {
				// Parse time if needed
				t, err := time.Parse(time.RFC3339, v.CeString)
				if err == nil {
					event.SetTime(t)
				} else {
					event.SetExtension(key, v.CeString)
				}
			} else {
				event.SetExtension(key, v.CeString)
			}
		case *pb.CloudEventAttributeValue_CeBytes:
			event.SetExtension(key, v.CeBytes)
		case *pb.CloudEventAttributeValue_CeUri:
			event.SetExtension(key, v.CeUri)
		case *pb.CloudEventAttributeValue_CeUriRef:
			event.SetExtension(key, v.CeUriRef)
		case *pb.CloudEventAttributeValue_CeInteger:
			event.SetExtension(key, v.CeInteger)
		case *pb.CloudEventAttributeValue_CeBoolean:
			event.SetExtension(key, v.CeBoolean)
		}
	}

	// Set data
	// Choose a sensible default content type when missing to avoid JSON quoting of text
	currentCT := event.DataContentType()
	switch data := pbEvent.GetData().(type) {
	case *pb.CloudEvent_BinaryData:
		ct := currentCT
		if ct == "" {
			ct = "application/octet-stream"
		}
		if err := event.SetData(ct, data.BinaryData); err != nil {
			return nil, fmt.Errorf("failed to set binary data: %w", err)
		}
	case *pb.CloudEvent_TextData:
		ct := currentCT
		if ct == "" {
			ct = "text/plain"
		}
		if err := event.SetData(ct, data.TextData); err != nil {
			return nil, fmt.Errorf("failed to set text data: %w", err)
		}
	case *pb.CloudEvent_ProtoData:
		ct := currentCT
		if ct == "" {
			ct = "application/protobuf"
		}
		if err := event.SetData(ct, data.ProtoData); err != nil {
			return nil, fmt.Errorf("failed to set proto data: %w", err)
		}
	}

	return &event, nil
}

// CloudEventToPb converts cloudevents.Event to pb.CloudEvent
func CloudEventToPb(event *cloudevents.Event) (*pb.CloudEvent, error) {
	if event == nil {
		return nil, fmt.Errorf("event is nil")
	}

	pbEvent := &pb.CloudEvent{
		Id:          event.ID(),
		Source:      event.Source(),
		SpecVersion: event.SpecVersion(),
		Type:        event.Type(),
		Attributes:  make(map[string]*pb.CloudEventAttributeValue),
	}

	// Set data content type if present
	if event.DataContentType() != "" {
		pbEvent.Attributes["datacontenttype"] = &pb.CloudEventAttributeValue{
			Attr: &pb.CloudEventAttributeValue_CeString{
				CeString: event.DataContentType(),
			},
		}
	}

	// Set time if present
	if !event.Time().IsZero() {
		pbEvent.Attributes["time"] = &pb.CloudEventAttributeValue{
			Attr: &pb.CloudEventAttributeValue_CeString{
				CeString: event.Time().Format(time.RFC3339Nano),
			},
		}
	}

	// Set extensions
	for key, value := range event.Extensions() {
		switch v := value.(type) {
		case string:
			pbEvent.Attributes[key] = &pb.CloudEventAttributeValue{
				Attr: &pb.CloudEventAttributeValue_CeString{
					CeString: v,
				},
			}
		case []byte:
			pbEvent.Attributes[key] = &pb.CloudEventAttributeValue{
				Attr: &pb.CloudEventAttributeValue_CeBytes{
					CeBytes: v,
				},
			}
		case int32:
			pbEvent.Attributes[key] = &pb.CloudEventAttributeValue{
				Attr: &pb.CloudEventAttributeValue_CeInteger{
					CeInteger: v,
				},
			}
		case bool:
			pbEvent.Attributes[key] = &pb.CloudEventAttributeValue{
				Attr: &pb.CloudEventAttributeValue_CeBoolean{
					CeBoolean: v,
				},
			}
		default:
			// Convert unknown types to string
			pbEvent.Attributes[key] = &pb.CloudEventAttributeValue{
				Attr: &pb.CloudEventAttributeValue_CeString{
					CeString: fmt.Sprintf("%v", v),
				},
			}
		}
	}

	// Set data
	dataBytes := event.Data()
	if len(dataBytes) > 0 {
		// Determine data type based on content type
		contentType := event.DataContentType()
		if contentType == "text/plain" || contentType == "application/json" {
			pbEvent.Data = &pb.CloudEvent_TextData{
				TextData: string(dataBytes),
			}
		} else {
			pbEvent.Data = &pb.CloudEvent_BinaryData{
				BinaryData: dataBytes,
			}
		}
	}

	return pbEvent, nil
}
