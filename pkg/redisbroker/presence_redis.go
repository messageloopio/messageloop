package redisbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/messageloopio/messageloop/config"

	"github.com/messageloopio/messageloop/internal/occupancy"
)

// redisPresenceStore implements occupancy.PresenceStore using one TTL key per
// (channel, client) membership plus a Redis set index per channel.
type redisPresenceStore struct {
	client *redis.Client
	opts   *Options
	// synLeaveHook is invoked for every ghost member pruned by an existing
	// Get/refresh path whose TTL evaporated (B2 §5.3). Set via
	// SetSyntheticLeaveHook before the store is used concurrently; the
	// pruned client ID is the membership key (session ID in v1.0).
	synLeaveHook func(ctx context.Context, ch, clientID string)
}

// NewPresenceStore returns a Redis-backed PresenceStore.
func NewPresenceStore(cfg config.RedisConfig) occupancy.PresenceStore {
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

// occupancyGenKey is the cluster-wide per-channel occupancy generation
// counter (B2 §4: Redis = INCR, no random UUIDs).
func (s *redisPresenceStore) occupancyGenKey(ch string) string {
	return fmt.Sprintf("%socc:gen:%s", s.opts.PresencePrefix, ch)
}

// SetSyntheticLeaveHook registers the callback invoked for every ghost member
// pruned because its TTL key evaporated (B2 §5.3). The memory store has no
// TTL and never reports.
func (s *redisPresenceStore) SetSyntheticLeaveHook(hook func(ctx context.Context, ch, clientID string)) {
	s.synLeaveHook = hook
}

// NextOccupancyGen returns a cluster-wide strictly-increasing generation per
// channel via INCR (B2 §4). The counter is not bounded and not expired: an
// old comparison can never alias a fresh generation.
func (s *redisPresenceStore) NextOccupancyGen(ctx context.Context, ch string) (uint64, error) {
	gen, err := s.client.Incr(ctx, s.occupancyGenKey(ch)).Result()
	if err != nil {
		return 0, err
	}
	if gen <= 0 {
		return 0, fmt.Errorf("occupancy gen for %q overflowed", ch)
	}
	return uint64(gen), nil
}

// presenceRemoveScript atomically removes a member and prunes an index left
// empty. The DEL-member / SREM / SCARD / DEL-index decision must be one
// atomic script: the previous read-then-delete sequence (SCard followed by
// DEL) had a window where a concurrent Add could land between SREM and DEL,
// producing a member that is "online but invisible" in the index.
var presenceRemoveScript = redis.NewScript(`
local index = KEYS[1]
local member = KEYS[2]
local client_id = ARGV[1]
redis.call('DEL', member)
redis.call('SREM', index, client_id)
if redis.call('SCARD', index) == 0 then
  redis.call('DEL', index)
  return 1
end
return 0
`)

// Add records or refreshes the client's presence with an independent TTL.
// The index key shares the member TTL so stale indexes cannot outlive their
// members.
func (s *redisPresenceStore) Add(ctx context.Context, ch string, info *occupancy.PresenceInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.memberKey(ch, info.ClientID), data, s.opts.PresenceTTL)
	pipe.SAdd(ctx, s.indexKey(ch), info.ClientID)
	pipe.Expire(ctx, s.indexKey(ch), s.opts.PresenceTTL)
	_, err = pipe.Exec(ctx)
	return err
}

// Remove deletes a client's membership entry and channel index reference
// atomically (see presenceRemoveScript). An index left empty is removed
// entirely.
func (s *redisPresenceStore) Remove(ctx context.Context, ch, clientID string) error {
	return presenceRemoveScript.Run(
		ctx,
		s.client,
		[]string{s.indexKey(ch), s.memberKey(ch, clientID)},
		clientID,
	).Err()
}

// Get returns all currently present clients in ch.
func (s *redisPresenceStore) Get(ctx context.Context, ch string) (map[string]*occupancy.PresenceInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	clientIDs, err := s.client.SMembers(ctx, s.indexKey(ch)).Result()
	if err != nil {
		return nil, err
	}
	result := make(map[string]*occupancy.PresenceInfo, len(clientIDs))
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
		var info occupancy.PresenceInfo
		if err := json.Unmarshal([]byte(data), &info); err != nil {
			staleClientIDs = append(staleClientIDs, clientID)
			continue
		}
		result[clientID] = &info
	}

	if len(staleClientIDs) > 0 {
		_ = s.client.SRem(ctx, s.indexKey(ch), staleClientIDs).Err()
		// Ghost members whose TTL key evaporated synthesize a leave at the
		// existing pruning point (B2 §5.3): the node turns each pruned
		// membership into an occupancy leave so covered subscribers learn the
		// session vanished.
		if s.synLeaveHook != nil {
			for _, clientID := range staleClientIDs {
				s.synLeaveHook(ctx, ch, clientID)
			}
		}
	}

	return result, nil
}

var _ occupancy.PresenceStore = (*redisPresenceStore)(nil)
