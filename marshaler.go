package messageloop

import (
	"encoding/json"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Marshaler defines the interface for marshaling protocol messages.
type Marshaler interface {
	// Marshal converts a message to bytes.
	Marshal(msg any) ([]byte, error)
	// Unmarshal converts bytes to a message.
	Unmarshal(data []byte, msg any) error
	// Name returns the marshaler name.
	Name() string
	// UseBytes returns true if this marshaler uses binary encoding.
	UseBytes() bool
}

// JSONMarshaler implements JSON marshaling for protocol messages.
type JSONMarshaler struct{}

func (JSONMarshaler) Marshal(msg any) ([]byte, error) {
	if m, ok := msg.(proto.Message); ok {
		return ProtoJSONMarshaler.Marshal(m)
	}
	return json.Marshal(msg)
}

func (JSONMarshaler) Unmarshal(data []byte, msg any) error {
	if m, ok := msg.(proto.Message); ok {
		return ProtoJSONMarshaler.Unmarshal(data, m)
	}
	return json.Unmarshal(data, msg)
}

func (JSONMarshaler) Name() string {
	return "json"
}

func (JSONMarshaler) UseBytes() bool {
	return false
}

// ProtobufMarshaler implements protobuf marshaling for protocol messages.
type ProtobufMarshaler struct{}

func (ProtobufMarshaler) Marshal(msg any) ([]byte, error) {
	m, ok := msg.(proto.Message)
	if !ok {
		return nil, &MarshalTypeError{Type: msg}
	}
	return proto.Marshal(m)
}

func (ProtobufMarshaler) Unmarshal(data []byte, msg any) error {
	m, ok := msg.(proto.Message)
	if !ok {
		return &UnmarshalTypeError{Type: msg}
	}
	return proto.Unmarshal(data, m)
}

func (ProtobufMarshaler) Name() string {
	return "proto"
}

func (ProtobufMarshaler) UseBytes() bool {
	return true
}

// ProtoJSONMarshaler is a JSON marshaler that uses protobuf JSON encoding.
var ProtoJSONMarshaler = &protoJSONMarshaler{
	Marshaler: protojson.MarshalOptions{
		UseProtoNames:   true,
		EmitUnpopulated: false,
	},
	Unmarshaler: protojson.UnmarshalOptions{
		DiscardUnknown: true,
	},
}

type protoJSONMarshaler struct {
	Marshaler   protojson.MarshalOptions
	Unmarshaler protojson.UnmarshalOptions
}

func (p *protoJSONMarshaler) Marshal(msg any) ([]byte, error) {
	m, ok := msg.(proto.Message)
	if !ok {
		return nil, &MarshalTypeError{Type: msg}
	}
	return p.Marshaler.Marshal(m)
}

func (p *protoJSONMarshaler) Unmarshal(data []byte, msg any) error {
	m, ok := msg.(proto.Message)
	if !ok {
		return &UnmarshalTypeError{Type: msg}
	}
	return p.Unmarshaler.Unmarshal(data, m)
}

func (p *protoJSONMarshaler) Name() string {
	return "json"
}

func (p *protoJSONMarshaler) UseBytes() bool {
	return false
}

// Marshalers is a list of available marshalers.
var Marshalers = []Marshaler{
	JSONMarshaler{},
	ProtobufMarshaler{},
}

// MarshalTypeError is returned when Marshal receives an unexpected type.
type MarshalTypeError struct {
	Type any
}

func (e *MarshalTypeError) Error() string {
	return "message is not a proto.Message"
}

// UnmarshalTypeError is returned when Unmarshal receives an unexpected type.
type UnmarshalTypeError struct {
	Type any
}

func (e *UnmarshalTypeError) Error() string {
	return "message is not a proto.Message"
}
