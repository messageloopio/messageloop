package main

import (
	"context"
	"fmt"
	"log"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	messageloopgo "github.com/messageloopio/messageloop/sdks/go"
)

func main() {
	if err := dynamicSubscriptionExample(); err != nil {
		log.Fatal(err)
	}
}

// dynamicSubscriptionExample demonstrates dynamic subscription management.
func dynamicSubscriptionExample() error {
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
