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
	if err := BasicQUICExample(); err != nil {
		log.Fatal(err)
	}
}

// BasicQUICExample demonstrates a basic QUIC connection.
func BasicQUICExample() error {
	serverAddr := os.Getenv("SERVER_ADDR")
	if serverAddr == "" {
		serverAddr = "localhost:4433"
	}
	client, err := messageloopgo.DialQUIC(
		serverAddr,
		messageloopgo.WithClientID("example-quic-client"),
		messageloopgo.WithInsecureSkipVerify(),
	)
	if err != nil {
		return fmt.Errorf("dial quic failed: %w", err)
	}
	defer client.Close()

	client.OnConnected(func(sessionID string) {
		log.Printf("Connected via QUIC! Session ID: %s", sessionID)
	})

	client.OnMessage(func(msgs []*messageloopgo.Message) {
		for _, msg := range msgs {
			log.Printf("Received message - ID: %s, Type: %s, Data: %s",
				msg.ID, msg.Type, msg.String())
		}
	})

	ctx := context.Background()
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}

	if err := client.Subscribe("chat.messages"); err != nil {
		return fmt.Errorf("subscribe failed: %w", err)
	}

	msg := messageloopgo.NewMessageWithData("chat.message", messageloopgo.NewTextData("Hello via QUIC!"))
	if err := client.Publish("chat.messages", msg); err != nil {
		return fmt.Errorf("publish failed: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return nil
	}
}
