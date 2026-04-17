package redisbroker

import (
	"context"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/redis/go-redis/v9"
)

type redisClusterQueryStore struct {
	client        *redis.Client
	opts          *Options
	nodeID        string
	incarnationID string
}

var adjustChannelSubscriptionsScript = redis.NewScript(`
local channel = ARGV[1]
local delta = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local current = tonumber(redis.call('HGET', KEYS[1], channel) or '0')
local next = current + delta
if next <= 0 then
  redis.call('HDEL', KEYS[1], channel)
  if redis.call('HLEN', KEYS[1]) == 0 then
    redis.call('DEL', KEYS[1])
  end
  return 0
end
redis.call('HSET', KEYS[1], channel, tostring(next))
redis.call('EXPIRE', KEYS[1], ttl)
return next
`)

// NewClusterQueryStore returns a Redis-backed ClusterQueryStore.
func NewClusterQueryStore(cfg config.RedisConfig, nodeID, incarnationID string) messageloop.ClusterQueryStore {
	opts := NewOptions(cfg)
	return &redisClusterQueryStore{
		client:        newRedisClient(opts),
		opts:          opts,
		nodeID:        nodeID,
		incarnationID: incarnationID,
	}
}

func (s *redisClusterQueryStore) Start(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.client.Ping(pingCtx).Err()
}

func (s *redisClusterQueryStore) Shutdown(context.Context) error {
	return s.client.Close()
}

func (s *redisClusterQueryStore) ownerProjectionKey() string {
	return s.opts.ClusterChannelPrefix + "owner:" + s.nodeID + ":" + s.incarnationID
}

func (s *redisClusterQueryStore) AdjustChannelSubscriptions(ctx context.Context, channel string, delta int64, ttl time.Duration) error {
	if channel == "" || delta == 0 {
		return nil
	}
	ttlSeconds := int64(math.Ceil(ttl.Seconds()))
	if ttlSeconds <= 0 {
		ttlSeconds = 1
	}
	return adjustChannelSubscriptionsScript.Run(
		ctx,
		s.client,
		[]string{s.ownerProjectionKey()},
		channel,
		strconv.FormatInt(delta, 10),
		strconv.FormatInt(ttlSeconds, 10),
	).Err()
}

func (s *redisClusterQueryStore) ReplaceNodeChannels(ctx context.Context, channels map[string]int64, ttl time.Duration) error {
	key := s.ownerProjectionKey()
	if key == "" {
		return nil
	}
	pipe := s.client.TxPipeline()
	pipe.Del(ctx, key)
	if len(channels) > 0 {
		fields := make(map[string]interface{}, len(channels))
		for channel, count := range channels {
			if channel == "" || count <= 0 {
				continue
			}
			fields[channel] = count
		}
		if len(fields) > 0 {
			pipe.HSet(ctx, key, fields)
			pipe.Expire(ctx, key, ttl)
		}
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *redisClusterQueryStore) ListChannels(ctx context.Context) ([]messageloop.ClusterChannelInfo, error) {
	keys, err := scanKeys(ctx, s.client, s.opts.ClusterChannelPrefix+"owner:*")
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}

	pipe := s.client.Pipeline()
	cmds := make(map[string]*redis.MapStringStringCmd, len(keys))
	for _, key := range keys {
		cmds[key] = pipe.HGetAll(ctx, key)
	}
	_, err = pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return nil, err
	}

	aggregated := make(map[string]int64)
	for _, cmd := range cmds {
		values, cmdErr := cmd.Result()
		if cmdErr != nil {
			continue
		}
		for channel, value := range values {
			count, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr != nil || count <= 0 {
				continue
			}
			aggregated[channel] += count
		}
	}

	channels := make([]messageloop.ClusterChannelInfo, 0, len(aggregated))
	for channel, count := range aggregated {
		if count <= 0 {
			continue
		}
		channels = append(channels, messageloop.ClusterChannelInfo{Name: channel, Subscribers: count})
	}

	sort.Slice(channels, func(i, j int) bool {
		return channels[i].Name < channels[j].Name
	})
	return channels, nil
}

func scanKeys(ctx context.Context, client *redis.Client, pattern string) ([]string, error) {
	keys := make([]string, 0)
	var cursor uint64
	for {
		batch, nextCursor, err := client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, err
		}
		keys = append(keys, batch...)
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return keys, nil
}

var _ messageloop.ClusterQueryStore = (*redisClusterQueryStore)(nil)
