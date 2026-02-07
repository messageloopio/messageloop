package main

import (
	"context"
	"fmt"
	"log"
	"time"

	messageloopgo "github.com/messageloopio/messageloop/sdks/go"
)

func main() {
	if err := protobufEncodingExample(); err != nil {
		log.Fatal(err)
	}
}

// protobufEncodingExample demonstrates using protobuf encoding.
func protobufEncodingExample() error {
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
