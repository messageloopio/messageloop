package v2

import (
	v2pb "github.com/messageloopio/messageloop/shared/genproto/v2"
	"google.golang.org/protobuf/encoding/protojson"
)

// JSONEncoder encodes and decodes Frame/Message using protobuf JSON format.
// Useful for debugging and human-readable wire format.
type JSONEncoder struct {
	marshal   protojson.MarshalOptions
	unmarshal protojson.UnmarshalOptions
}

var _ Encoder = (*JSONEncoder)(nil)

// NewJSONEncoder creates a JSONEncoder with sensible defaults.
func NewJSONEncoder() *JSONEncoder {
	return &JSONEncoder{
		marshal: protojson.MarshalOptions{
			UseProtoNames:   true,
			EmitUnpopulated: false,
		},
		unmarshal: protojson.UnmarshalOptions{
			DiscardUnknown: true,
		},
	}
}

func (e *JSONEncoder) Encoding() v2pb.Encoding {
	return v2pb.Encoding_ENCODING_JSON
}

func (e *JSONEncoder) EncodeFrame(frame *v2pb.Frame) ([]byte, error) {
	return e.marshal.Marshal(frame)
}

func (e *JSONEncoder) DecodeFrame(data []byte) (*v2pb.Frame, error) {
	frame := &v2pb.Frame{}
	if err := e.unmarshal.Unmarshal(data, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

func (e *JSONEncoder) EncodeMessage(msg *v2pb.Message) ([]byte, error) {
	return e.marshal.Marshal(msg)
}

func (e *JSONEncoder) DecodeMessage(data []byte) (*v2pb.Message, error) {
	msg := &v2pb.Message{}
	if err := e.unmarshal.Unmarshal(data, msg); err != nil {
		return nil, err
	}
	return msg, nil
}
