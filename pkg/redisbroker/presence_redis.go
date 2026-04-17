package redisbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/redis/go-redis/v9"
)

// redisPresenceStore implements messageloop.PresenceStore using one TTL key per
// (channel, client) membership plus a Redis set index per channel.
type redisPresenceStore struct {
	client *redis.Client
	opts   *Options
}

// NewPresenceStore returns a Redis-backed PresenceStore.
func NewPresenceStore(cfg config.RedisConfig) messageloop.PresenceStore {
	opts := NewOptions(cfg)
	return &redisPresenceStore{
		client: newRedisClient(opts),
		opts:   opts,
	}
}

func (s *redisPresenceStore) indexKey(ch string) string {
	return fmt.Sprintf("%sidx:%s", s.opts.PresencePrefix, ch)
}

func (s *redisPresenceStore) memberKey(ch, clientID string) string {
	return fmt.Sprintf("%smember:%s:%s", s.opts.PresencePrefix, ch, clientID)
}

// Add records or refreshes the client's presence with an independent TTL.
func (s *redisPresenceStore) Add(ctx context.Context, ch string, info *messageloop.PresenceInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.memberKey(ch, info.ClientID), data, s.opts.PresenceTTL)
	pipe.SAdd(ctx, s.indexKey(ch), info.ClientID)
	pipe.Expire(ctx, s.indexKey(ch), s.opts.PresenceTTL*2)
	_, err = pipe.Exec(ctx)
	return err
}

// Remove deletes a client's membership entry and channel index reference.
func (s *redisPresenceStore) Remove(ctx context.Context, ch, clientID string) error {
	pipe := s.client.Pipeline()
	pipe.Del(ctx, s.memberKey(ch, clientID))
	pipe.SRem(ctx, s.indexKey(ch), clientID)
	_, err := pipe.Exec(ctx)
	return err
}

// Get returns all currently present clients in ch.
func (s *redisPresenceStore) Get(ctx context.Context, ch string) (map[string]*messageloop.PresenceInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	clientIDs, err := s.client.SMembers(ctx, s.indexKey(ch)).Result()
	if err != nil {
		return nil, err
	}
	result := make(map[string]*messageloop.PresenceInfo, len(clientIDs))
	if len(clientIDs) == 0 {
		return result, nil
	}

	pipe := s.client.Pipeline()
	cmds := make(map[string]*redis.StringCmd, len(clientIDs))
	for _, clientID := range clientIDs {
		cmds[clientID] = pipe.Get(ctx, s.memberKey(ch, clientID))
	}
	_, err = pipe.Exec(ctx)
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}

	staleClientIDs := make([]string, 0)
	for clientID, cmd := range cmds {
		data, cmdErr := cmd.Result()
		if errors.Is(cmdErr, redis.Nil) {
			staleClientIDs = append(staleClientIDs, clientID)
			continue
		}
		if cmdErr != nil {
			return nil, cmdErr
		}
		var info messageloop.PresenceInfo
		if err := json.Unmarshal([]byte(data), &info); err != nil {
			staleClientIDs = append(staleClientIDs, clientID)
			continue
		}
		result[clientID] = &info
	}

	if len(staleClientIDs) > 0 {
		_ = s.client.SRem(ctx, s.indexKey(ch), staleClientIDs).Err()
	}

	return result, nil
}

var _ messageloop.PresenceStore = (*redisPresenceStore)(nil)
