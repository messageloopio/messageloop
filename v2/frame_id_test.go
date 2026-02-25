package v2_test

import (
	"strings"
	"testing"

	v2 "github.com/messageloopio/messageloop/v2"
)

func TestUUIDGenerator_Generate(t *testing.T) {
	g := &v2.UUIDGenerator{}

	id1 := g.Generate()
	id2 := g.Generate()

	if id1 == "" {
		t.Error("Generate() returned empty string")
	}
	if id1 == id2 {
		t.Error("Generate() returned duplicate IDs")
	}
	// UUID v4 format: xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx
	parts := strings.Split(id1, "-")
	if len(parts) != 5 {
		t.Errorf("expected UUID format with 5 parts, got %d parts: %q", len(parts), id1)
	}
}

func TestUUIDGenerator_Uniqueness(t *testing.T) {
	g := &v2.UUIDGenerator{}
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := g.Generate()
		if seen[id] {
			t.Fatalf("duplicate ID generated at iteration %d: %q", i, id)
		}
		seen[id] = true
	}
}
