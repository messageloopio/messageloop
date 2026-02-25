// Package main demonstrates the MessageLoop v2 protocol components.
// This example shows how to use the v2 encoder, Frame structure,
// and ClientV2 without a live server connection.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	v2 "github.com/messageloopio/messageloop/v2"
	v2pb "github.com/messageloopio/messageloop/shared/genproto/v2"
)

func main() {
	fmt.Println("=== MessageLoop v2 Protocol Example ===")
	fmt.Println()

	// 1. Demonstrate encoder usage
	demonstrateEncoders()

	// 2. Demonstrate ClientV2 message flow
	demonstrateClientV2()

	// 3. Demonstrate dedup cache
	demonstrateDedupCache()
}

func demonstrateEncoders() {
	fmt.Println("--- Encoder Demo ---")

	// Protobuf encoder (production use)
	pbEnc := &v2.ProtobufEncoder{}
	fmt.Printf("Protobuf encoder: %v\n", pbEnc.Encoding())

	// JSON encoder (debugging/human-readable)
	jsonEnc := v2.NewJSONEncoder()
	fmt.Printf("JSON encoder: %v\n", jsonEnc.Encoding())

	// Create a sample message
	msg := &v2pb.Message{
		Payload: &v2pb.Message_Connect{
			Connect: &v2pb.Connect{
				Token:      "eyJhbGciOiJIUzI1NiJ9...",
				ClientType: "browser",
				Metadata: map[string][]byte{
					"user-agent": []byte("Mozilla/5.0"),
					"ip":         []byte("192.168.1.1"),
				},
			},
		},
	}

	// Encode with protobuf
	pbData, err := pbEnc.EncodeMessage(msg)
	if err != nil {
		log.Fatalf("protobuf encode failed: %v", err)
	}
	fmt.Printf("Protobuf encoded: %d bytes\n", len(pbData))

	// Encode with JSON
	jsonData, err := jsonEnc.EncodeMessage(msg)
	if err != nil {
		log.Fatalf("json encode failed: %v", err)
	}
	fmt.Printf("JSON encoded: %d bytes\n", len(jsonData))
	fmt.Printf("JSON payload: %s\n", string(jsonData))
	fmt.Println()
}

func demonstrateClientV2() {
	fmt.Println("--- ClientV2 Demo ---")

	// Create an in-memory transport for demo
	tr := newLoopbackTransport()
	enc := &v2.ProtobufEncoder{}

	var received []string
	client := v2.NewClientV2("demo-session-001", tr, enc,
		v2.WithOnMessage(func(msg *v2pb.Message) {
			switch p := msg.Payload.(type) {
			case *v2pb.Message_Ping:
				received = append(received, fmt.Sprintf("Ping(ts=%d)", p.Ping.Timestamp))
			case *v2pb.Message_Publication:
				received = append(received, fmt.Sprintf("Publication(ch=%s)", p.Publication.Channel))
			default:
				received = append(received, fmt.Sprintf("Message(%T)", p))
			}
		}),
	)

	// Start the receive loop
	go client.Start()

	// Send a Publication to the "client" (demo: inject inbound frame)
	ctx := context.Background()
	if err := client.Publish(ctx, "sports.news", []byte(`{"goals":3}`), "match-update", nil); err != nil {
		log.Fatalf("Publish failed: %v", err)
	}
	fmt.Printf("Published to sports.news (session: %s)\n", client.SessionID())

	// Inject a Ping message as if from the client
	pingMsg := &v2pb.Message{
		Payload: &v2pb.Message_Ping{Ping: &v2pb.Ping{Timestamp: time.Now().UnixMilli()}},
	}
	payload, _ := enc.EncodeMessage(pingMsg)
	frame := &v2pb.Frame{
		FrameId:   (&v2.UUIDGenerator{}).Generate(),
		SessionId: "demo-session-001",
		ClientSeq: 1,
		Timestamp: time.Now().UnixMilli(),
		Encoding:  v2pb.Encoding_ENCODING_PROTOBUF,
		Payload:   payload,
	}
	data, _ := enc.EncodeFrame(frame)
	tr.inject(data)

	time.Sleep(50 * time.Millisecond) // let the receive loop process

	// Disconnect
	if err := client.Disconnect(v2pb.DisconnectCode_DISCONNECT_NORMAL, "bye", true); err != nil {
		log.Printf("Disconnect error: %v", err)
	}
	fmt.Printf("Received inbound messages: %v\n", received)
	fmt.Println()
}

func demonstrateDedupCache() {
	fmt.Println("--- Dedup Cache Demo ---")

	cache := v2.NewDedupCacheWithTTL(100 * time.Millisecond)

	session := "session-abc"

	// First time - not duplicate
	fmt.Printf("seq=1 first time: duplicate=%v\n", cache.IsDuplicate(session, 1))
	// Second time - duplicate
	fmt.Printf("seq=1 second time: duplicate=%v\n", cache.IsDuplicate(session, 1))
	// Different session - not duplicate
	fmt.Printf("seq=1 session-xyz: duplicate=%v\n", cache.IsDuplicate("session-xyz", 1))

	fmt.Printf("Cache size: %d\n", cache.Size())
	time.Sleep(150 * time.Millisecond)
	cache.Evict()
	fmt.Printf("Cache size after TTL eviction: %d\n", cache.Size())
	fmt.Println()
}

// loopbackTransport is an in-memory transport for demonstration.
type loopbackTransport struct {
	inbox chan []byte
	done  chan struct{}
}

func newLoopbackTransport() *loopbackTransport {
	return &loopbackTransport{
		inbox: make(chan []byte, 32),
		done:  make(chan struct{}),
	}
}

func (t *loopbackTransport) Send(_ context.Context, data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case t.inbox <- cp:
	default: // drop if full
	}
	return nil
}

func (t *loopbackTransport) Receive(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case data := <-t.inbox:
		return data, nil
	case <-t.done:
		return nil, fmt.Errorf("transport closed")
	}
}

func (t *loopbackTransport) Close(_ v2pb.DisconnectCode, _ string) error {
	select {
	case <-t.done:
	default:
		close(t.done)
	}
	return nil
}

func (t *loopbackTransport) Done() <-chan struct{} { return t.done }

func (t *loopbackTransport) inject(data []byte) {
	t.inbox <- data
}
