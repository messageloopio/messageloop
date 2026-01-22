package messageloop

import (
	"encoding/json"
	"testing"

	cloudevents "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	sharedpb "github.com/fleetlit/messageloop/genproto/shared/v1"
	clientpb "github.com/fleetlit/messageloop/genproto/v1"
)

func TestJSONMarshaler_Name(t *testing.T) {
	m := JSONMarshaler{}
	if got := m.Name(); got != "json" {
		t.Errorf("JSONMarshaler.Name() = %v, want %v", got, "json")
	}
}

func TestJSONMarshaler_UseBytes(t *testing.T) {
	m := JSONMarshaler{}
	if got := m.UseBytes(); got != false {
		t.Errorf("JSONMarshaler.UseBytes() = %v, want %v", got, false)
	}
}

func TestJSONMarshaler_Marshal_Unmarshal_NonProto(t *testing.T) {
	m := JSONMarshaler{}

	// Test with regular struct
	type testStruct struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	original := testStruct{Name: "test", Value: 42}

	// Marshal
	data, err := m.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	// Unmarshal
	var decoded testStruct
	err = m.Unmarshal(data, &decoded)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if decoded.Name != original.Name {
		t.Errorf("Name = %v, want %v", decoded.Name, original.Name)
	}
	if decoded.Value != original.Value {
		t.Errorf("Value = %v, want %v", decoded.Value, original.Value)
	}
}

func TestJSONMarshaler_Marshal_ProtoMessage(t *testing.T) {
	m := JSONMarshaler{}

	msg := &clientpb.OutboundMessage{
		Id: "test-id",
		Envelope: &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		},
	}

	data, err := m.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	// Should be valid JSON
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Result is not valid JSON: %v", err)
	}

	if raw["id"] != "test-id" {
		t.Errorf("id = %v, want %v", raw["id"], "test-id")
	}
}

func TestJSONMarshaler_Unmarshal_ProtoMessage(t *testing.T) {
	m := JSONMarshaler{}

	// Create a JSON representation of a proto message
	jsonData := `{"id":"test-id","pong":{}}`

	var msg clientpb.OutboundMessage
	err := m.Unmarshal([]byte(jsonData), &msg)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if msg.Id != "test-id" {
		t.Errorf("Id = %v, want %v", msg.Id, "test-id")
	}
}

func TestProtobufMarshaler_Name(t *testing.T) {
	m := ProtobufMarshaler{}
	if got := m.Name(); got != "proto" {
		t.Errorf("ProtobufMarshaler.Name() = %v, want %v", got, "proto")
	}
}

func TestProtobufMarshaler_UseBytes(t *testing.T) {
	m := ProtobufMarshaler{}
	if got := m.UseBytes(); got != true {
		t.Errorf("ProtobufMarshaler.UseBytes() = %v, want %v", got, true)
	}
}

func TestProtobufMarshaler_Marshal_Unmarshal_ProtoMessage(t *testing.T) {
	m := ProtobufMarshaler{}

	original := &clientpb.OutboundMessage{
		Id: "test-id-123",
		Envelope: &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		},
	}

	// Marshal
	data, err := m.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	// Unmarshal
	var decoded clientpb.OutboundMessage
	err = m.Unmarshal(data, &decoded)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if decoded.Id != original.Id {
		t.Errorf("Id = %v, want %v", decoded.Id, original.Id)
	}
}

func TestProtobufMarshaler_Marshal_NonProtoError(t *testing.T) {
	m := ProtobufMarshaler{}

	type testStruct struct {
		Name string
	}

	_, err := m.Marshal(testStruct{Name: "test"})
	if err == nil {
		t.Fatal("Marshal() should return error for non-ProtoMessage")
	}

	if _, ok := err.(*MarshalTypeError); !ok {
		t.Errorf("Error should be MarshalTypeError, got %T", err)
	}
	// Check the error message
	if err.Error() != "message is not a proto.Message" {
		t.Errorf("Error message = %v, want %v", err.Error(), "message is not a proto.Message")
	}
}

func TestMarshalTypeError_Error(t *testing.T) {
	err := &MarshalTypeError{Type: "testType"}
	if got := err.Error(); got != "message is not a proto.Message" {
		t.Errorf("MarshalTypeError.Error() = %v, want %v", got, "message is not a proto.Message")
	}
}

func TestProtobufMarshaler_Unmarshal_NonProtoError(t *testing.T) {
	m := ProtobufMarshaler{}

	type testStruct struct {
		Name string
	}

	err := m.Unmarshal([]byte("test"), testStruct{})
	if err == nil {
		t.Fatal("Unmarshal() should return error for non-ProtoMessage")
	}

	if _, ok := err.(*UnmarshalTypeError); !ok {
		t.Errorf("Error should be UnmarshalTypeError, got %T", err)
	}
}

func TestUnmarshalTypeError_Error(t *testing.T) {
	err := &UnmarshalTypeError{Type: "testType"}
	if got := err.Error(); got != "message is not a proto.Message" {
		t.Errorf("UnmarshalTypeError.Error() = %v, want %v", got, "message is not a proto.Message")
	}
}

func TestProtoJSONMarshaler_Name(t *testing.T) {
	if got := ProtoJSONMarshaler.Name(); got != "json" {
		t.Errorf("ProtoJSONMarshaler.Name() = %v, want %v", got, "json")
	}
}

func TestProtoJSONMarshaler_UseBytes(t *testing.T) {
	if got := ProtoJSONMarshaler.UseBytes(); got != false {
		t.Errorf("ProtoJSONMarshaler.UseBytes() = %v, want %v", got, false)
	}
}

func TestProtoJSONMarshaler_Marshal_Unmarshal(t *testing.T) {
	original := &clientpb.OutboundMessage{
		Id: "test-id-456",
		Metadata: map[string]string{
			"key1": "value1",
			"key2": "value2",
		},
		Envelope: &clientpb.OutboundMessage_Connected{
			Connected: &clientpb.Connected{
				SessionId: "session-123",
			},
		},
	}

	// Marshal
	data, err := ProtoJSONMarshaler.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	// Unmarshal
	var decoded clientpb.OutboundMessage
	err = ProtoJSONMarshaler.Unmarshal(data, &decoded)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if decoded.Id != original.Id {
		t.Errorf("Id = %v, want %v", decoded.Id, original.Id)
	}
	if decoded.GetConnected().SessionId != original.GetConnected().SessionId {
		t.Errorf("SessionId = %v, want %v", decoded.GetConnected().SessionId, original.GetConnected().SessionId)
	}
}

func TestProtoJSONMarshaler_Marshal_NonProtoError(t *testing.T) {
	type testStruct struct {
		Name string
	}

	_, err := ProtoJSONMarshaler.Marshal(testStruct{Name: "test"})
	if err == nil {
		t.Fatal("Marshal() should return error for non-ProtoMessage")
	}
}

func TestProtoJSONMarshaler_Unmarshal_NonProtoError(t *testing.T) {
	type testStruct struct {
		Name string
	}

	err := ProtoJSONMarshaler.Unmarshal([]byte("{}"), testStruct{})
	if err == nil {
		t.Fatal("Unmarshal() should return error for non-ProtoMessage")
	}
}

func TestMarshalers(t *testing.T) {
	marshalers := Marshalers
	if len(marshalers) != 2 {
		t.Errorf("len(Marshalers) = %v, want %v", len(marshalers), 2)
	}

	// Check that each marshaler implements the interface
	for i, m := range marshalers {
		if m.Name() == "" {
			t.Errorf("Marshaler %d has empty name", i)
		}
		// Just verify it doesn't panic
		_ = m.UseBytes()
	}
}

func TestJSONMarshaler_RoundTripCloudEvent(t *testing.T) {
	m := JSONMarshaler{}

	original := &cloudevents.CloudEvent{
		Id:          "event-123",
		Source:      "test-channel",
		SpecVersion: "1.0",
		Type:        "test.event",
		Attributes: map[string]*cloudevents.CloudEventAttributeValue{
			"datacontenttype": {
				Attr: &cloudevents.CloudEventAttributeValue_CeString{
					CeString: "application/json",
				},
			},
		},
		Data: &cloudevents.CloudEvent_TextData{
			TextData: `{"message":"hello"}`,
		},
	}

	data, err := m.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded cloudevents.CloudEvent
	err = m.Unmarshal(data, &decoded)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if decoded.Id != original.Id {
		t.Errorf("Id = %v, want %v", decoded.Id, original.Id)
	}
	if decoded.Source != original.Source {
		t.Errorf("Source = %v, want %v", decoded.Source, original.Source)
	}
}

func TestProtobufMarshaler_RoundTripCloudEvent(t *testing.T) {
	m := ProtobufMarshaler{}

	original := &cloudevents.CloudEvent{
		Id:          "event-456",
		Source:      "test-channel-2",
		SpecVersion: "1.0",
		Type:        "test.event.2",
		Data: &cloudevents.CloudEvent_BinaryData{
			BinaryData: []byte("test payload"),
		},
	}

	data, err := m.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded cloudevents.CloudEvent
	err = m.Unmarshal(data, &decoded)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if decoded.Id != original.Id {
		t.Errorf("Id = %v, want %v", decoded.Id, original.Id)
	}
	if decoded.Source != original.Source {
		t.Errorf("Source = %v, want %v", decoded.Source, original.Source)
	}
}

func TestJSONMarshaler_MarshalError(t *testing.T) {
	m := JSONMarshaler{}

	// Channel without buffering can't be marshaled to JSON
	_, err := m.Marshal(make(chan int))
	if err == nil {
		t.Fatal("Marshal() should return error for non-serializable type")
	}
}

func TestProtoJSONMarshaler_DiscardUnknown(t *testing.T) {
	// Test that unknown fields are discarded (configured in Unmarshaler)
	jsonWithUnknown := `{"id":"test","unknownField":"should be ignored","pong":{}}`

	var msg clientpb.OutboundMessage
	err := ProtoJSONMarshaler.Unmarshal([]byte(jsonWithUnknown), &msg)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if msg.Id != "test" {
		t.Errorf("Id = %v, want %v", msg.Id, "test")
	}
}

func TestProtoJSONMarshaler_UseProtoNames(t *testing.T) {
	msg := &sharedpb.Error{
		Code:    "TEST_ERROR",
		Type:    "test_type",
		Message: "test error message",
	}

	data, err := ProtoJSONMarshaler.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	// Check that it uses proto field names (snake_case) not JSON names
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Failed to parse JSON: %v", err)
	}

	// Proto field names should be present
	if _, ok := raw["code"]; !ok {
		t.Error("Field 'code' should be present (using proto names)")
	}
	if _, ok := raw["type"]; !ok {
		t.Error("Field 'type' should be present (using proto names)")
	}
}

func TestMarshalersList(t *testing.T) {
	// Verify Marshalers contains expected marshalers
	if len(Marshalers) < 2 {
		t.Errorf("Marshalers should have at least 2 entries, got %d", len(Marshalers))
	}

	names := make(map[string]bool)
	for _, m := range Marshalers {
		names[m.Name()] = true
	}

	if !names["json"] {
		t.Error("Marshalers should contain a JSON marshaler")
	}
	if !names["proto"] {
		t.Error("Marshalers should contain a Protobuf marshaler")
	}
}

func BenchmarkJSONMarshaler_Marshal(b *testing.B) {
	m := JSONMarshaler{}
	msg := &clientpb.OutboundMessage{
		Id: "bench-id",
		Envelope: &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = m.Marshal(msg)
	}
}

func BenchmarkProtobufMarshaler_Marshal(b *testing.B) {
	m := ProtobufMarshaler{}
	msg := &clientpb.OutboundMessage{
		Id: "bench-id",
		Envelope: &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = m.Marshal(msg)
	}
}

func BenchmarkProtoJSONMarshaler_Marshal(b *testing.B) {
	msg := &clientpb.OutboundMessage{
		Id: "bench-id",
		Envelope: &clientpb.OutboundMessage_Pong{
			Pong: &clientpb.Pong{},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ProtoJSONMarshaler.Marshal(msg)
	}
}
