package redisbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/redis/go-redis/v9"
)

type redisSessionDirectory struct {
	client *redis.Client
	opts   *Options
}

// NewSessionDirectory returns a Redis-backed SessionDirectory.
func NewSessionDirectory(cfg config.RedisConfig) messageloop.SessionDirectory {
	opts := NewOptions(cfg)
	return &redisSessionDirectory{
		client: newRedisClient(opts),
		opts:   opts,
	}
}

func (d *redisSessionDirectory) Start(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return d.client.Ping(pingCtx).Err()
}

func (d *redisSessionDirectory) Shutdown(context.Context) error {
	return d.client.Close()
}

func (d *redisSessionDirectory) nodeLeaseKey(nodeID, incarnationID string) string {
	return fmt.Sprintf("%s%s:%s", d.opts.ClusterNodePrefix, nodeID, incarnationID)
}

// nodeEpochKey is the monotone counter key for a node's process generations
// (KD-K27). It deliberately sits at ml:cluster:node_epoch:{nodeID}, NOT
// under the ml:cluster:node: prefix, so the ListNodeLeases SCAN never picks
// it up.
func (d *redisSessionDirectory) nodeEpochKey(nodeID string) string {
	return d.opts.ClusterPrefix + "node_epoch:" + nodeID
}

// NextNodeEpoch allocates the node's next process generation with a single
// INCR; the first issue for a nodeID is 1. The decimal rendering of the
// returned epoch (messageloop.FormatNodeEpoch) is the IncarnationID.
func (d *redisSessionDirectory) NextNodeEpoch(ctx context.Context, nodeID string) (uint64, error) {
	if nodeID == "" {
		return 0, errors.New("node_epoch: node_id is required")
	}
	epoch, err := d.client.Incr(ctx, d.nodeEpochKey(nodeID)).Uint64()
	if err != nil {
		return 0, fmt.Errorf("node_epoch INCR for node %s: %w", nodeID, err)
	}
	return epoch, nil
}

func (d *redisSessionDirectory) sessionLeaseKey(sessionID string) string {
	return d.opts.ClusterSessionLeasePrefix + sessionID
}

func (d *redisSessionDirectory) sessionSnapshotKey(sessionID string) string {
	return d.opts.ClusterSessionSnapshotPrefix + sessionID
}

// userMemberKey is the per-session member key of the user→sessions index. It
// carries the same TTL as the session lease so the member expires together
// with the lease; the set index (userSessionsKey) is trimmed by
// RemoveUserSession and rebuilt by the periodic repair.
func (d *redisSessionDirectory) userMemberKey(userID, sessionID string) string {
	return fmt.Sprintf("%suser:member:%s:%s", d.opts.ClusterPrefix, userID, sessionID)
}

// userSessionsKey is the set of session IDs currently indexed for userID.
func (d *redisSessionDirectory) userSessionsKey(userID string) string {
	return fmt.Sprintf("%suser:sessions:%s", d.opts.ClusterPrefix, userID)
}

func (d *redisSessionDirectory) PutNodeLease(ctx context.Context, lease *messageloop.ClusterNodeLease, ttl time.Duration) error {
	if lease == nil || lease.NodeID == "" || lease.IncarnationID == "" {
		return nil
	}
	return d.setJSON(ctx, d.nodeLeaseKey(lease.NodeID, lease.IncarnationID), lease, ttl)
}

func (d *redisSessionDirectory) GetNodeLease(ctx context.Context, nodeID, incarnationID string) (*messageloop.ClusterNodeLease, error) {
	if nodeID == "" || incarnationID == "" {
		return nil, nil
	}
	lease := &messageloop.ClusterNodeLease{}
	found, err := d.getJSON(ctx, d.nodeLeaseKey(nodeID, incarnationID), lease)
	if err != nil || !found {
		return nil, err
	}
	return lease, nil
}

func (d *redisSessionDirectory) CompareAndSwapSessionLease(ctx context.Context, expected, desired *messageloop.ClusterSessionLease, ttl time.Duration) (bool, error) {
	if desired == nil || desired.SessionID == "" {
		return false, nil
	}

	const compareMismatch = "cluster lease compare mismatch"
	key := d.sessionLeaseKey(desired.SessionID)
	err := d.client.Watch(ctx, func(tx *redis.Tx) error {
		current, err := tx.Get(ctx, key).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return err
		}

		var currentLease *messageloop.ClusterSessionLease
		if !errors.Is(err, redis.Nil) {
			currentLease = &messageloop.ClusterSessionLease{}
			if unmarshalErr := json.Unmarshal([]byte(current), currentLease); unmarshalErr != nil {
				return unmarshalErr
			}
		}

		if !clusterSessionLeaseEqual(currentLease, expected) {
			return errors.New(compareMismatch)
		}

		payload, marshalErr := json.Marshal(desired)
		if marshalErr != nil {
			return marshalErr
		}

		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, key, payload, ttl)
			return nil
		})
		return err
	}, key)
	if err == nil {
		return true, d.syncUserIndex(ctx, expected, desired, ttl)
	}
	if err.Error() == compareMismatch || errors.Is(err, redis.TxFailedErr) {
		return false, nil
	}
	return false, err
}

func (d *redisSessionDirectory) GetSessionLease(ctx context.Context, sessionID string) (*messageloop.ClusterSessionLease, error) {
	if sessionID == "" {
		return nil, nil
	}
	lease := &messageloop.ClusterSessionLease{}
	found, err := d.getJSON(ctx, d.sessionLeaseKey(sessionID), lease)
	if err != nil || !found {
		return nil, err
	}
	return lease, nil
}

func (d *redisSessionDirectory) DeleteSessionLease(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	// Read the user before deleting the lease so the user index membership
	// can be removed together with the lease.
	lease, err := d.GetSessionLease(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := d.client.Del(ctx, d.sessionLeaseKey(sessionID)).Err(); err != nil {
		return err
	}
	return d.syncUserIndex(ctx, lease, nil, 0)
}

func (d *redisSessionDirectory) PutSessionSnapshot(ctx context.Context, snapshot *messageloop.ClusterSessionSnapshot, ttl time.Duration) error {
	if snapshot == nil || snapshot.SessionID == "" {
		return nil
	}
	return d.setJSON(ctx, d.sessionSnapshotKey(snapshot.SessionID), snapshot, ttl)
}

func (d *redisSessionDirectory) GetSessionSnapshot(ctx context.Context, sessionID string) (*messageloop.ClusterSessionSnapshot, error) {
	if sessionID == "" {
		return nil, nil
	}
	snapshot := &messageloop.ClusterSessionSnapshot{}
	found, err := d.getJSON(ctx, d.sessionSnapshotKey(sessionID), snapshot)
	if err != nil || !found {
		return nil, err
	}
	return snapshot, nil
}

func (d *redisSessionDirectory) DeleteSessionSnapshot(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	return d.client.Del(ctx, d.sessionSnapshotKey(sessionID)).Err()
}

func (d *redisSessionDirectory) setJSON(ctx context.Context, key string, value any, ttl time.Duration) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return d.client.Set(ctx, key, payload, ttl).Err()
}

func (d *redisSessionDirectory) getJSON(ctx context.Context, key string, target any) (bool, error) {
	data, err := d.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(data), target); err != nil {
		return false, err
	}
	return true, nil
}

// AddUserSession records a session's membership in a user's index: a
// per-session member key with the session lease TTL plus a set member. The
// set itself has no TTL; stale members expire with their member keys and are
// filtered at expansion time.
func (d *redisSessionDirectory) AddUserSession(ctx context.Context, userID, sessionID string, ttl time.Duration) error {
	if userID == "" || sessionID == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = time.Second
	}
	pipe := d.client.TxPipeline()
	pipe.Set(ctx, d.userMemberKey(userID, sessionID), "1", ttl)
	pipe.SAdd(ctx, d.userSessionsKey(userID), sessionID)
	_, err := pipe.Exec(ctx)
	return err
}

func (d *redisSessionDirectory) RemoveUserSession(ctx context.Context, userID, sessionID string) error {
	if userID == "" || sessionID == "" {
		return nil
	}
	pipe := d.client.TxPipeline()
	pipe.Del(ctx, d.userMemberKey(userID, sessionID))
	pipe.SRem(ctx, d.userSessionsKey(userID), sessionID)
	_, err := pipe.Exec(ctx)
	return err
}

func (d *redisSessionDirectory) ListUserSessions(ctx context.Context, userID string) ([]string, error) {
	if userID == "" {
		return nil, nil
	}
	return d.client.SMembers(ctx, d.userSessionsKey(userID)).Result()
}

// ListSessionLeases enumerates every stored session lease (SCAN
// ml:cluster:session:lease:*). It feeds the periodic user-index repair and
// the membership OnLeave invalidation; a lease that vanished between the
// scan and the read is skipped.
func (d *redisSessionDirectory) ListSessionLeases(ctx context.Context) ([]*messageloop.ClusterSessionLease, error) {
	blobs, err := d.listLeaseJSON(ctx, d.opts.ClusterSessionLeasePrefix)
	if err != nil {
		return nil, err
	}
	leases := make([]*messageloop.ClusterSessionLease, 0, len(blobs))
	for _, raw := range blobs {
		lease := &messageloop.ClusterSessionLease{}
		if unmarshalErr := json.Unmarshal(raw, lease); unmarshalErr != nil {
			continue
		}
		leases = append(leases, lease)
	}
	return leases, nil
}

// ListNodeLeases enumerates every stored node lease (SCAN
// ml:cluster:node:*). It feeds the membership repair loop that drives
// OnLeave; a lease that vanished between the scan and the read is skipped.
func (d *redisSessionDirectory) ListNodeLeases(ctx context.Context) ([]*messageloop.ClusterNodeLease, error) {
	blobs, err := d.listLeaseJSON(ctx, d.opts.ClusterNodePrefix)
	if err != nil {
		return nil, err
	}
	leases := make([]*messageloop.ClusterNodeLease, 0, len(blobs))
	for _, raw := range blobs {
		lease := &messageloop.ClusterNodeLease{}
		if unmarshalErr := json.Unmarshal(raw, lease); unmarshalErr != nil {
			continue
		}
		leases = append(leases, lease)
	}
	return leases, nil
}

// listLeaseJSON SCANs a lease prefix and pipeline-reads every key, returning
// the raw JSON blobs of the keys that still exist.
func (d *redisSessionDirectory) listLeaseJSON(ctx context.Context, prefix string) ([][]byte, error) {
	keys, err := scanKeys(ctx, d.client, prefix+"*")
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}

	pipe := d.client.Pipeline()
	cmds := make(map[string]*redis.StringCmd, len(keys))
	for _, key := range keys {
		cmds[key] = pipe.Get(ctx, key)
	}
	_, err = pipe.Exec(ctx)
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}

	blobs := make([][]byte, 0, len(keys))
	for _, key := range keys {
		cmd, ok := cmds[key]
		if !ok {
			continue
		}
		raw, cmdErr := cmd.Result()
		if cmdErr != nil {
			continue
		}
		blobs = append(blobs, []byte(raw))
	}
	return blobs, nil
}

// syncUserIndex maintains the user→sessions index for one lease write. The
// lease itself has already been written or deleted; index maintenance is
// best-effort — a failure only warns, because the index is a hint (never
// authoritative) and the periodic repair converges stale entries.
func (d *redisSessionDirectory) syncUserIndex(ctx context.Context, oldLease, newLease *messageloop.ClusterSessionLease, ttl time.Duration) error {
	if err := messageloop.SyncUserIndex(ctx, d, oldLease, newLease, ttl); err != nil {
		log.WarnContext(ctx, "failed to sync user session index", err, "session_id", leaseSessionID(oldLease, newLease))
		return nil
	}
	return nil
}

func leaseSessionID(oldLease, newLease *messageloop.ClusterSessionLease) string {
	if newLease != nil {
		return newLease.SessionID
	}
	if oldLease != nil {
		return oldLease.SessionID
	}
	return ""
}

func clusterSessionLeaseEqual(left, right *messageloop.ClusterSessionLease) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.SessionID == right.SessionID &&
		left.NodeID == right.NodeID &&
		left.IncarnationID == right.IncarnationID &&
		left.LeaseVersion == right.LeaseVersion
}

var _ messageloop.SessionDirectory = (*redisSessionDirectory)(nil)
var _ messageloop.ClusterSessionLeaseLister = (*redisSessionDirectory)(nil)
var _ messageloop.ClusterNodeLeaseLister = (*redisSessionDirectory)(nil)
var _ messageloop.NodeEpochAllocator = (*redisSessionDirectory)(nil)
