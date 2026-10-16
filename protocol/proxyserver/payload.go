package proxyserver

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"

	sharedv2 "github.com/messageloopio/messageloop/protocol/genproto/messageloop/shared/v2"
)

// Payload helpers over the sharedv2.Payload oneof (json Struct / binary /
// text). They exist so proxy backends never hand-roll the variant switch.

// PayloadText returns the text variant, or "" for any other shape.
func PayloadText(p *sharedv2.Payload) string {
	if p == nil {
		return ""
	}
	return p.GetText()
}

// PayloadBinary returns the binary variant, or nil for any other shape.
func PayloadBinary(p *sharedv2.Payload) []byte {
	if p == nil {
		return nil
	}
	return p.GetBinary()
}

// PayloadJSON decodes the json variant into into. It never falls back to the
// text/binary variants: a payload produced by a JSON-encoding client arrives
// as a Struct, but text/binary payloads are the caller's to interpret.
func PayloadJSON(p *sharedv2.Payload, into any) error {
	if p == nil || p.GetJson() == nil {
		return fmt.Errorf("payload has no json variant")
	}
	raw, err := p.GetJson().MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshal payload struct: %w", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("decode payload json: %w", err)
	}
	return nil
}

// NewTextPayload builds a text payload.
func NewTextPayload(s string) *sharedv2.Payload {
	return &sharedv2.Payload{
		ContentType: "text/plain",
		Data:        &sharedv2.Payload_Text{Text: s},
	}
}

// NewBinaryPayload builds a binary payload.
func NewBinaryPayload(b []byte) *sharedv2.Payload {
	return &sharedv2.Payload{
		ContentType: "application/octet-stream",
		Data:        &sharedv2.Payload_Binary{Binary: b},
	}
}

// NewJSONPayload builds a json payload from any JSON-marshalable value.
func NewJSONPayload(v any) (*sharedv2.Payload, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode payload json: %w", err)
	}
	s := &structpb.Struct{}
	if err := s.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("encode payload struct: %w", err)
	}
	return &sharedv2.Payload{
		ContentType: "application/json",
		Data:        &sharedv2.Payload_Json{Json: s},
	}, nil
}
