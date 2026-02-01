package redisbroker

import (
	"time"

	"github.com/deeplooplabs/messageloop/config"
)

const (
	defaultStreamPrefix    = "ml:stream:"
	defaultPubSubPrefix    = "ml:pubsub:"
	defaultConsumerGroup   = "messageloop"
	defaultStreamMaxLength = 10000
	defaultHistoryTTL      = 24 * time.Hour
	defaultPoolSize        = 10
	defaultMinIdleConns    = 5
	defaultMaxRetries      = 3
	defaultDialTimeout     = 5 * time.Second
	defaultReadTimeout     = 3 * time.Second
	defaultWriteTimeout    = 3 * time.Second
)

// Options contains the configuration for the Redis broker.
type Options struct {
	// Addr is the Redis server address (e.g., "localhost:6379")
	Addr string
	// Password for Redis authentication
	Password string
	// DB is the Redis database number to use
	DB int
	// PoolSize is the maximum number of socket connections
	PoolSize int
	// MinIdleConns is the minimum number of idle connections
	MinIdleConns int
	// MaxRetries is the maximum number of retries before giving up
	MaxRetries int
	// DialTimeout is the timeout for connecting to Redis
	DialTimeout time.Duration
	// ReadTimeout is the timeout for reading operations
	ReadTimeout time.Duration
	// WriteTimeout is the timeout for write operations
	WriteTimeout time.Duration
	// StreamMaxLength is the maximum length of Redis streams
	StreamMaxLength int64
	// StreamApproximate enables approximate stream trimming
	StreamApproximate bool
	// HistoryTTL is the time-to-live for stream history
	HistoryTTL time.Duration
	// ConsumerGroup is the name of the Redis consumer group
	ConsumerGroup string
	// StreamPrefix is the prefix for stream keys
	StreamPrefix string
	// PubSubPrefix is the prefix for pub/sub channels
	PubSubPrefix string
}

// NewOptions creates Options from config.RedisConfig.
func NewOptions(cfg config.RedisConfig) *Options {
	opts := &Options{
		Addr:              cfg.Addr,
		Password:          cfg.Password,
		DB:                cfg.DB,
		PoolSize:          defaultPoolSize,
		MinIdleConns:      defaultMinIdleConns,
		MaxRetries:        defaultMaxRetries,
		DialTimeout:       defaultDialTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      defaultWriteTimeout,
		StreamMaxLength:   defaultStreamMaxLength,
		StreamApproximate: true,
		HistoryTTL:        defaultHistoryTTL,
		ConsumerGroup:     defaultConsumerGroup,
		StreamPrefix:      defaultStreamPrefix,
		PubSubPrefix:      defaultPubSubPrefix,
	}

	if cfg.PoolSize > 0 {
		opts.PoolSize = cfg.PoolSize
	}
	if cfg.MinIdleConns > 0 {
		opts.MinIdleConns = cfg.MinIdleConns
	}
	if cfg.MaxRetries > 0 {
		opts.MaxRetries = cfg.MaxRetries
	}
	if cfg.DialTimeout != "" {
		if d, err := time.ParseDuration(cfg.DialTimeout); err == nil {
			opts.DialTimeout = d
		}
	}
	if cfg.ReadTimeout != "" {
		if d, err := time.ParseDuration(cfg.ReadTimeout); err == nil {
			opts.ReadTimeout = d
		}
	}
	if cfg.WriteTimeout != "" {
		if d, err := time.ParseDuration(cfg.WriteTimeout); err == nil {
			opts.WriteTimeout = d
		}
	}
	if cfg.StreamMaxLength > 0 {
		opts.StreamMaxLength = cfg.StreamMaxLength
	}
	if cfg.StreamApproximate {
		opts.StreamApproximate = cfg.StreamApproximate
	}
	if cfg.HistoryTTL != "" {
		if d, err := time.ParseDuration(cfg.HistoryTTL); err == nil {
			opts.HistoryTTL = d
		}
	}
	if cfg.ConsumerGroup != "" {
		opts.ConsumerGroup = cfg.ConsumerGroup
	}

	return opts
}
