package redisbroker

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/redis/go-redis/v9"
)

// consume runs the consumer loop that reads messages from Redis Streams and Pub/Sub.
// It uses separate goroutines for stream reading and pub/sub handling to avoid blocking.
func (b *redisBroker) consume() {
	defer b.wg.Done()

	// Consumer ID for this node
	consumerID := fmt.Sprintf("node-%s", b.nodeID)

	// Shared state between goroutines
	var streams []string
	var mu sync.RWMutex
	pendingIDs := make(map[string]string) // stream -> last read ID

	// Channel for pub/sub messages - buffered to avoid blocking
	pubsubCh := make(chan *redis.Message, 100)

	// Create a pub/sub connection for real-time notifications
	pubSub := b.client.PSubscribe(b.ctx, b.options.PubSubPrefix+"*")
	defer pubSub.Close()

	// Start pub/sub reader goroutine
	go func() {
		for {
			msg, err := pubSub.Receive(b.ctx)
			if err != nil {
				close(pubsubCh)
				return
			}
			if m, ok := msg.(*redis.Message); ok {
				select {
				case pubsubCh <- m:
				case <-b.ctx.Done():
					return
				}
			}
		}
	}()

	// Create a stream reader context
	streamCtx, streamCancel := context.WithCancel(b.ctx)
	defer streamCancel()

	for {
		select {
		case <-b.ctx.Done():
			return

		case msg, ok := <-pubsubCh:
			if !ok {
				return
			}
			// Handle real-time pub/sub notification immediately
			if err := b.handlePubSubMessage(msg); err != nil {
				log.Printf("[redisbroker] error handling pub/sub message: %v", err)
			}

		case <-streamCtx.Done():
			return

		default:
			// Check for new subscriptions
			b.subMu.RLock()
			newSubs := make([]string, 0, len(b.subscribed))
			for ch := range b.subscribed {
				stream := b.options.StreamPrefix + ch
				// Check if we already track this stream
				tracked := false
				mu.RLock()
				for _, s := range streams {
					if s == stream {
						tracked = true
						break
					}
				}
				mu.RUnlock()
				if !tracked {
					newSubs = append(newSubs, ch)
				}
			}
			b.subMu.RUnlock()

			// Add new streams to our list
			for _, ch := range newSubs {
				stream := b.options.StreamPrefix + ch
				mu.Lock()
				streams = append(streams, stream, ">")
				pendingIDs[stream] = ">"
				mu.Unlock()
				log.Printf("[redisbroker] starting to consume stream: %s", stream)
			}

			// If we have streams to read, read them
			if len(streams) > 0 {
				b.readStreams(streamCtx, consumerID, &mu, &pendingIDs, streams)
			} else {
				// No streams, wait a bit before checking again
				select {
				case <-b.ctx.Done():
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		}
	}
}

// readStreams reads messages from Redis Streams using XReadGroup.
// This function is non-blocking and returns immediately when there's no data.
func (b *redisBroker) readStreams(ctx context.Context, consumerID string, mu *sync.RWMutex, pendingIDs *map[string]string, streams []string) {
	// Build the stream IDs argument for XReadGroup
	args := redis.XReadGroupArgs{
		Group:    b.options.ConsumerGroup,
		Consumer: consumerID,
		Streams:  streams,
		Count:    10,
		Block:    100 * time.Millisecond, // Short block for faster response
	}

	result, err := b.client.XReadGroup(ctx, &args).Result()
	if err != nil {
		if err == context.DeadlineExceeded || err == context.Canceled {
			return
		}
		// redis: nil means no messages available (not an error)
		return
	}

	if result == nil {
		return
	}

	// Process each stream's messages
	for _, streamResult := range result {
		stream := streamResult.Stream
		messages := streamResult.Messages

		for _, msg := range messages {
			// Extract channel from stream name
			ch := strings.TrimPrefix(stream, b.options.StreamPrefix)

			// Parse the message payload
			payload, ok := msg.Values["payload"].(string)
			if !ok {
				// Skip messages with invalid format, ack to remove from stream
				b.client.XAck(ctx, stream, b.options.ConsumerGroup, msg.ID)
				continue
			}

			redisMsg, err := deserializeMessage([]byte(payload))
			if err != nil {
				// Skip messages that can't be deserialized, ack to remove from stream
				b.client.XAck(ctx, stream, b.options.ConsumerGroup, msg.ID)
				continue
			}

			// Handle the message
			if err := b.handleMessage(ch, redisMsg); err != nil {
				// Don't ack on error - message will be retried
				continue
			}

			// Acknowledge the message
			b.client.XAck(ctx, stream, b.options.ConsumerGroup, msg.ID)

			// Update pending ID
			mu.Lock()
			(*pendingIDs)[stream] = msg.ID
			mu.Unlock()
		}
	}
}

// handlePubSubMessage processes a message received from Redis Pub/Sub.
func (b *redisBroker) handlePubSubMessage(msg *redis.Message) error {
	// Extract channel from pub/sub channel name
	channelName := msg.Channel
	if len(channelName) <= len(b.options.PubSubPrefix) {
		return nil
	}
	ch := channelName[len(b.options.PubSubPrefix):]

	// Deserialize message
	redisMsg, err := deserializeMessage([]byte(msg.Payload))
	if err != nil {
		return err
	}

	return b.handleMessage(ch, redisMsg)
}

// handleMessage dispatches a message to the appropriate handler.
func (b *redisBroker) handleMessage(ch string, msg *redisMessage) error {
	switch msg.Type {
	case messageTypePublication:
		return b.handlePublication(ch, msg)
	case messageTypeJoin:
		return b.handleJoin(ch, msg)
	case messageTypeLeave:
		return b.handleLeave(ch, msg)
	default:
		log.Printf("[redisbroker] unknown message type: %s", msg.Type)
	}
	return nil
}

// handlePublication processes a publication message.
func (b *redisBroker) handlePublication(ch string, msg *redisMessage) error {
	if b.handler == nil {
		return nil
	}

	// We need to set offset from the stream ID
	// Since we receive via pub/sub, we use 0 as offset for real-time messages
	pub := &messageloop.Publication{
		Channel:  ch,
		Offset:   0, // Will be set by caller if needed
		Metadata: nil,
		Payload:  msg.Payload,
		Time:     time.Now().UnixMilli(),
		IsText:   msg.IsText,
	}

	return b.handler.HandlePublication(ch, pub)
}

// handleJoin processes a join message.
func (b *redisBroker) handleJoin(ch string, msg *redisMessage) error {
	if b.handler == nil {
		return nil
	}
	return b.handler.HandleJoin(ch, msg.Info)
}

// handleLeave processes a leave message.
func (b *redisBroker) handleLeave(ch string, msg *redisMessage) error {
	if b.handler == nil {
		return nil
	}
	return b.handler.HandleLeave(ch, msg.Info)
}
