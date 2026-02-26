package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

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

	client.OnMessage(func(msgs []*messageloopgo.Message) {
		for _, msg := range msgs {
			log.Printf("Received message - ID: %s, Type: %s, Data: %s",
				msg.ID, msg.Type, msg.String())
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
	msg := messageloopgo.NewMessageWithData("chat.message", messageloopgo.NewTextData("Hello via gRPC!"))
	if err := client.Publish("chat.messages", msg); err != nil {
		return fmt.Errorf("publish failed: %w", err)
	}

	// Make an RPC call
	req := messageloopgo.NewMessageWithData("getUser", messageloopgo.NewJSONData(map[string]any{
		"message": "hello world",
	}))
	resp := messageloopgo.NewMessage("")
	err = client.RPC(ctx, "user.service", "getUser", req, resp)
	if err != nil {
		return fmt.Errorf("rpc failed: %w", err)
	}

	log.Printf("RPC response: %s", resp.String())

	// Keep running
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return nil
	}
}
