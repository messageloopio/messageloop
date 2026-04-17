package redisbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/redis/go-redis/v9"
)

const (
	clusterCommandRequestPrefix = "ml:cluster:cmd:req:"
	clusterCommandReplyPrefix   = "ml:cluster:cmd:reply:"
	clusterCommandReplyKey      = "reply_channel"
	defaultCommandTimeout       = 5 * time.Second
)

type redisClusterCommandBus struct {
	client        *redis.Client
	opts          *Options
	nodeID        string
	incarnationID string

	mu      sync.RWMutex
	handler messageloop.ClusterCommandHandler
	pubsub  *redis.PubSub
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	start   bool
	stop    bool
}

// NewClusterCommandBus returns a Redis-backed request/reply ClusterCommandBus.
func NewClusterCommandBus(cfg config.RedisConfig, nodeID, incarnationID string) messageloop.ClusterCommandBus {
	opts := NewOptions(cfg)
	return &redisClusterCommandBus{
		client:        newRedisClient(opts),
		opts:          opts,
		nodeID:        nodeID,
		incarnationID: incarnationID,
	}
}

func (b *redisClusterCommandBus) SetHandler(handler messageloop.ClusterCommandHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handler = handler
}

func (b *redisClusterCommandBus) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.start {
		return nil
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := b.client.Ping(pingCtx).Err(); err != nil {
		return err
	}

	busCtx, busCancel := context.WithCancel(ctx)
	pubsub := b.client.Subscribe(busCtx, b.requestChannel(b.nodeID, b.incarnationID))
	if _, err := pubsub.Receive(busCtx); err != nil {
		busCancel()
		_ = pubsub.Close()
		return err
	}

	b.cancel = busCancel
	b.pubsub = pubsub
	b.start = true
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for message := range pubsub.Channel() {
			b.handleMessage(busCtx, message.Payload)
		}
	}()

	return nil
}

func (b *redisClusterCommandBus) Shutdown(context.Context) error {
	b.mu.Lock()
	if b.stop {
		b.mu.Unlock()
		return nil
	}
	b.stop = true
	if b.cancel != nil {
		b.cancel()
	}
	pubsub := b.pubsub
	b.mu.Unlock()

	if pubsub != nil {
		_ = pubsub.Close()
	}
	b.wg.Wait()
	return b.client.Close()
}

func (b *redisClusterCommandBus) SendCommand(ctx context.Context, cmd *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
	if cmd == nil || cmd.TargetNodeID == "" || cmd.TargetIncarnationID == "" {
		return nil, nil
	}

	commandCtx, cancel := ensureCommandTimeout(ctx)
	defer cancel()

	if cmd.CommandID == "" {
		cmd.CommandID = uuid.NewString()
	}
	cmd.IssuedAt = time.Now()
	if cmd.Metadata == nil {
		cmd.Metadata = make(map[string]string)
	}
	replyChannel := b.replyChannel(cmd.CommandID)
	cmd.Metadata[clusterCommandReplyKey] = replyChannel

	pubsub := b.client.Subscribe(commandCtx, replyChannel)
	defer pubsub.Close()
	if _, err := pubsub.Receive(commandCtx); err != nil {
		return nil, err
	}

	payload, err := json.Marshal(cmd)
	if err != nil {
		return nil, err
	}
	if err := b.client.Publish(commandCtx, b.requestChannel(cmd.TargetNodeID, cmd.TargetIncarnationID), payload).Err(); err != nil {
		return nil, err
	}

	select {
	case <-commandCtx.Done():
		return nil, commandCtx.Err()
	case reply, ok := <-pubsub.Channel():
		if !ok {
			return nil, fmt.Errorf("cluster command reply channel closed")
		}
		result := &messageloop.ClusterCommandResult{}
		if err := json.Unmarshal([]byte(reply.Payload), result); err != nil {
			return nil, err
		}
		return result, nil
	}
}

func (b *redisClusterCommandBus) BroadcastCommand(ctx context.Context, cmd *messageloop.ClusterCommand) ([]*messageloop.ClusterCommandResult, error) {
	if cmd == nil {
		return nil, nil
	}
	if cmd.TargetNodeID != "" && cmd.TargetIncarnationID != "" {
		result, err := b.SendCommand(ctx, cmd)
		if result == nil || err != nil {
			return nil, err
		}
		return []*messageloop.ClusterCommandResult{result}, nil
	}

	keys, err := scanKeys(ctx, b.client, b.opts.ClusterNodePrefix+"*")
	if err != nil {
		return nil, err
	}

	results := make([]*messageloop.ClusterCommandResult, 0, len(keys))
	for _, key := range keys {
		payload, getErr := b.client.Get(ctx, key).Result()
		if getErr != nil {
			continue
		}
		lease := &messageloop.ClusterNodeLease{}
		if err := json.Unmarshal([]byte(payload), lease); err != nil {
			continue
		}
		copyCommand := *cmd
		copyCommand.CommandID = uuid.NewString()
		copyCommand.TargetNodeID = lease.NodeID
		copyCommand.TargetIncarnationID = lease.IncarnationID
		result, sendErr := b.SendCommand(ctx, &copyCommand)
		if sendErr != nil {
			return results, sendErr
		}
		if result != nil {
			results = append(results, result)
		}
	}
	return results, nil
}

func (b *redisClusterCommandBus) requestChannel(nodeID, incarnationID string) string {
	return clusterCommandRequestPrefix + nodeID + ":" + incarnationID
}

func (b *redisClusterCommandBus) replyChannel(commandID string) string {
	return clusterCommandReplyPrefix + commandID
}

func (b *redisClusterCommandBus) handleMessage(ctx context.Context, payload string) {
	command := &messageloop.ClusterCommand{}
	if err := json.Unmarshal([]byte(payload), command); err != nil {
		return
	}

	result := &messageloop.ClusterCommandResult{
		CommandID:     command.CommandID,
		SessionID:     command.SessionID,
		NodeID:        b.nodeID,
		IncarnationID: b.incarnationID,
		Status:        messageloop.ClusterCommandStatusFailed,
		ErrorCode:     "CLUSTER_COMMAND_HANDLER_NOT_CONFIGURED",
		ErrorMessage:  "cluster command handler is not configured",
	}

	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler != nil {
		handledResult, err := handler(ctx, command)
		if err != nil {
			result.ErrorCode = "CLUSTER_COMMAND_HANDLER_FAILED"
			result.ErrorMessage = err.Error()
		} else if handledResult != nil {
			result = handledResult
		}
	}

	replyChannel := command.Metadata[clusterCommandReplyKey]
	if replyChannel == "" {
		return
	}
	encodedResult, err := json.Marshal(result)
	if err != nil {
		return
	}
	_ = b.client.Publish(ctx, replyChannel, encodedResult).Err()
}

func ensureCommandTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, defaultCommandTimeout)
}

var _ messageloop.ClusterCommandBus = (*redisClusterCommandBus)(nil)
