package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	messageloopgo "github.com/messageloopio/messageloop/sdks/go"
)

func main() {
	if err := BasicGRPCExample(); err != nil {
		log.Fatal(err)
	}
}

// BasicGRPCExample demonstrates a basic gRPC connection.
func BasicGRPCExample() error {
	serverAddr := os.Getenv("SERVER_ADDR")
	if serverAddr == "" {
		serverAddr = "localhost:9090"
	}
	// Create a gRPC client
	client, err := messageloopgo.DialGRPC(
		serverAddr,
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

	// Make an RPC call
	req, _ := messageloopgo.NewJSONMessage(
		"getUser",
		map[string]any{
			"message": "hello world",
		},
	)
	resp := cloudevents.NewEvent()
	err = client.RPC(ctx, "user.service", "getUser", req, &resp)
	if err != nil {
		return fmt.Errorf("rpc failed: %w", err)
	}

	// Keep running
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return nil
	}
}
