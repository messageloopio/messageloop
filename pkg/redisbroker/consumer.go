package redisbroker

import (
	"log"
	"time"

	"github.com/fleetlit/messageloop"
	"github.com/redis/go-redis/v9"
)

// consume runs the consumer loop that reads messages from Redis Streams.
func (b *redisBroker) consume() {
	defer b.wg.Done()

	// Subscribe to pub/sub channel for new messages
	pubSub := b.client.Subscribe(b.ctx, b.options.PubSubPrefix+"*")
	defer pubSub.Close()

	// Create a separate channel for the consumer
	ch := pubSub.Channel()

	for {
		select {
		case <-b.ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if err := b.handlePubSubMessage(msg); err != nil {
				log.Printf("[redisbroker] error handling pub/sub message: %v", err)
			}
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
