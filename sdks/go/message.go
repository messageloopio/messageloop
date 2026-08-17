package messageloopgo

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
	"google.golang.org/protobuf/types/known/structpb"
)

// Data represents message payload data with content type (MIME type).
type Data struct {
	contentType string
	value       any
}

// isJSON checks if contentType indicates JSON data.
func (d *Data) isJSON() bool {
	ct := strings.ToLower(d.contentType)
	return strings.HasPrefix(ct, "application/json") || strings.Contains(ct, "json")
}

// isBinary checks if contentType indicates binary data.
func (d *Data) isBinary() bool {
	ct := strings.ToLower(d.contentType)
	return !strings.HasPrefix(ct, "text/") && !d.isJSON()
}

// isText checks if contentType indicates text data.
func (d *Data) isText() bool {
	ct := strings.ToLower(d.contentType)
	return strings.HasPrefix(ct, "text/")
}

// ContentType returns the MIME type of the data.
func (d *Data) ContentType() string {
	return d.contentType
}

// AsJSON returns the data as a JSON map. Returns nil if data is not JSON or
// has no value.
func (d *Data) AsJSON() map[string]any {
	if d.isJSON() && d.value != nil {
		return d.value.(map[string]any)
	}
	return nil
}

// AsBinary returns the data as bytes. Returns nil if data is not binary or
// has no value.
func (d *Data) AsBinary() []byte {
	if d.isBinary() && d.value != nil {
		return d.value.([]byte)
	}
	return nil
}

// AsText returns the data as a string. Returns empty string if data is not
// text or has no value.
func (d *Data) AsText() string {
	if d.isText() && d.value != nil {
		return d.value.(string)
	}
	return ""
}

// As decodes the data into the provided target. The target must be a pointer.
// For JSON data, it unmarshals into the target.
// For binary data, it attempts to unmarshal as JSON first, then returns the raw bytes if target is *[]byte.
// For text data, it attempts to unmarshal as JSON first, then assigns the string if target is *string.
func (d *Data) As(out any) error {
	if out == nil {
		return fmt.Errorf("target cannot be nil")
	}

	if d.isJSON() {
		jsonBytes, err := json.Marshal(d.value)
		if err != nil {
			return fmt.Errorf("failed to marshal JSON data: %w", err)
		}
		return json.Unmarshal(jsonBytes, out)
	}

	if d.isBinary() {
		if d.value == nil {
			return fmt.Errorf("no data to decode")
		}
		// Try to unmarshal as JSON first
		binary := d.value.([]byte)
		if err := json.Unmarshal(binary, out); err != nil {
			// If target is *[]byte, return raw bytes
			if b, ok := out.(*[]byte); ok {
				*b = binary
				return nil
			}
			return fmt.Errorf("failed to unmarshal binary data: %w", err)
		}
		return nil
	}

	if d.isText() {
		if d.value == nil {
			return fmt.Errorf("no data to decode")
		}
		// Try to unmarshal as JSON first
		text := d.value.(string)
		if err := json.Unmarshal([]byte(text), out); err != nil {
			// If target is *string, return the text
			if s, ok := out.(*string); ok {
				*s = text
				return nil
			}
			return fmt.Errorf("failed to unmarshal text data: %w", err)
		}
		return nil
	}

	return fmt.Errorf("no data to decode")
}

// NewJSONData creates a new Data with JSON content.
func NewJSONData(data map[string]any) Data {
	return Data{
		contentType: "application/json",
		value:       data,
	}
}

// NewBinaryData creates a new Data with binary content.
func NewBinaryData(data []byte) Data {
	return Data{
		contentType: "application/octet-stream",
		value:       data,
	}
}

// NewTextData creates a new Data with text content.
func NewTextData(text string) Data {
	return Data{
		contentType: "text/plain",
		value:       text,
	}
}

// NewData creates a new Data from content type and value.
// It automatically detects the data type based on content type and value type.
func NewData(contentType string, data any) (Data, error) {
	d := Data{contentType: contentType}

	if data == nil {
		return d, nil
	}

	// Determine data type from content type
	ct := strings.ToLower(contentType)
	if strings.HasPrefix(ct, "application/json") || strings.Contains(ct, "json") {
		switch v := data.(type) {
		case map[string]any:
			d.value = v
		default:
			// Try to marshal and unmarshal as JSON
			jsonBytes, err := json.Marshal(data)
			if err != nil {
				return d, fmt.Errorf("failed to marshal data as JSON: %w", err)
			}
			var jsonData map[string]any
			if err := json.Unmarshal(jsonBytes, &jsonData); err != nil {
				return d, fmt.Errorf("failed to unmarshal data as JSON: %w", err)
			}
			d.value = jsonData
		}
	} else if strings.HasPrefix(ct, "text/") {
		switch v := data.(type) {
		case string:
			d.value = v
		case []byte:
			d.value = string(v)
		default:
			return d, fmt.Errorf("text content type requires string or []byte data")
		}
	} else {
		// Default to binary
		switch v := data.(type) {
		case []byte:
			d.value = v
		case string:
			d.value = []byte(v)
		default:
			return d, fmt.Errorf("binary content type requires []byte or string data")
		}
	}

	return d, nil
}

// Message represents a message with data payload and metadata.
type Message struct {
	// ID is the unique message identifier
	ID string
	// Type indicates the message type
	Type string
	// Data contains the actual message payload
	Data Data
	// Metadata contains additional key-value pairs
	Metadata map[string]string
}

// NewMessage creates a new message with auto-generated ID.
func NewMessage(msgType string) *Message {
	return &Message{
		ID:       uuid.NewString(),
		Type:     msgType,
		Metadata: make(map[string]string),
	}
}

// NewMessageWithData creates a new message with the specified data.
func NewMessageWithData(msgType string, data Data) *Message {
	msg := NewMessage(msgType)
	msg.Data = data
	return msg
}

// SetData sets the data on the message, automatically detecting the data type
// based on content type and value type.
func (m *Message) SetData(contentType string, data any) error {
	d, err := NewData(contentType, data)
	if err != nil {
		return err
	}
	m.Data = d
	return nil
}

// SetMetadata sets a metadata key-value pair.
func (m *Message) SetMetadata(key, value string) {
	if m.Metadata == nil {
		m.Metadata = make(map[string]string)
	}
	m.Metadata[key] = value
}

// GetMetadata gets a metadata value by key. Returns empty string if not found.
func (m *Message) GetMetadata(key string) string {
	if m.Metadata == nil {
		return ""
	}
	return m.Metadata[key]
}

// DataAs decodes the message data into the provided target.
// This is a convenience method that calls m.Data.As(out).
func (m *Message) DataAs(out any) error {
	return m.Data.As(out)
}

// ToPayload converts Message to protobuf Payload.
func (m *Message) ToPayload() (*sharedv2.Payload, error) {
	payload := &sharedv2.Payload{
		ContentType: m.Data.contentType,
	}

	if m.Data.isJSON() {
		s, err := structpb.NewStruct(m.Data.AsJSON())
		if err != nil {
			return nil, fmt.Errorf("failed to convert JSON to struct: %w", err)
		}
		payload.Data = &sharedv2.Payload_Json{Json: s}
	} else if m.Data.isBinary() {
		payload.Data = &sharedv2.Payload_Binary{Binary: m.Data.AsBinary()}
	} else if m.Data.isText() {
		payload.Data = &sharedv2.Payload_Text{Text: m.Data.AsText()}
	}

	return payload, nil
}

// PayloadToMessage converts protobuf Payload to Message.
func PayloadToMessage(payload *sharedv2.Payload, id string) *Message {
	msg := NewMessage("messageloop.message")
	if id != "" {
		msg.ID = id
	}

	if payload == nil {
		return msg
	}

	msg.Data.contentType = payload.GetContentType()

	switch d := payload.GetData().(type) {
	case *sharedv2.Payload_Json:
		msg.Data.value = d.Json.AsMap()
		if msg.Data.contentType == "" {
			msg.Data.contentType = "application/json"
		}
	case *sharedv2.Payload_Binary:
		msg.Data.value = d.Binary
		if msg.Data.contentType == "" {
			msg.Data.contentType = "application/octet-stream"
		}
	case *sharedv2.Payload_Text:
		msg.Data.value = d.Text
		if msg.Data.contentType == "" {
			msg.Data.contentType = "text/plain"
		}
	}

	return msg
}

// posOffset returns the offset carried by a v2 Position and whether it was
// set. An unset offset (nil) is a deliberate "no hint" and is never treated
// as 0-means-start.
func posOffset(p *sharedv2.Position) (uint64, bool) {
	if p == nil || p.Offset == nil {
		return 0, false
	}
	return p.GetOffset(), true
}

// Position builds a v2 Position with a set offset. Use it with WithRecover to
// resume a subscription from a known point: for example
// WithRecover(Position(epoch, lastSeenOffset)). A nil cursor passed to
// WithRecover is a "no hint" recover.
func Position(streamEpoch string, offset uint64) *sharedv2.Position {
	return &sharedv2.Position{StreamEpoch: streamEpoch, Offset: &offset}
}

// ReceivedMessage represents a message received from a subscribed channel.
type ReceivedMessage struct {
	// ID is the unique message identifier
	ID string
	// Channel is the channel the message was published to
	Channel string
	// Offset is the message offset in the channel; 0 with OffsetSet false when
	// the wire position carried no offset
	Offset    uint64
	OffsetSet bool
	// Position is the authoritative wire position of the message
	Position *sharedv2.Position
	// Replay is true for messages delivered by a recovery replay rather than
	// in real time
	Replay bool
	// Message is the decoded message
	Message *Message
}

// messageFromEnv converts one client-v2 Message envelope into the SDK Message
// type, tagging the channel and (when set) the position offset in metadata.
// Replay and live messages share this single consumer path (§5).
func messageFromEnv(env *clientpb.Message) *Message {
	if env == nil {
		return nil
	}
	msg := PayloadToMessage(env.GetPayload(), env.GetId())
	msg.Type = "messageloop.message"
	msg.SetMetadata("channel", env.GetChannel())
	if off, set := posOffset(env.GetPosition()); set {
		msg.SetMetadata("offset", fmt.Sprintf("%d", off))
	}
	return msg
}

// String returns a string representation of the message for debugging.
func (m *Message) String() string {
	if m == nil {
		return ""
	}

	if m.Data.isJSON() {
		if jsonBytes, err := json.Marshal(m.Data.AsJSON()); err == nil {
			return string(jsonBytes)
		}
	} else if m.Data.isBinary() {
		return string(m.Data.AsBinary())
	} else if m.Data.isText() {
		return m.Data.AsText()
	}
	return ""
}
