package redisbroker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/deeplooplabs/messageloop"
	"github.com/deeplooplabs/messageloop/config"
	"github.com/redis/go-redis/v9"
)

// redisBroker implements the Broker interface using Redis Streams and Pub/Sub.
type redisBroker struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	client   *redis.Client
	nodeID   string
	handler  messageloop.BrokerEventHandler
	options  *Options

	subMu      sync.RWMutex
	subscribed map[string]struct{}
}

// New creates a new Redis broker.
func New(cfg config.RedisConfig, nodeID string) messageloop.Broker {
	return &redisBroker{
		nodeID:    nodeID,
		options:   NewOptions(cfg),
		subscribed: make(map[string]struct{}),
	}
}

// RegisterBrokerEventHandler registers the handler for broker events.
func (b *redisBroker) RegisterBrokerEventHandler(handler messageloop.BrokerEventHandler) error {
	b.handler = handler

	// Initialize Redis client
	b.client = redis.NewClient(&redis.Options{
		Addr:         b.options.Addr,
		Password:     b.options.Password,
		DB:           b.options.DB,
		PoolSize:     b.options.PoolSize,
		MinIdleConns: b.options.MinIdleConns,
		MaxRetries:   b.options.MaxRetries,
		DialTimeout:  b.options.DialTimeout,
		ReadTimeout:  b.options.ReadTimeout,
		WriteTimeout: b.options.WriteTimeout,
	})

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	// Start consumer goroutine
	b.ctx, b.cancel = context.WithCancel(context.Background())
	b.wg.Add(1)
	go b.consume()

	return nil
}

// Subscribe subscribes the node to a channel.
func (b *redisBroker) Subscribe(ch string) error {
	b.subMu.Lock()
	defer b.subMu.Unlock()

	// Create consumer group for this stream if it doesn't exist
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()

	stream := b.options.StreamPrefix + ch
	group := b.options.ConsumerGroup

	// Try to create consumer group - ignore error if it already exists
	_ = b.client.XGroupCreate(ctx, stream, group, "0").Err()

	b.subscribed[ch] = struct{}{}
	return nil
}

// Unsubscribe unsubscribes the node from a channel.
func (b *redisBroker) Unsubscribe(ch string) error {
	b.subMu.Lock()
	defer b.subMu.Unlock()

	delete(b.subscribed, ch)
	return nil
}

// Publish publishes data to a channel.
func (b *redisBroker) Publish(ch string, data []byte, opts messageloop.PublishOptions) (messageloop.StreamPosition, bool, error) {
	msg := newPublicationMessage(ch, data)
	return b.publishToRedis(ch, msg)
}

// PublishJoin publishes a join event to a channel.
func (b *redisBroker) PublishJoin(ch string, info *messageloop.ClientDesc) error {
	msg := newJoinMessage(ch, info)
	_, _, err := b.publishToRedis(ch, msg)
	return err
}

// PublishLeave publishes a leave event to a channel.
func (b *redisBroker) PublishLeave(ch string, info *messageloop.ClientDesc) error {
	msg := newLeaveMessage(ch, info)
	_, _, err := b.publishToRedis(ch, msg)
	return err
}

// History retrieves publications from the history stream.
func (b *redisBroker) History(ch string, opts messageloop.HistoryOptions) ([]*messageloop.Publication, messageloop.StreamPosition, error) {
	return b.getHistory(ch, opts)
}

// RemoveHistory removes history for a channel.
func (b *redisBroker) RemoveHistory(ch string) error {
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()

	stream := b.options.StreamPrefix + ch
	return b.client.Del(ctx, stream).Err()
}

// Close shuts down the broker.
func (b *redisBroker) Close() error {
	b.cancel()
	b.wg.Wait()
	if b.client != nil {
		return b.client.Close()
	}
	return nil
}
