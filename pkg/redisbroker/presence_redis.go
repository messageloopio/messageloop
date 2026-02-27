package redisbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/redis/go-redis/v9"
)

// redisPresenceStore implements messageloop.PresenceStore using Redis hashes.
// Each channel's presence is stored as a HASH at ml:presence:{ch},
// where each field is a clientID and the value is JSON-encoded PresenceInfo.
// The hash key carries a TTL that is refreshed on every Add call, so stale
// entries (from crashed clients) expire automatically.
type redisPresenceStore struct {
	client *redis.Client
	opts   *Options
}

// NewPresenceStore returns a Redis-backed PresenceStore.
func NewPresenceStore(cfg config.RedisConfig) messageloop.PresenceStore {
	opts := NewOptions(cfg)
	return &redisPresenceStore{
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
		opts: opts,
	}
}

func (s *redisPresenceStore) key(ch string) string {
	return fmt.Sprintf("%s%s", s.opts.PresencePrefix, ch)
}

// Add records or refreshes the client's presence in ch and resets the key TTL.
func (s *redisPresenceStore) Add(ctx context.Context, ch string, info *messageloop.PresenceInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	key := s.key(ch)
	pipe := s.client.Pipeline()
	pipe.HSet(ctx, key, info.ClientID, data)
	pipe.Expire(ctx, key, s.opts.PresenceTTL)
	_, err = pipe.Exec(ctx)
	return err
}

// Remove deletes a client's entry from ch's presence hash.
func (s *redisPresenceStore) Remove(ctx context.Context, ch, clientID string) error {
	return s.client.HDel(ctx, s.key(ch), clientID).Err()
}

// Get returns all currently present clients in ch.
func (s *redisPresenceStore) Get(ctx context.Context, ch string) (map[string]*messageloop.PresenceInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	raw, err := s.client.HGetAll(ctx, s.key(ch)).Result()
	if err != nil {
		return nil, err
	}
	result := make(map[string]*messageloop.PresenceInfo, len(raw))
	for clientID, data := range raw {
		var info messageloop.PresenceInfo
		if err := json.Unmarshal([]byte(data), &info); err != nil {
			continue
		}
		result[clientID] = &info
	}
	return result, nil
}

var _ messageloop.PresenceStore = (*redisPresenceStore)(nil)
