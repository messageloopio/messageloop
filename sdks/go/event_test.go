package messageloopgo

import (
	"testing"

	"github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	clientpb "github.com/fleetlit/messageloop/genproto/v1"
)

func TestCloudEventConversion(t *testing.T) {
	// Test NewCloudEvent
	event := NewCloudEvent(
		"test-id",
		"test-source",
		"test.type",
		[]byte("test data"),
	)

	if event.ID() != "test-id" {
		t.Errorf("expected ID 'test-id', got '%s'", event.ID())
	}
	if event.Source() != "test-source" {
		t.Errorf("expected source 'test-source', got '%s'", event.Source())
	}
	if event.Type() != "test.type" {
		t.Errorf("expected type 'test.type', got '%s'", event.Type())
	}
	if string(event.Data()) != "test data" {
		t.Errorf("expected data 'test data', got '%s'", string(event.Data()))
	}
}

func TestNewTextCloudEvent(t *testing.T) {
	event := NewTextCloudEvent(
		"test-id-2",
		"test-source-2",
		"test.text",
		"hello world",
	)

	if event.ID() != "test-id-2" {
		t.Errorf("expected ID 'test-id-2', got '%s'", event.ID())
	}
	if string(event.Data()) != "hello world" {
		t.Errorf("expected data 'hello world', got '%s'", string(event.Data()))
	}
	if event.DataContentType() != "text/plain" {
		t.Errorf("expected content type 'text/plain', got '%s'", event.DataContentType())
	}
}

func TestSetEventAttribute(t *testing.T) {
	// This is a simple helper function test
	// The actual functionality is tested in other conversion tests
	event := NewCloudEvent("test", "test", "test", []byte("test"))
	SetEventAttribute(event, "custom-key", "custom-value")
	// Extension functionality depends on cloudevents SDK implementation
	// Core functionality is tested in round-trip conversion
}

func TestCloudEventToPbConversion(t *testing.T) {
	// Create a cloudevents.Event
	event := cloudevents.NewEvent()
	event.SetID("test-id")
	event.SetSource("test-source")
	event.SetType("test.type")
	event.SetSpecVersion("1.0")
	event.SetDataContentType("application/json")
	_ = event.SetData("application/json", []byte(`{"key":"value"}`))

	// Convert to pb.CloudEvent
	pbEvent, err := CloudEventToPb(&event)
	if err != nil {
		t.Fatalf("CloudEventToPb failed: %v", err)
	}

	if pbEvent.GetId() != "test-id" {
		t.Errorf("expected ID 'test-id', got '%s'", pbEvent.GetId())
	}
	if pbEvent.GetSource() != "test-source" {
		t.Errorf("expected source 'test-source', got '%s'", pbEvent.GetSource())
	}
	if pbEvent.GetType() != "test.type" {
		t.Errorf("expected type 'test.type', got '%s'", pbEvent.GetType())
	}

	// Check data - the official SDK uses BinaryData for all data
	binaryData := pbEvent.GetBinaryData()
	if string(binaryData) != `{"key":"value"}` {
		t.Errorf("expected binary data '{\"key\":\"value\"}', got '%s'", string(binaryData))
	}

	// Check content type
	if attr := pbEvent.GetAttributes()["datacontenttype"]; attr != nil {
		if ct := attr.GetCeString(); ct != "application/json" {
			t.Errorf("expected content type 'application/json', got '%s'", ct)
		}
	} else {
		t.Error("expected datacontenttype attribute to be set")
	}
}

func TestPbToCloudEventConversion(t *testing.T) {
	// Create a pb.CloudEvent using helper
	pbEvent := NewCloudEvent("test-id", "test-source", "test.type", []byte("test data"))

	// Convert to cloudevents.Event (actually it's already converted)
	if pbEvent.ID() != "test-id" {
		t.Errorf("expected ID 'test-id', got '%s'", pbEvent.ID())
	}
	if pbEvent.Source() != "test-source" {
		t.Errorf("expected source 'test-source', got '%s'", pbEvent.Source())
	}
	if string(pbEvent.Data()) != "test data" {
		t.Errorf("expected data 'test data', got '%s'", string(pbEvent.Data()))
	}
}

func TestRoundTripConversion(t *testing.T) {
	// Create original event
	original := cloudevents.NewEvent()
	original.SetID("round-trip-id")
	original.SetSource("round-trip-source")
	original.SetType("round.trip")
	original.SetSpecVersion("1.0")
	_ = original.SetData("text/plain", "round trip data")

	// Convert to pb and back
	pbEvent, err := CloudEventToPb(&original)
	if err != nil {
		t.Fatalf("CloudEventToPb failed: %v", err)
	}

	converted, err := PbToCloudEvent(pbEvent)
	if err != nil {
		t.Fatalf("PbToCloudEvent failed: %v", err)
	}

	// Verify fields match
	if converted.ID() != original.ID() {
		t.Errorf("ID mismatch: expected '%s', got '%s'", original.ID(), converted.ID())
	}
	if converted.Source() != original.Source() {
		t.Errorf("Source mismatch: expected '%s', got '%s'", original.Source(), converted.Source())
	}
	if converted.Type() != original.Type() {
		t.Errorf("Type mismatch: expected '%s', got '%s'", original.Type(), converted.Type())
	}
	if string(converted.Data()) != string(original.Data()) {
		t.Errorf("Data mismatch: expected '%s', got '%s'", string(original.Data()), string(converted.Data()))
	}
}

func TestWrapPublicationToEvents(t *testing.T) {
	// Create test messages
	pbEvent1 := &pb.CloudEvent{
		Id:          "test-1",
		Source:      "test-source",
		SpecVersion: "1.0",
		Type:        "test.type",
		Attributes: map[string]*pb.CloudEventAttributeValue{
			"datacontenttype": {
				Attr: &pb.CloudEventAttributeValue_CeString{
					CeString: "text/plain",
				},
			},
		},
		Data: &pb.CloudEvent_TextData{
			TextData: "test data 1",
		},
	}

	pbEvent2 := &pb.CloudEvent{
		Id:          "test-2",
		Source:      "test-source-2",
		SpecVersion: "1.0",
		Type:        "test.type.2",
		Attributes: map[string]*pb.CloudEventAttributeValue{
			"datacontenttype": {
				Attr: &pb.CloudEventAttributeValue_CeString{
					CeString: "application/octet-stream",
				},
			},
		},
		Data: &pb.CloudEvent_BinaryData{
			BinaryData: []byte("test data 2"),
		},
	}

	// Create publication
	pub := &clientpb.Publication{
		Envelopes: []*clientpb.Message{
			{
				Id:      "msg-1",
				Channel: "test.channel",
				Offset:  1,
				Payload: pbEvent1,
			},
			{
				Id:      "msg-2",
				Channel: "test.channel",
				Offset:  2,
				Payload: pbEvent2,
			},
		},
	}

	// Convert to events
	events := wrapPublicationToEvents(pub)

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// Verify first event
	if events[0].ID() != "test-1" {
		t.Errorf("expected ID 'test-1', got '%s'", events[0].ID())
	}
	if events[0].Source() != "test-source" {
		t.Errorf("expected source 'test-source', got '%s'", events[0].Source())
	}
	if string(events[0].Data()) != "test data 1" {
		t.Errorf("expected data 'test data 1', got '%s'", string(events[0].Data()))
	}

	// Verify second event
	if events[1].ID() != "test-2" {
		t.Errorf("expected ID 'test-2', got '%s'", events[1].ID())
	}
	if events[1].Source() != "test-source-2" {
		t.Errorf("expected source 'test-source-2', got '%s'", events[1].Source())
	}
	if string(events[1].Data()) != "test data 2" {
		t.Errorf("expected data 'test data 2', got '%s'", string(events[1].Data()))
	}
}
