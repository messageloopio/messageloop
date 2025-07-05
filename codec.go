package messageloop

import (
	"github.com/lynx-go/x/encoding/json"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type Marshaler interface {
	Name() string
	Binary() bool
	Marshal(data any) ([]byte, error)
	Unmarshal(data []byte, out any) error
}

var DefaultJSONMarshaler = &JSONMarshaler{}

var DefaultProtoMarshaler = &ProtoMarshaler{}

var DefaultProtoJsonMarshaler = &ProtoJsonMarshaler{}

var Marshalers = []Marshaler{
	//DefaultJSONMarshaler,
	DefaultProtoMarshaler,
	DefaultProtoJsonMarshaler,
}

type JSONMarshaler struct {
}

func (m *JSONMarshaler) Binary() bool {
	return false
}

func (m *JSONMarshaler) Name() string {
	return "json"
}

func (m *JSONMarshaler) Marshal(data any) ([]byte, error) {
	return json.Marshal(data)
}

func (m *JSONMarshaler) Unmarshal(data []byte, out any) error {
	return json.Unmarshal(data, out)
}

var _ Marshaler = new(JSONMarshaler)

type ProtoMarshaler struct{}

func (m *ProtoMarshaler) Binary() bool {
	return true
}

func (m *ProtoMarshaler) Name() string {
	return "protobuf"
}

func (m *ProtoMarshaler) Marshal(data any) ([]byte, error) {
	return proto.Marshal(data.(proto.Message))
}

func (m *ProtoMarshaler) Unmarshal(data []byte, out any) error {
	return proto.Unmarshal(data, out.(proto.Message))
}

var _ Marshaler = new(ProtoMarshaler)

type ProtoJsonMarshaler struct{}

func (m *ProtoJsonMarshaler) Binary() bool {
	return false
}

func (m *ProtoJsonMarshaler) Name() string {
	return "json"
}

func (m *ProtoJsonMarshaler) Marshal(data any) ([]byte, error) {
	return protojson.Marshal(data.(proto.Message))
}

func (m *ProtoJsonMarshaler) Unmarshal(data []byte, out any) error {
	return protojson.Unmarshal(data, out.(proto.Message))
}

var _ Marshaler = new(ProtoJsonMarshaler)
