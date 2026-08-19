package redisbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/internal/cluster"
	"github.com/redis/go-redis/v9"
)

type redisSessionDirectory struct {
	client *redis.Client
	opts   *Options
}

// NewSessionDirectory returns a Redis-backed SessionDirectory.
func NewSessionDirectory(cfg config.RedisConfig) cluster.SessionDirectory {
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
// (KD-K27). It deliberately sits at ml2:cluster:node_epoch:{nodeID}, NOT
// under the ml2:cluster:node: prefix, so the ListNodeLeases SCAN never picks
// it up.
func (d *redisSessionDirectory) nodeEpochKey(nodeID string) string {
	return d.opts.ClusterPrefix + "node_epoch:" + nodeID
}

// NextNodeEpoch allocates the node's next process generation with a single
// INCR; the first issue for a nodeID is 1. The decimal rendering of the
// returned epoch (cluster.FormatNodeEpoch) is the IncarnationID.
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

func (d *redisSessionDirectory) PutNodeLease(ctx context.Context, lease *cluster.ClusterNodeLease, ttl time.Duration) error {
	if lease == nil || lease.NodeID == "" || lease.IncarnationID == "" {
		return nil
	}
	return d.setJSON(ctx, d.nodeLeaseKey(lease.NodeID, lease.IncarnationID), lease, ttl)
}

func (d *redisSessionDirectory) GetNodeLease(ctx context.Context, nodeID, incarnationID string) (*cluster.ClusterNodeLease, error) {
	if nodeID == "" || incarnationID == "" {
		return nil, nil
	}
	lease := &cluster.ClusterNodeLease{}
	found, err := d.getJSON(ctx, d.nodeLeaseKey(nodeID, incarnationID), lease)
	if err != nil || !found {
		return nil, err
	}
	return lease, nil
}

func (d *redisSessionDirectory) CompareAndSwapSessionLease(ctx context.Context, expected, desired *cluster.ClusterSessionLease, ttl time.Duration) (bool, error) {
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

		var currentLease *cluster.ClusterSessionLease
		if !errors.Is(err, redis.Nil) {
			currentLease = &cluster.ClusterSessionLease{}
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

// compareAndSwapSessionStateScript performs the session lease CAS and the
// snapshot write in one atomic step (PR-KA-D10 §1.2), eliminating the
// blind-write window between a won lease CAS and the snapshot PUT that used
// to follow it.
//
// KEYS[1] = session lease key, KEYS[2] = session snapshot key.
// ARGV[1] = expected lease JSON (empty requires the lease key to be absent),
// ARGV[2] = desired lease JSON, ARGV[3] = snapshot JSON (empty skips the
// snapshot write), ARGV[4]/ARGV[5] = lease/snapshot TTL in milliseconds.
//
// The compare predicate is exactly the production four-field one
// (session_id/node_id/incarnation_id/lease_version —
// clusterSessionLeaseEqual); it is NOT a full-blob comparison, so a
// concurrent same-fence refresh that only moved LastActivityAt/TTL still
// matches. Serialization is pinned: the lease value written here is the same
// json.Marshal(ClusterSessionLease) blob the WATCH-based
// CompareAndSwapSessionLease writes, and the snapshot value is the same
// json.Marshal(ClusterSessionSnapshot) blob PutSessionSnapshot writes — key
// names, value shapes and every reader are unchanged. lease_version compares
// as a Lua number, which is exact far beyond any realistic version.
var compareAndSwapSessionStateScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if ARGV[1] == '' then
  if current then return 0 end
else
  if not current then return 0 end
  local cur = cjson.decode(current)
  local exp = cjson.decode(ARGV[1])
  if cur['session_id'] ~= exp['session_id']
     or cur['node_id'] ~= exp['node_id']
     or cur['incarnation_id'] ~= exp['incarnation_id']
     or cur['lease_version'] ~= exp['lease_version'] then
    return 0
  end
end
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[4])
if ARGV[3] ~= '' then
  redis.call('SET', KEYS[2], ARGV[3], 'PX', ARGV[5])
end
return 1
`)

// CompareAndSwapSessionState atomically CASes the session lease and writes
// the session snapshot (SessionStateCompareAndSwapper): the four-field
// compare, the lease SET and the snapshot SET run inside one Lua script, so
// a failed compare writes neither key and a won compare never leaves a stale
// snapshot behind. TTLs are unchanged (lease TTL / 24h snapshot TTL), applied
// as PX — internally the same absolute expiry the EX/PX mix of the plain Set
// calls produced.
func (d *redisSessionDirectory) CompareAndSwapSessionState(ctx context.Context, expected, desired *cluster.ClusterSessionLease, snapshot *cluster.ClusterSessionSnapshot, leaseTTL, snapshotTTL time.Duration) (bool, error) {
	if desired == nil || desired.SessionID == "" {
		return false, nil
	}

	expectedJSON := ""
	if expected != nil {
		payload, err := json.Marshal(expected)
		if err != nil {
			return false, err
		}
		expectedJSON = string(payload)
	}
	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		return false, err
	}
	snapshotJSON := ""
	if snapshot != nil && snapshot.SessionID != "" {
		payload, err := json.Marshal(snapshot)
		if err != nil {
			return false, err
		}
		snapshotJSON = string(payload)
	}

	result, err := compareAndSwapSessionStateScript.Run(ctx, d.client,
		[]string{d.sessionLeaseKey(desired.SessionID), d.sessionSnapshotKey(desired.SessionID)},
		expectedJSON, string(desiredJSON), snapshotJSON,
		ttlMilliseconds(leaseTTL), ttlMilliseconds(snapshotTTL)).Int()
	if err != nil {
		return false, err
	}
	if result != 1 {
		return false, nil
	}
	return true, d.syncUserIndex(ctx, expected, desired, leaseTTL)
}

func ttlMilliseconds(d time.Duration) int64 {
	if ms := d.Milliseconds(); ms > 0 {
		return ms
	}
	return 1
}

func (d *redisSessionDirectory) GetSessionLease(ctx context.Context, sessionID string) (*cluster.ClusterSessionLease, error) {
	if sessionID == "" {
		return nil, nil
	}
	lease := &cluster.ClusterSessionLease{}
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

func (d *redisSessionDirectory) PutSessionSnapshot(ctx context.Context, snapshot *cluster.ClusterSessionSnapshot, ttl time.Duration) error {
	if snapshot == nil || snapshot.SessionID == "" {
		return nil
	}
	return d.setJSON(ctx, d.sessionSnapshotKey(snapshot.SessionID), snapshot, ttl)
}

func (d *redisSessionDirectory) GetSessionSnapshot(ctx context.Context, sessionID string) (*cluster.ClusterSessionSnapshot, error) {
	if sessionID == "" {
		return nil, nil
	}
	snapshot := &cluster.ClusterSessionSnapshot{}
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
// ml2:cluster:session:lease:*). It feeds the periodic user-index repair and
// the membership OnLeave invalidation; a lease that vanished between the
// scan and the read is skipped.
func (d *redisSessionDirectory) ListSessionLeases(ctx context.Context) ([]*cluster.ClusterSessionLease, error) {
	blobs, err := d.listLeaseJSON(ctx, d.opts.ClusterSessionLeasePrefix)
	if err != nil {
		return nil, err
	}
	leases := make([]*cluster.ClusterSessionLease, 0, len(blobs))
	for _, raw := range blobs {
		lease := &cluster.ClusterSessionLease{}
		if unmarshalErr := json.Unmarshal(raw, lease); unmarshalErr != nil {
			continue
		}
		leases = append(leases, lease)
	}
	return leases, nil
}

// ListNodeLeases enumerates every stored node lease (SCAN
// ml2:cluster:node:*). It feeds the membership repair loop that drives
// OnLeave; a lease that vanished between the scan and the read is skipped.
func (d *redisSessionDirectory) ListNodeLeases(ctx context.Context) ([]*cluster.ClusterNodeLease, error) {
	blobs, err := d.listLeaseJSON(ctx, d.opts.ClusterNodePrefix)
	if err != nil {
		return nil, err
	}
	leases := make([]*cluster.ClusterNodeLease, 0, len(blobs))
	for _, raw := range blobs {
		lease := &cluster.ClusterNodeLease{}
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
func (d *redisSessionDirectory) syncUserIndex(ctx context.Context, oldLease, newLease *cluster.ClusterSessionLease, ttl time.Duration) error {
	if err := cluster.SyncUserIndex(ctx, d, oldLease, newLease, ttl); err != nil {
		log.WarnContext(ctx, "failed to sync user session index", err, "session_id", leaseSessionID(oldLease, newLease))
		return nil
	}
	return nil
}

func leaseSessionID(oldLease, newLease *cluster.ClusterSessionLease) string {
	if newLease != nil {
		return newLease.SessionID
	}
	if oldLease != nil {
		return oldLease.SessionID
	}
	return ""
}

func clusterSessionLeaseEqual(left, right *cluster.ClusterSessionLease) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.SessionID == right.SessionID &&
		left.NodeID == right.NodeID &&
		left.IncarnationID == right.IncarnationID &&
		left.LeaseVersion == right.LeaseVersion
}

var _ cluster.SessionDirectory = (*redisSessionDirectory)(nil)
var _ cluster.SessionStateCompareAndSwapper = (*redisSessionDirectory)(nil)
var _ cluster.ClusterSessionLeaseLister = (*redisSessionDirectory)(nil)
var _ cluster.ClusterNodeLeaseLister = (*redisSessionDirectory)(nil)
var _ cluster.NodeEpochAllocator = (*redisSessionDirectory)(nil)
