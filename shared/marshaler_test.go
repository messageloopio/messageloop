package shared

import (
	"strings"
	"testing"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	"google.golang.org/protobuf/proto"
)

type testStruct struct {
	Name string
}

func allMarshalers() []Marshaler {
	return []Marshaler{JSONMarshaler{}, ProtobufMarshaler{}, ProtoJSONMarshaler}
}

func testProtoMsg() *clientpb.OutboundMessage {
	return &clientpb.OutboundMessage{
		Id: "test-id",
		Envelope: &clientpb.OutboundMessage_Connected{
			Connected: &clientpb.Connected{SessionId: "session-123"},
		},
	}
}

func TestMarshalersNames(t *testing.T) {
	want := map[string]Marshaler{
		"json":      JSONMarshaler{},
		"proto":     ProtobufMarshaler{},
		"protojson": ProtoJSONMarshaler,
	}
	if len(Marshalers) != len(want) {
		t.Fatalf("len(Marshalers) = %d, want %d", len(Marshalers), len(want))
	}
	seen := make(map[string]bool, len(Marshalers))
	for i, m := range Marshalers {
		name := m.Name()
		if name == "" {
			t.Fatalf("Marshalers[%d] has empty name", i)
		}
		if seen[name] {
			t.Fatalf("duplicate marshaler name %q", name)
		}
		seen[name] = true
		if _, ok := want[name]; !ok {
			t.Fatalf("unexpected marshaler name %q", name)
		}
	}
}

func TestMarshalersProtoRoundTrip(t *testing.T) {
	for _, m := range allMarshalers() {
		original := testProtoMsg()

		data, err := m.Marshal(original)
		if err != nil {
			t.Fatalf("%s.Marshal: %v", m.Name(), err)
		}
		if len(data) == 0 {
			t.Fatalf("%s.Marshal: empty output", m.Name())
		}

		appended, err := m.MarshalAppend([]byte("prefix"), original)
		if err != nil {
			t.Fatalf("%s.MarshalAppend: %v", m.Name(), err)
		}
		if string(appended[:6]) != "prefix" {
			t.Fatalf("%s.MarshalAppend: prefix lost: %q", m.Name(), appended[:min(len(appended), 6)])
		}

		var decoded clientpb.OutboundMessage
		if err := m.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("%s.Unmarshal: %v", m.Name(), err)
		}
		if !proto.Equal(&decoded, original) {
			t.Fatalf("%s round trip mismatch: %v != %v", m.Name(), &decoded, original)
		}
	}
}

func TestMarshalersNonProtoStruct(t *testing.T) {
	// The generic JSON marshaler handles non-proto values; the protobuf ones
	// reject them with a typed error.
	original := testStruct{Name: "test"}
	data, err := JSONMarshaler{}.Marshal(original)
	if err != nil {
		t.Fatalf("JSONMarshaler.Marshal: %v", err)
	}
	var decoded testStruct
	unmarshalErr := JSONMarshaler{}.Unmarshal(data, &decoded)
	if unmarshalErr != nil {
		t.Fatalf("JSONMarshaler.Unmarshal: %v", unmarshalErr)
	}
	if decoded.Name != original.Name {
		t.Fatalf("decoded.Name = %q, want %q", decoded.Name, original.Name)
	}

	for _, m := range []Marshaler{ProtobufMarshaler{}, ProtoJSONMarshaler} {
		_, err := m.Marshal(original)
		if err == nil {
			t.Fatalf("%s.Marshal: want error for non-proto value", m.Name())
		}
		if _, ok := err.(*MarshalTypeError); !ok {
			t.Fatalf("%s.Marshal: want MarshalTypeError, got %T", m.Name(), err)
		}
		err = m.Unmarshal([]byte("{}"), &decoded)
		if err == nil {
			t.Fatalf("%s.Unmarshal: want error for non-proto value", m.Name())
		}
		if _, ok := err.(*UnmarshalTypeError); !ok {
			t.Fatalf("%s.Unmarshal: want UnmarshalTypeError, got %T", m.Name(), err)
		}
	}
}

func TestTypeErrorsDistinguishable(t *testing.T) {
	marshalErr := &MarshalTypeError{Type: testStruct{}}
	unmarshalErr := &UnmarshalTypeError{Type: testStruct{}}

	if marshalErr.Error() == unmarshalErr.Error() {
		t.Fatalf("MarshalTypeError and UnmarshalTypeError produce identical messages: %q", marshalErr.Error())
	}
	if !strings.Contains(marshalErr.Error(), "marshal") {
		t.Fatalf("MarshalTypeError.Error() = %q, want it to mention the operation", marshalErr.Error())
	}
	if !strings.Contains(unmarshalErr.Error(), "unmarshal") {
		t.Fatalf("UnmarshalTypeError.Error() = %q, want it to mention the operation", unmarshalErr.Error())
	}
	for _, msg := range []string{marshalErr.Error(), unmarshalErr.Error()} {
		if !strings.Contains(msg, "testStruct") {
			t.Fatalf("error %q does not include the offending type", msg)
		}
		if !strings.Contains(msg, "proto.Message") {
			t.Fatalf("error %q does not describe the expectation", msg)
		}
	}
}

func TestProtoJSONMarshalerUseProtoNames(t *testing.T) {
	msg := &clientpb.InboundMessage{
		Id: "id-1",
		Envelope: &clientpb.InboundMessage_RpcRequest{
			RpcRequest: &clientpb.RpcRequest{Channel: "ch", Method: "m"},
		},
	}
	data, err := ProtoJSONMarshaler.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{"id", "rpc_request", "channel", "method"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("protojson output %s missing proto name %q", data, want)
		}
	}
}

func TestProtoJSONMarshalerDiscardUnknown(t *testing.T) {
	jsonData := `{"id":"x","unknown_field":42,"pong":{}}`
	var msg clientpb.OutboundMessage
	if err := ProtoJSONMarshaler.Unmarshal([]byte(jsonData), &msg); err != nil {
		t.Fatalf("Unmarshal with unknown field: %v", err)
	}
	if msg.Id != "x" {
		t.Fatalf("Id = %q, want %q", msg.Id, "x")
	}
}
