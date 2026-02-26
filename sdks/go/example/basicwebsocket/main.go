package main

import (
	"context"
	"fmt"
	"log"
	"time"

	messageloopgo "github.com/messageloopio/messageloop/sdks/go"
)

func main() {
	if err := basicWebSocketExample(); err != nil {
		log.Fatal(err)
	}
}

// basicWebSocketExample demonstrates a basic WebSocket connection.
func basicWebSocketExample() error {
	// Create a WebSocket client with JSON encoding
	client, err := messageloopgo.Dial(
		"ws://localhost:9080/ws",
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

	client.OnMessage(func(msgs []*messageloopgo.Message) {
		for _, msg := range msgs {
			log.Printf("Received message - ID: %s, Type: %s, ContentType: %s",
				msg.ID, msg.Type, msg.Data.ContentType())
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
	msg := messageloopgo.NewMessageWithData("chat.message", messageloopgo.NewBinaryData([]byte("Hello, MessageLoop!")))
	if err := client.Publish("chat.messages", msg); err != nil {
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
