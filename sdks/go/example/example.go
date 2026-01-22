package example

import (
	"context"
	"fmt"
	"log"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/fleetlit/messageloop/sdks/go"
)

// BasicWebSocketExample demonstrates a basic WebSocket connection.
func BasicWebSocketExample() error {
	// Create a WebSocket client with JSON encoding
	client, err := messageloopgo.Dial(
		"ws://localhost:8080/ws",
		messageloopgo.WithEncoding(messageloopgo.EncodingJSON),
		messageloopgo.WithClientID("example-client"),
		messageloopgo.WithAutoSubscribe("chat.messages"),
	)
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}
	defer client.Close()

	// Set up handlers
	client.OnConnected(func(sessionID string) {
		log.Printf("Connected! Session ID: %s", sessionID)
	})

	client.OnMessage(func(events []*cloudevents.Event) {
		for _, event := range events {
			log.Printf("Received event - ID: %s, Type: %s, Source: %s",
				event.ID(), event.Type(), event.Source())
		}
	})

	client.OnError(func(err error) {
		log.Printf("Error: %v", err)
	})

	// Connect to the server (waits for connection to be established)
	ctx := context.Background()
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}

	// Subscribe to more channels
	if err := client.Subscribe("chat.presence", "chat.typing"); err != nil {
		return fmt.Errorf("subscribe failed: %w", err)
	}

	// Publish a message
	event := messageloopgo.NewCloudEvent(
		"msg-123",
		"/client/example",
		"chat.message",
		[]byte("Hello, MessageLoop!"),
	)
	if err := client.Publish("chat.messages", event); err != nil {
		return fmt.Errorf("publish failed: %w", err)
	}

	// Keep running
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return nil
	}
}

// BasicGRPCExample demonstrates a basic gRPC connection.
func BasicGRPCExample() error {
	// Create a gRPC client
	client, err := messageloopgo.DialGRPC(
		"localhost:9090",
		messageloopgo.WithClientID("example-grpc-client"),
	)
	if err != nil {
		return fmt.Errorf("dial grpc failed: %w", err)
	}
	defer client.Close()

	// Set up handlers
	client.OnConnected(func(sessionID string) {
		log.Printf("Connected via gRPC! Session ID: %s", sessionID)
	})

	client.OnMessage(func(events []*cloudevents.Event) {
		for _, event := range events {
			log.Printf("Received event - ID: %s, Type: %s, Data: %s",
				event.ID(), event.Type(), string(event.Data()))
		}
	})

	// Connect to the server
	ctx := context.Background()
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}

	if err := client.Subscribe("chat.messages", "chat.presence", "chat.typing"); err != nil {
		return fmt.Errorf("subscribe failed: %w", err)
	}

	// Publish a message
	event := messageloopgo.NewTextCloudEvent(
		"msg-456",
		"/client/example",
		"chat.message",
		"Hello via gRPC!",
	)
	if err := client.Publish("chat.messages", event); err != nil {
		return fmt.Errorf("publish failed: %w", err)
	}

	// Keep running
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return nil
	}
}

// ProtobufEncodingExample demonstrates using protobuf encoding.
func ProtobufEncodingExample() error {
	// Create a WebSocket client with protobuf encoding
	client, err := messageloopgo.Dial(
		"ws://localhost:8080/ws",
		messageloopgo.WithEncoding(messageloopgo.EncodingProtobuf),
		messageloopgo.WithClientID("protobuf-example"),
	)
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}
	defer client.Close()

	// Connect to the server
	ctx := context.Background()
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}

	// Publish messages with protobuf encoding
	event := messageloopgo.NewCloudEvent(
		"msg-789",
		"/client/example",
		"chat.message",
		[]byte("Protobuf encoded message"),
	)
	if err := client.Publish("chat.messages", event); err != nil {
		return fmt.Errorf("publish failed: %w", err)
	}

	// Keep running for a bit
	time.Sleep(5 * time.Second)
	return nil
}

// DynamicSubscriptionExample demonstrates dynamic subscription management.
func DynamicSubscriptionExample() error {
	client, err := messageloopgo.Dial(
		"ws://localhost:8080/ws",
		messageloopgo.WithClientID("dynamic-sub-example"),
	)
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}
	defer client.Close()

	// Track received messages
	messageCount := 0

	client.OnMessage(func(events []*cloudevents.Event) {
		messageCount += len(events)
		log.Printf("Received %d events (total: %d)", len(events), messageCount)
	})

	// Connect
	ctx := context.Background()
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}

	time.Sleep(1 * time.Second)

	// Subscribe to channels dynamically
	channels := []string{"channel.1", "channel.2", "channel.3"}
	for _, ch := range channels {
		if err := client.Subscribe(ch); err != nil {
			log.Printf("Failed to subscribe to %s: %v", ch, err)
		} else {
			log.Printf("Subscribed to %s", ch)
		}
	}

	// Unsubscribe after 10 seconds
	time.Sleep(10 * time.Second)
	for _, ch := range channels[:2] {
		if err := client.Unsubscribe(ch); err != nil {
			log.Printf("Failed to unsubscribe from %s: %v", ch, err)
		} else {
			log.Printf("Unsubscribed from %s", ch)
		}
	}

	// Keep running
	time.Sleep(10 * time.Second)
	return nil
}

//
//func main() {
//	if err := BasicGRPCExample(); err != nil {
//		log.Printf("Failed to connect to gRPC server: %v", err)
//	}
//}
