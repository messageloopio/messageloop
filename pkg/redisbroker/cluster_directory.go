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

func (d *redisSessionDirectory) sessionLeaseKey(sessionID string) string {
	return d.opts.ClusterSessionLeasePrefix + sessionID
}

func (d *redisSessionDirectory) sessionSnapshotKey(sessionID string) string {
	return d.opts.ClusterSessionSnapshotPrefix + sessionID
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

func (d *redisSessionDirectory) PutSessionLease(ctx context.Context, lease *messageloop.ClusterSessionLease, ttl time.Duration) error {
	if lease == nil || lease.SessionID == "" {
		return nil
	}
	if lease.LeaseVersion == 0 {
		lease.LeaseVersion = 1
	}
	return d.setJSON(ctx, d.sessionLeaseKey(lease.SessionID), lease, ttl)
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
		return true, nil
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
	return d.client.Del(ctx, d.sessionLeaseKey(sessionID)).Err()
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
