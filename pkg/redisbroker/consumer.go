package redisbroker

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/redis/go-redis/v9"
)

// consume runs the consumer loop that reads messages from Redis Streams using Consumer Groups.
// It combines stream reading (for persistence) with pub/sub (for real-time notifications).
func (b *redisBroker) consume() {
	defer b.wg.Done()

	// Consumer ID for this node
	consumerID := fmt.Sprintf("node-%s", b.nodeID)

	// Start with empty stream list
	var streams []string
	pendingIDs := make(map[string]string) // stream -> last read ID

	// Create a pub/sub connection for real-time notifications
	pubSub := b.client.PSubscribe(b.ctx, b.options.PubSubPrefix+"*")
	defer pubSub.Close()

	// Channel for pub/sub messages
	pubsubCh := pubSub.Channel()

	// Create a stream reader context that we can restart
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
			// Handle real-time pub/sub notification
			if err := b.handlePubSubMessage(msg); err != nil {
				log.Printf("[redisbroker] error handling pub/sub message: %v", err)
			}

		default:
			// Check for new subscriptions
			b.subMu.RLock()
			newSubs := make([]string, 0, len(b.subscribed))
			for ch := range b.subscribed {
				stream := b.options.StreamPrefix + ch
				// Check if we already track this stream
				tracked := false
				for _, s := range streams {
					if s == stream {
						tracked = true
						break
					}
				}
				if !tracked {
					newSubs = append(newSubs, ch)
				}
			}
			b.subMu.RUnlock()

			// Add new streams to our list
			for _, ch := range newSubs {
				stream := b.options.StreamPrefix + ch
				// XReadGroup requires alternating stream keys and IDs: [key1, id1, key2, id2, ...]
				streams = append(streams, stream, ">")
				pendingIDs[stream] = ">"
				log.Printf("[redisbroker] starting to consume stream: %s", stream)
			}

			// If we have streams to read, use XReadGroup
			if len(streams) > 0 {
				if err := b.readStreams(streamCtx, consumerID, streams, pendingIDs); err != nil {
					if err == context.Canceled {
						return
					}
					log.Printf("[redisbroker] stream read error, will retry: %v", err)
					// Wait before retrying
					select {
					case <-b.ctx.Done():
						return
					case <-time.After(time.Second):
					}
				}
			} else {
				// No streams to read, wait a bit before checking again
				select {
				case <-b.ctx.Done():
					return
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
	}
}

// readStreams reads messages from Redis Streams using XReadGroup.
func (b *redisBroker) readStreams(ctx context.Context, consumerID string, streams []string, pendingIDs map[string]string) error {
	// Build the stream IDs argument for XReadGroup
	args := redis.XReadGroupArgs{
		Group:    b.options.ConsumerGroup,
		Consumer: consumerID,
		Streams:  streams,
		Count:    10,
		Block:    5 * time.Second, // Block for up to 5 seconds
	}

	result, err := b.client.XReadGroup(ctx, &args).Result()
	if err != nil {
		if err == context.DeadlineExceeded || err == context.Canceled {
			return err
		}
		return fmt.Errorf("XReadGroup failed: %w", err)
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
				log.Printf("[redisbroker] invalid message format in stream %s", stream)
				continue
			}

			redisMsg, err := deserializeMessage([]byte(payload))
			if err != nil {
				log.Printf("[redisbroker] failed to deserialize message: %v", err)
				continue
			}

			// Handle the message
			if err := b.handleMessage(ch, redisMsg); err != nil {
				log.Printf("[redisbroker] error handling message: %v", err)
				// Don't ack on error - message will be retried
				continue
			}

			// Acknowledge the message
			if err := b.client.XAck(ctx, stream, b.options.ConsumerGroup, msg.ID).Err(); err != nil {
				log.Printf("[redisbroker] failed to ack message: %v", err)
			}

			// Update pending ID
			pendingIDs[stream] = msg.ID
		}
	}

	return nil
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
