package v2_test

import (
	"testing"

	v2 "github.com/messageloopio/messageloop/v2"
	v2pb "github.com/messageloopio/messageloop/shared/genproto/v2"
)

func TestProtobufEncoder_RoundTrip(t *testing.T) {
	enc := &v2.ProtobufEncoder{}

	if enc.Encoding() != v2pb.Encoding_ENCODING_PROTOBUF {
		t.Fatalf("expected PROTOBUF encoding, got %v", enc.Encoding())
	}

	frame := &v2pb.Frame{
		FrameId:   "test-frame-1",
		SessionId: "session-abc",
		ClientSeq: 42,
		Timestamp: 1700000000000,
		Flags:     uint32(v2pb.FrameFlags_FLAG_ACK_REQUIRED),
		Encoding:  v2pb.Encoding_ENCODING_PROTOBUF,
		Payload:   []byte("hello"),
	}

	data, err := enc.EncodeFrame(frame)
	if err != nil {
		t.Fatalf("EncodeFrame failed: %v", err)
	}

	decoded, err := enc.DecodeFrame(data)
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}

	if decoded.FrameId != frame.FrameId {
		t.Errorf("FrameId mismatch: got %q, want %q", decoded.FrameId, frame.FrameId)
	}
	if decoded.ClientSeq != frame.ClientSeq {
		t.Errorf("ClientSeq mismatch: got %d, want %d", decoded.ClientSeq, frame.ClientSeq)
	}
	if string(decoded.Payload) != string(frame.Payload) {
		t.Errorf("Payload mismatch: got %q, want %q", decoded.Payload, frame.Payload)
	}
}

func TestProtobufEncoder_Message_RoundTrip(t *testing.T) {
	enc := &v2.ProtobufEncoder{}

	msg := &v2pb.Message{
		Payload: &v2pb.Message_Ping{
			Ping: &v2pb.Ping{Timestamp: 1234567890},
		},
	}

	data, err := enc.EncodeMessage(msg)
	if err != nil {
		t.Fatalf("EncodeMessage failed: %v", err)
	}

	decoded, err := enc.DecodeMessage(data)
	if err != nil {
		t.Fatalf("DecodeMessage failed: %v", err)
	}

	ping, ok := decoded.Payload.(*v2pb.Message_Ping)
	if !ok {
		t.Fatalf("expected Ping payload, got %T", decoded.Payload)
	}
	if ping.Ping.Timestamp != 1234567890 {
		t.Errorf("Timestamp mismatch: got %d, want 1234567890", ping.Ping.Timestamp)
	}
}

func TestJSONEncoder_RoundTrip(t *testing.T) {
	enc := v2.NewJSONEncoder()

	if enc.Encoding() != v2pb.Encoding_ENCODING_JSON {
		t.Fatalf("expected JSON encoding, got %v", enc.Encoding())
	}

	frame := &v2pb.Frame{
		FrameId:   "test-frame-json",
		SessionId: "session-xyz",
		ClientSeq: 7,
		Timestamp: 1700000000000,
		Encoding:  v2pb.Encoding_ENCODING_JSON,
	}

	data, err := enc.EncodeFrame(frame)
	if err != nil {
		t.Fatalf("EncodeFrame failed: %v", err)
	}

	decoded, err := enc.DecodeFrame(data)
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}

	if decoded.FrameId != frame.FrameId {
		t.Errorf("FrameId mismatch: got %q, want %q", decoded.FrameId, frame.FrameId)
	}
	if decoded.ClientSeq != frame.ClientSeq {
		t.Errorf("ClientSeq mismatch: got %d, want %d", decoded.ClientSeq, frame.ClientSeq)
	}
}

func TestJSONEncoder_Message_RoundTrip(t *testing.T) {
	enc := v2.NewJSONEncoder()

	msg := &v2pb.Message{
		Payload: &v2pb.Message_Connect{
			Connect: &v2pb.Connect{
				Token:      "my-token",
				ClientType: "browser",
			},
		},
	}

	data, err := enc.EncodeMessage(msg)
	if err != nil {
		t.Fatalf("EncodeMessage failed: %v", err)
	}

	decoded, err := enc.DecodeMessage(data)
	if err != nil {
		t.Fatalf("DecodeMessage failed: %v", err)
	}

	conn, ok := decoded.Payload.(*v2pb.Message_Connect)
	if !ok {
		t.Fatalf("expected Connect payload, got %T", decoded.Payload)
	}
	if conn.Connect.Token != "my-token" {
		t.Errorf("Token mismatch: got %q, want %q", conn.Connect.Token, "my-token")
	}
}
