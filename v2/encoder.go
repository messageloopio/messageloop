package v2

import (
	v2pb "github.com/messageloopio/messageloop/shared/genproto/v2"
)

// Encoder defines the interface for encoding and decoding Frame messages.
// Implementations support different wire formats (Protobuf, JSON, etc.)
type Encoder interface {
	// Encoding returns the encoding type this encoder handles.
	Encoding() v2pb.Encoding

	// EncodeFrame serializes a Frame to bytes.
	EncodeFrame(frame *v2pb.Frame) ([]byte, error)

	// DecodeFrame deserializes bytes into a Frame.
	DecodeFrame(data []byte) (*v2pb.Frame, error)

	// EncodeMessage serializes a Message to bytes.
	EncodeMessage(msg *v2pb.Message) ([]byte, error)

	// DecodeMessage deserializes bytes into a Message.
	DecodeMessage(data []byte) (*v2pb.Message, error)
}
