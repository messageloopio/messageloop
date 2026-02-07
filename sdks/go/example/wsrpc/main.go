package main

import (
	"context"
	"fmt"
	"log"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	messageloopgo "github.com/messageloopio/messageloop/sdks/go"
)

func main() {
	if err := RPCExample(); err != nil {
		log.Fatal(err)
	}
}

// RPCExample demonstrates making RPC calls.
func RPCExample() error {
	// Create a WebSocket client
	client, err := messageloopgo.Dial(
		"ws://localhost:9080/ws",
		messageloopgo.WithClientID("rpc-example"),
	)
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}
	defer client.Close()

	// Connect to the server (waits for connection to be established)
	ctx := context.Background()
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}

	// Make an RPC call
	req, _ := messageloopgo.NewJSONMessage(
		"getUser",
		map[string]any{
			"message": "hello world",
		},
	)
	resp := cloudevents.NewEvent()
	err = client.RPC(ctx, "user.service", "GetUser", req, &resp)
	if err != nil {
		return fmt.Errorf("rpc failed: %w", err)
	}

	log.Printf("RPC response: %s", resp.Data())

	return nil
}
