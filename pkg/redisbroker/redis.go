package redisbroker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/redis/go-redis/v9"
)

// redisBroker implements messageloop.Broker using Redis Streams (history)
// and Redis Pub/Sub (real-time fan-out).
type redisBroker struct {
	client  *redis.Client
	opts    *Options
	handler messageloop.PublicationHandler

	subMu      sync.RWMutex
	subscribed map[string]struct{}
}

// New creates a new Redis-backed Broker.
// Call go broker.Start(ctx, handler) to start processing events.
func New(cfg config.RedisConfig) messageloop.Broker {
	opts := NewOptions(cfg)
	return &redisBroker{
		client: redis.NewClient(&redis.Options{
			Addr:         opts.Addr,
			Password:     opts.Password,
			DB:           opts.DB,
			PoolSize:     opts.PoolSize,
			MinIdleConns: opts.MinIdleConns,
			MaxRetries:   opts.MaxRetries,
			DialTimeout:  opts.DialTimeout,
			ReadTimeout:  opts.ReadTimeout,
			WriteTimeout: opts.WriteTimeout,
		}),
		opts:       opts,
		subscribed: make(map[string]struct{}),
	}
}

// Start verifies the Redis connection and then runs the Pub/Sub consumer loop
// until ctx is cancelled. Intended to be called as: go broker.Start(ctx, handler).
func (b *redisBroker) Start(ctx context.Context, handler messageloop.PublicationHandler) error {
	b.handler = handler

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := b.client.Ping(pingCtx).Err(); err != nil {
		return fmt.Errorf("redis broker: connect: %w", err)
	}

	defer b.client.Close()
	return b.runPubSubWithRetry(ctx)
}

// Subscribe registers interest in ch on this node.
func (b *redisBroker) Subscribe(ch string) error {
	b.subMu.Lock()
	b.subscribed[ch] = struct{}{}
	b.subMu.Unlock()
	return nil
}

// Unsubscribe removes interest in ch on this node.
func (b *redisBroker) Unsubscribe(ch string) error {
	b.subMu.Lock()
	delete(b.subscribed, ch)
	b.subMu.Unlock()
	return nil
}

// Publish writes payload to the Redis Stream (for history) and broadcasts via
// Pub/Sub (for real-time delivery). Returns the stream offset assigned.
func (b *redisBroker) Publish(ch string, payload []byte, isText bool) (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	msg := &redisMessage{Type: messageTypePublication, Channel: ch, Payload: payload, IsText: isText}

	// First, write to stream to get the offset.
	streamData, err := serializeMessage(msg)
	if err != nil {
		return 0, err
	}
	stream := b.opts.StreamPrefix + ch
	id, err := b.client.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		MaxLen: b.opts.StreamMaxLength,
		Approx: b.opts.StreamApproximate,
		Values: map[string]interface{}{"data": streamData},
	}).Result()
	if err != nil {
		return 0, err
	}
	if err := b.client.Expire(ctx, stream, b.opts.HistoryTTL).Err(); err != nil {
		log.WarnContext(ctx, "failed to set stream TTL", "stream", stream, "error", err)
	}

	offset := parseStreamOffset(id)

	// Serialize again with offset included for pub/sub delivery.
	msg.Offset = offset
	pubSubData, err := serializeMessage(msg)
	if err != nil {
		return 0, err
	}
	if err := b.client.Publish(ctx, b.opts.PubSubPrefix+ch, pubSubData).Err(); err != nil {
		return 0, err
	}

	return offset, nil
}

// History returns publications stored for ch with offset >= sinceOffset.
func (b *redisBroker) History(ch string, sinceOffset uint64, limit int) ([]*messageloop.Publication, error) {
	return b.getHistory(ch, sinceOffset, limit)
}

var _ messageloop.Broker = (*redisBroker)(nil)
