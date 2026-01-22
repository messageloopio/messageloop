package messageloopsdk

import (
	pb "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
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
	Event *pb.CloudEvent
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

// wrapMessage converts a protobuf Message to a Message.
func wrapMessage(m *clientpb.Message) *Message {
	if m == nil {
		return nil
	}
	event := m.GetPayload()
	data, contentType := extractData(event)
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
func NewCloudEvent(id, source, eventType string, data []byte) *pb.CloudEvent {
	event := &pb.CloudEvent{
		Id:          id,
		Source:      source,
		SpecVersion: "1.0",
		Type:        eventType,
		Attributes:  make(map[string]*pb.CloudEventAttributeValue),
	}

	if len(data) > 0 {
		event.Data = &pb.CloudEvent_BinaryData{
			BinaryData: data,
		}
		// Set default content type
		event.Attributes["datacontenttype"] = &pb.CloudEventAttributeValue{
			Attr: &pb.CloudEventAttributeValue_CeString{
				CeString: "application/octet-stream",
			},
		}
	}

	return event
}

// NewTextCloudEvent creates a new CloudEvent with text data for publishing.
func NewTextCloudEvent(id, source, eventType, textData string) *pb.CloudEvent {
	event := &pb.CloudEvent{
		Id:          id,
		Source:      source,
		SpecVersion: "1.0",
		Type:        eventType,
		Attributes:  make(map[string]*pb.CloudEventAttributeValue),
	}

	if textData != "" {
		event.Data = &pb.CloudEvent_TextData{
			TextData: textData,
		}
		// Set default content type
		event.Attributes["datacontenttype"] = &pb.CloudEventAttributeValue{
			Attr: &pb.CloudEventAttributeValue_CeString{
				CeString: "text/plain",
			},
		}
	}

	return event
}

// SetEventAttribute sets an attribute on a CloudEvent.
func SetEventAttribute(event *pb.CloudEvent, key, value string) {
	if event == nil {
		return
	}
	if event.Attributes == nil {
		event.Attributes = make(map[string]*pb.CloudEventAttributeValue)
	}
	event.Attributes[key] = &pb.CloudEventAttributeValue{
		Attr: &pb.CloudEventAttributeValue_CeString{
			CeString: value,
		},
	}
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
