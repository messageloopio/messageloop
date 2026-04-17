package redisbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/redis/go-redis/v9"
)

const (
	clusterCommandRequestPrefix = "ml:cluster:cmd:req:"
	clusterCommandReplyPrefix   = "ml:cluster:cmd:reply:"
	clusterCommandStatePrefix   = "ml:cluster:cmd:state:"
	clusterCommandReplyKey      = "reply_channel"
	defaultCommandTimeout       = 5 * time.Second
	defaultCommandStateTTL      = 10 * time.Minute
)

type redisClusterCommandBus struct {
	client        *redis.Client
	opts          *Options
	nodeID        string
	incarnationID string

	mu        sync.RWMutex
	handler   messageloop.ClusterCommandHandler
	pubsub    *redis.PubSub
	cancel    context.CancelFunc
	readerWG  sync.WaitGroup
	handlerWG sync.WaitGroup
	metrics   *messageloop.Metrics
	start     bool
	stop      bool
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

func (b *redisClusterCommandBus) SetMetrics(metrics *messageloop.Metrics) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.metrics = metrics
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
	b.readerWG.Add(1)
	go func() {
		defer b.readerWG.Done()
		for message := range pubsub.Channel() {
			b.handlerWG.Add(1)
			go func(payload string) {
				defer b.handlerWG.Done()
				b.handleMessage(busCtx, payload)
			}(message.Payload)
		}
	}()

	return nil
}

func (b *redisClusterCommandBus) Shutdown(ctx context.Context) error {
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
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		b.readerWG.Wait()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-readerDone:
	}
	handlersDone := make(chan struct{})
	go func() {
		defer close(handlersDone)
		b.handlerWG.Wait()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-handlersDone:
	}
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
	if resolvedResult, err := b.resolveExistingCommand(commandCtx, cmd.CommandID); err != nil {
		return nil, err
	} else if resolvedResult != nil {
		b.recordDedupeHit(commandCtx, cmd, "send")
		return resolvedResult, nil
	}
	replyChannel := b.replyChannel(uuid.NewString())
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
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			b.recordCommandTimeout(commandCtx, cmd)
		}
		return b.resolveTimedOutCommand(commandCtx, cmd)
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
	type broadcastOutcome struct {
		result *messageloop.ClusterCommandResult
	}
	outcomes := make(chan broadcastOutcome, len(keys))
	var wg sync.WaitGroup
	for _, key := range keys {
		payload, getErr := b.client.Get(ctx, key).Result()
		if getErr != nil {
			continue
		}
		lease := &messageloop.ClusterNodeLease{}
		if err := json.Unmarshal([]byte(payload), lease); err != nil {
			continue
		}
		if cmd.Metadata["exclude_self"] == "true" && lease.NodeID == b.nodeID && lease.IncarnationID == b.incarnationID {
			continue
		}
		wg.Add(1)
		go func(lease *messageloop.ClusterNodeLease) {
			defer wg.Done()
			copyCommand := *cmd
			copyCommand.CommandID = uuid.NewString()
			copyCommand.TargetNodeID = lease.NodeID
			copyCommand.TargetIncarnationID = lease.IncarnationID
			result, sendErr := b.SendCommand(ctx, &copyCommand)
			if sendErr != nil {
				outcomes <- broadcastOutcome{result: &messageloop.ClusterCommandResult{
					CommandID:     copyCommand.CommandID,
					SessionID:     copyCommand.SessionID,
					NodeID:        lease.NodeID,
					IncarnationID: lease.IncarnationID,
					Status:        messageloop.ClusterCommandStatusFailed,
					ErrorCode:     "CLUSTER_COMMAND_SEND_FAILED",
					ErrorMessage:  sendErr.Error(),
				}}
				return
			}
			outcomes <- broadcastOutcome{result: result}
		}(lease)
	}
	go func() {
		wg.Wait()
		close(outcomes)
	}()
	for outcome := range outcomes {
		if outcome.result != nil {
			results = append(results, outcome.result)
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].NodeID != results[j].NodeID {
			return results[i].NodeID < results[j].NodeID
		}
		return results[i].IncarnationID < results[j].IncarnationID
	})
	return results, nil
}

func (b *redisClusterCommandBus) requestChannel(nodeID, incarnationID string) string {
	return clusterCommandRequestPrefix + nodeID + ":" + incarnationID
}

func (b *redisClusterCommandBus) replyChannel(commandID string) string {
	return clusterCommandReplyPrefix + commandID
}

func (b *redisClusterCommandBus) commandStateKey(commandID string) string {
	return clusterCommandStatePrefix + commandID
}

func (b *redisClusterCommandBus) handleMessage(ctx context.Context, payload string) {
	command := &messageloop.ClusterCommand{}
	if err := json.Unmarshal([]byte(payload), command); err != nil {
		return
	}
	if command.CommandID == "" {
		command.CommandID = uuid.NewString()
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

	claimed, storedResult, err := b.claimCommandExecution(ctx, command)
	if err != nil {
		result.ErrorCode = "CLUSTER_COMMAND_DEDUPE_FAILED"
		result.ErrorMessage = err.Error()
		b.publishCommandResult(ctx, command, result)
		return
	}
	if !claimed {
		b.recordDedupeHit(ctx, command, "owner")
		result = storedResult
		if result == nil {
			result = &messageloop.ClusterCommandResult{
				CommandID:     command.CommandID,
				SessionID:     command.SessionID,
				NodeID:        b.nodeID,
				IncarnationID: b.incarnationID,
				Status:        messageloop.ClusterCommandStatusInProgress,
				ErrorCode:     "COMMAND_IN_PROGRESS",
				ErrorMessage:  "cluster command is already in progress",
			}
		}
		if result.Status == messageloop.ClusterCommandStatusPending {
			result = cloneClusterCommandResult(result)
			result.Status = messageloop.ClusterCommandStatusInProgress
			result.ErrorCode = "COMMAND_IN_PROGRESS"
			result.ErrorMessage = "cluster command is already in progress"
		}
		b.publishCommandResult(ctx, command, result)
		return
	}

	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler != nil {
		handledResult, err := b.executeHandler(ctx, handler, command)
		if err != nil {
			result.Status = messageloop.ClusterCommandStatusFailed
			result.ErrorCode = "CLUSTER_COMMAND_HANDLER_FAILED"
			result.ErrorMessage = err.Error()
		} else if handledResult != nil {
			result = handledResult
		}
	}
	if result.CommandID == "" {
		result.CommandID = command.CommandID
	}
	if result.SessionID == "" {
		result.SessionID = command.SessionID
	}
	if result.NodeID == "" {
		result.NodeID = b.nodeID
	}
	if result.IncarnationID == "" {
		result.IncarnationID = b.incarnationID
	}
	if result.Status == "" || result.Status == messageloop.ClusterCommandStatusPending {
		result.Status = messageloop.ClusterCommandStatusSucceeded
	}
	if storeErr := b.storeCommandResult(ctx, result); storeErr != nil {
		result = cloneClusterCommandResult(result)
		result.Status = messageloop.ClusterCommandStatusUnknownFinalState
		result.ErrorCode = "UNKNOWN_FINAL_STATE"
		result.ErrorMessage = fmt.Sprintf("cluster command completed but terminal result could not be persisted: %v", storeErr)
		b.recordUnknownFinalState(ctx, command, result.ErrorMessage)
	}
	b.publishCommandResult(ctx, command, result)
}

func ensureCommandTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, defaultCommandTimeout)
}

func (b *redisClusterCommandBus) resolveExistingCommand(ctx context.Context, commandID string) (*messageloop.ClusterCommandResult, error) {
	if commandID == "" {
		return nil, nil
	}
	storedResult, err := b.loadCommandResult(ctx, commandID)
	if err != nil || storedResult == nil {
		return storedResult, err
	}
	if storedResult.Status != messageloop.ClusterCommandStatusPending {
		return storedResult, nil
	}
	resolved := cloneClusterCommandResult(storedResult)
	resolved.Status = messageloop.ClusterCommandStatusInProgress
	resolved.ErrorCode = "COMMAND_IN_PROGRESS"
	resolved.ErrorMessage = "cluster command is already in progress"
	return resolved, nil
}

func (b *redisClusterCommandBus) claimCommandExecution(ctx context.Context, command *messageloop.ClusterCommand) (bool, *messageloop.ClusterCommandResult, error) {
	if command == nil || command.CommandID == "" {
		return false, nil, nil
	}
	pendingResult := &messageloop.ClusterCommandResult{
		CommandID:     command.CommandID,
		SessionID:     command.SessionID,
		NodeID:        b.nodeID,
		IncarnationID: b.incarnationID,
		Status:        messageloop.ClusterCommandStatusPending,
	}
	encodedPending, err := json.Marshal(pendingResult)
	if err != nil {
		return false, nil, err
	}
	claimed, err := b.client.SetNX(ctx, b.commandStateKey(command.CommandID), encodedPending, defaultCommandStateTTL).Result()
	if err != nil {
		return false, nil, err
	}
	if claimed {
		return true, pendingResult, nil
	}
	storedResult, err := b.loadCommandResult(ctx, command.CommandID)
	if err != nil {
		return false, nil, err
	}
	if storedResult != nil {
		return false, storedResult, nil
	}
	claimed, err = b.client.SetNX(ctx, b.commandStateKey(command.CommandID), encodedPending, defaultCommandStateTTL).Result()
	if err != nil {
		return false, nil, err
	}
	if claimed {
		return true, pendingResult, nil
	}
	storedResult, err = b.loadCommandResult(ctx, command.CommandID)
	return false, storedResult, err
}

func (b *redisClusterCommandBus) loadCommandResult(ctx context.Context, commandID string) (*messageloop.ClusterCommandResult, error) {
	if commandID == "" {
		return nil, nil
	}
	data, err := b.client.Get(ctx, b.commandStateKey(commandID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := &messageloop.ClusterCommandResult{}
	if err := json.Unmarshal([]byte(data), result); err != nil {
		return nil, err
	}
	return result, nil
}

func (b *redisClusterCommandBus) storeCommandResult(ctx context.Context, result *messageloop.ClusterCommandResult) error {
	if result == nil || result.CommandID == "" {
		return nil
	}
	encodedResult, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return b.client.Set(ctx, b.commandStateKey(result.CommandID), encodedResult, defaultCommandStateTTL).Err()
}

func (b *redisClusterCommandBus) publishCommandResult(ctx context.Context, command *messageloop.ClusterCommand, result *messageloop.ClusterCommandResult) {
	if command == nil || result == nil {
		return
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

func (b *redisClusterCommandBus) executeHandler(ctx context.Context, handler messageloop.ClusterCommandHandler, command *messageloop.ClusterCommand) (result *messageloop.ClusterCommandResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			err = fmt.Errorf("panic in cluster command handler: %v", recovered)
		}
	}()
	return handler(ctx, command)
}

func (b *redisClusterCommandBus) resolveTimedOutCommand(ctx context.Context, command *messageloop.ClusterCommand) (*messageloop.ClusterCommandResult, error) {
	if command == nil {
		return nil, ctx.Err()
	}
	storedResult, err := b.loadCommandResult(context.Background(), command.CommandID)
	if err != nil {
		return nil, err
	}
	if storedResult != nil && storedResult.Status != messageloop.ClusterCommandStatusPending {
		return storedResult, nil
	}
	b.recordUnknownFinalState(ctx, command, "cluster command timed out before a terminal result was observed")
	return &messageloop.ClusterCommandResult{
		CommandID:     command.CommandID,
		SessionID:     command.SessionID,
		NodeID:        command.TargetNodeID,
		IncarnationID: command.TargetIncarnationID,
		Status:        messageloop.ClusterCommandStatusUnknownFinalState,
		ErrorCode:     "UNKNOWN_FINAL_STATE",
		ErrorMessage:  "cluster command timed out before a terminal result was observed",
	}, nil
}

func (b *redisClusterCommandBus) getMetrics() *messageloop.Metrics {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.metrics
}

func (b *redisClusterCommandBus) recordDedupeHit(ctx context.Context, cmd *messageloop.ClusterCommand, stage string) {
	if metrics := b.getMetrics(); metrics != nil {
		metrics.ClusterCommandDedupeHits.Inc()
	}
	if cmd == nil {
		return
	}
	log.DebugContext(ctx, "cluster command dedupe hit",
		"command_id", cmd.CommandID,
		"command_type", cmd.Type,
		"stage", stage,
		"target_node_id", cmd.TargetNodeID,
		"target_incarnation_id", cmd.TargetIncarnationID,
	)
}

func (b *redisClusterCommandBus) recordCommandTimeout(ctx context.Context, cmd *messageloop.ClusterCommand) {
	if metrics := b.getMetrics(); metrics != nil {
		metrics.ClusterCommandTimeouts.Inc()
	}
	if cmd == nil {
		return
	}
	log.WarnContext(ctx, "cluster command timed out waiting for reply",
		"command_id", cmd.CommandID,
		"command_type", cmd.Type,
		"target_node_id", cmd.TargetNodeID,
		"target_incarnation_id", cmd.TargetIncarnationID,
	)
}

func (b *redisClusterCommandBus) recordUnknownFinalState(ctx context.Context, cmd *messageloop.ClusterCommand, reason string) {
	if metrics := b.getMetrics(); metrics != nil {
		metrics.ClusterCommandUnknownFinalState.Inc()
	}
	if cmd == nil {
		return
	}
	log.WarnContext(ctx, "cluster command entered unknown final state",
		"command_id", cmd.CommandID,
		"command_type", cmd.Type,
		"target_node_id", cmd.TargetNodeID,
		"target_incarnation_id", cmd.TargetIncarnationID,
		"reason", reason,
	)
}

func cloneClusterCommandResult(result *messageloop.ClusterCommandResult) *messageloop.ClusterCommandResult {
	if result == nil {
		return nil
	}
	clone := *result
	if result.Metadata != nil {
		clone.Metadata = make(map[string]string, len(result.Metadata))
		for key, value := range result.Metadata {
			clone.Metadata[key] = value
		}
	}
	return &clone
}

var _ messageloop.ClusterCommandBus = (*redisClusterCommandBus)(nil)
