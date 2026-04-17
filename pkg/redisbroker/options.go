package redisbroker

import (
	"time"

	"github.com/messageloopio/messageloop/config"
)

const (
	defaultStreamPrefix                 = "ml:stream:"
	defaultPubSubPrefix                 = "ml:pubsub:"
	defaultPresencePrefix               = "ml:presence:"
	defaultClusterPrefix                = "ml:cluster:"
	defaultClusterNodePrefix            = "ml:cluster:node:"
	defaultClusterSessionLeasePrefix    = "ml:cluster:session:lease:"
	defaultClusterSessionSnapshotPrefix = "ml:cluster:session:snapshot:"
	defaultClusterChannelPrefix         = "ml:cluster:channel:"
	defaultStreamMaxLength              = 10000
	defaultHistoryTTL                   = 24 * time.Hour
	defaultPresenceTTL                  = 60 * time.Second
	defaultPoolSize                     = 10
	defaultMinIdleConns                 = 5
	defaultMaxRetries                   = 3
	defaultDialTimeout                  = 5 * time.Second
	defaultReadTimeout                  = 3 * time.Second
	defaultWriteTimeout                 = 3 * time.Second
)

// Options contains the configuration for the Redis broker.
type Options struct {
	Addr                         string
	Password                     string
	DB                           int
	PoolSize                     int
	MinIdleConns                 int
	MaxRetries                   int
	DialTimeout                  time.Duration
	ReadTimeout                  time.Duration
	WriteTimeout                 time.Duration
	StreamMaxLength              int64
	StreamApproximate            bool
	HistoryTTL                   time.Duration
	PresenceTTL                  time.Duration
	StreamPrefix                 string
	PubSubPrefix                 string
	PresencePrefix               string
	ClusterPrefix                string
	ClusterNodePrefix            string
	ClusterSessionLeasePrefix    string
	ClusterSessionSnapshotPrefix string
	ClusterChannelPrefix         string
}

// NewOptions creates Options from config.RedisConfig.
func NewOptions(cfg config.RedisConfig) *Options {
	opts := &Options{
		Addr:                         cfg.Addr,
		Password:                     cfg.Password,
		DB:                           cfg.DB,
		PoolSize:                     defaultPoolSize,
		MinIdleConns:                 defaultMinIdleConns,
		MaxRetries:                   defaultMaxRetries,
		DialTimeout:                  defaultDialTimeout,
		ReadTimeout:                  defaultReadTimeout,
		WriteTimeout:                 defaultWriteTimeout,
		StreamMaxLength:              defaultStreamMaxLength,
		StreamApproximate:            true,
		HistoryTTL:                   defaultHistoryTTL,
		PresenceTTL:                  defaultPresenceTTL,
		StreamPrefix:                 defaultStreamPrefix,
		PubSubPrefix:                 defaultPubSubPrefix,
		PresencePrefix:               defaultPresencePrefix,
		ClusterPrefix:                defaultClusterPrefix,
		ClusterNodePrefix:            defaultClusterNodePrefix,
		ClusterSessionLeasePrefix:    defaultClusterSessionLeasePrefix,
		ClusterSessionSnapshotPrefix: defaultClusterSessionSnapshotPrefix,
		ClusterChannelPrefix:         defaultClusterChannelPrefix,
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

	return opts
}
