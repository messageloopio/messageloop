package runtime

import (
	"context"
	"reflect"
	"testing"
)

// --- memory broker node integration ---
//
// These two tests lived in broker_memory_test.go before PR-KA-D11 moved the
// memory broker to internal/stream. They exercise the root Node, so they
// stay here; the concrete type check now goes through reflection because
// *stream.memoryBroker is unexported.

func TestMemoryBroker_IntegrationWithNode(t *testing.T) {
	node := NewNode(nil)
	if node.Broker() == nil {
		t.Fatal("Node should have a default broker")
	}
	if got := reflect.TypeOf(node.Broker()).String(); got != "*stream.memoryBroker" {
		t.Errorf("Node default broker should be *stream.memoryBroker, got %s", got)
	}
}

func TestMemoryBroker_Node_Run(t *testing.T) {
	node := NewNode(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := node.Run(ctx); err != nil {
		t.Fatalf("node.Run: %v", err)
	}
}
