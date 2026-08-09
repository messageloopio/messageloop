package messageloopgo

import (
	"encoding/json"
	"testing"
)

// TestDataNilValueNoPanic reproduces the P1-12 panic: Data created with
// NewData("application/json", nil) has a nil value; every accessor must
// return empty values instead of panicking on the type assertion.
func TestDataNilValueNoPanic(t *testing.T) {
	d, err := NewData("application/json", nil)
	if err != nil {
		t.Fatalf("NewData: %v", err)
	}

	if d.AsJSON() != nil {
		t.Fatalf("AsJSON of nil data = %v, want nil", d.AsJSON())
	}
	if d.AsBinary() != nil {
		t.Fatalf("AsBinary of nil data = %v, want nil", d.AsBinary())
	}
	if d.AsText() != "" {
		t.Fatalf("AsText of nil data = %q, want empty", d.AsText())
	}
	if err := d.As(&map[string]any{}); err != nil {
		t.Fatalf("As of nil JSON data failed: %v", err)
	}

	// ToPayload and String must not panic either.
	m := NewMessageWithData("test", d)
	if _, err := m.ToPayload(); err != nil {
		t.Fatalf("ToPayload of nil JSON data failed: %v", err)
	}
	_ = m.String()

	// Binary and text data with nil value are guarded too.
	bd, err := NewData("application/octet-stream", nil)
	if err != nil {
		t.Fatalf("NewData binary: %v", err)
	}
	if bd.AsBinary() != nil {
		t.Fatalf("AsBinary of nil binary data = %v, want nil", bd.AsBinary())
	}
	var raw []byte
	if err := bd.As(&raw); err == nil {
		t.Fatal("As of nil binary data succeeded, want error")
	}

	td, err := NewData("text/plain", nil)
	if err != nil {
		t.Fatalf("NewData text: %v", err)
	}
	if td.AsText() != "" {
		t.Fatalf("AsText of nil text data = %q, want empty", td.AsText())
	}
	var s string
	if err := td.As(&s); err == nil {
		t.Fatal("As of nil text data succeeded, want error")
	}
}

// TestMessageZeroValueNoPanic reproduces the P1-12 panic on the zero-value
// Message: ToPayload and String must return empty values without panicking.
func TestMessageZeroValueNoPanic(t *testing.T) {
	var m Message

	if m.String() != "" {
		t.Fatalf("String of zero Message = %q, want empty", m.String())
	}

	payload, err := m.ToPayload()
	if err != nil {
		t.Fatalf("ToPayload of zero Message failed: %v", err)
	}
	if payload == nil {
		t.Fatal("ToPayload of zero Message = nil, want empty payload")
	}
	if payload.GetContentType() != "" {
		t.Fatalf("ToPayload of zero Message = %v, want empty content type", payload)
	}
}

// TestDataJSONEmptyValue verifies JSON data with an empty map keeps working
// through the JSON accessor.
func TestDataJSONEmptyValue(t *testing.T) {
	d := NewJSONData(map[string]any{})
	if got := d.AsJSON(); got == nil {
		t.Fatal("AsJSON of empty map = nil, want non-nil empty map")
	}
	if _, err := json.Marshal(d.AsJSON()); err != nil {
		t.Fatalf("marshal of AsJSON result failed: %v", err)
	}
}
