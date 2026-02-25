package v2

import (
	v2pb "github.com/messageloopio/messageloop/shared/genproto/v2"
	"google.golang.org/protobuf/proto"
)

// ProtobufEncoder encodes and decodes Frame/Message using protobuf binary format.
type ProtobufEncoder struct{}

var _ Encoder = (*ProtobufEncoder)(nil)

func (e *ProtobufEncoder) Encoding() v2pb.Encoding {
	return v2pb.Encoding_ENCODING_PROTOBUF
}

func (e *ProtobufEncoder) EncodeFrame(frame *v2pb.Frame) ([]byte, error) {
	return proto.Marshal(frame)
}

func (e *ProtobufEncoder) DecodeFrame(data []byte) (*v2pb.Frame, error) {
	frame := &v2pb.Frame{}
	if err := proto.Unmarshal(data, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

func (e *ProtobufEncoder) EncodeMessage(msg *v2pb.Message) ([]byte, error) {
	return proto.Marshal(msg)
}

func (e *ProtobufEncoder) DecodeMessage(data []byte) (*v2pb.Message, error) {
	msg := &v2pb.Message{}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil, err
	}
	return msg, nil
}
