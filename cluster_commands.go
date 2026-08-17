package messageloop

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	"google.golang.org/protobuf/proto"
)

const (
	clusterCommandMetaDisconnectCode   = "disconnect_code"
	clusterCommandMetaDisconnectReason = "disconnect_reason"
	clusterCommandMetaSurveyTimeoutMS  = "survey_timeout_ms"
	clusterCommandMetaSurveyResults    = "survey_results"
	// clusterCommandMetaSurveyCountOnly marks a survey command as a pure
	// subscriber-count preflight (client survey gate): the receiving node
	// returns its local matching count and never runs localSurvey.
	clusterCommandMetaSurveyCountOnly = "count_only"
	// clusterCommandMetaSurveyCount carries the local matching subscriber
	// count in a count_only reply.
	clusterCommandMetaSurveyCount = "count"
)

// ClusterCommandHandler returns the node-local cluster command handler.
func (n *Node) ClusterCommandHandler() ClusterCommandHandler {
	return n.handleClusterCommand
}

// PublishToSession delivers a direct publication to a local or remote session.
func (n *Node) PublishToSession(ctx context.Context, sessionID string, msg *clientpb.Message) (bool, error) {
	if msg == nil {
		return false, nil
	}
	payload, err := proto.Marshal(msg)
	if err != nil {
		return false, err
	}
	result, err := n.dispatchSessionCommand(ctx, sessionID, &ClusterCommand{
		Type:      ClusterCommandPublish,
		SessionID: sessionID,
		Payload:   payload,
	})
	return clusterCommandSucceeded(result), err
}

// DisconnectSession disconnects a local or remote session.
func (n *Node) DisconnectSession(ctx context.Context, sessionID string, disconnect Disconnect) (bool, error) {
	result, err := n.dispatchSessionCommand(ctx, sessionID, &ClusterCommand{
		Type:      ClusterCommandDisconnect,
		SessionID: sessionID,
		Metadata: map[string]string{
			clusterCommandMetaDisconnectCode:   strconv.FormatUint(uint64(disconnect.Code), 10),
			clusterCommandMetaDisconnectReason: disconnect.Reason,
		},
	})
	return clusterCommandSucceeded(result), err
}

// adminPrincipal is the fixed authorization identity used for admin API
// operations (server-side gRPC API): "admin" matches allow lists exactly like
// a user ID (PR-KA-A4 §5.3).
const adminPrincipal = "admin"

// SubscribeSession subscribes a local or remote session to a channel.
// The channel is checked against the Authorizer with the admin principal
// before any command is dispatched; cluster command handlers trust the
// initiating node's check.
func (n *Node) SubscribeSession(ctx context.Context, sessionID, channel string) (bool, error) {
	if !n.AdminCanSubscribe(channel) {
		return false, fmt.Errorf("admin subscribe denied by ACL rule for channel %s", channel)
	}
	result, err := n.dispatchSessionCommand(ctx, sessionID, &ClusterCommand{
		Type:      ClusterCommandSubscribe,
		SessionID: sessionID,
		Channel:   channel,
	})
	return clusterCommandSucceeded(result), err
}

// UnsubscribeSession unsubscribes a local or remote session from a channel.
func (n *Node) UnsubscribeSession(ctx context.Context, sessionID, channel string) (bool, error) {
	if !n.AdminCanSubscribe(channel) {
		return false, fmt.Errorf("admin unsubscribe denied by ACL rule for channel %s", channel)
	}
	result, err := n.dispatchSessionCommand(ctx, sessionID, &ClusterCommand{
		Type:      ClusterCommandUnsubscribe,
		SessionID: sessionID,
		Channel:   channel,
	})
	return clusterCommandSucceeded(result), err
}

// AdminCanSubscribe reports whether the admin API may subscribe a session to
// channel (PR-KA-A4 §8.4): the key must compile (bare "*"/"**" always fail,
// even with pattern.global), subscribe.any skips the static allow lists
// (deny_all still binds), and otherwise the subscribe decision must allow
// the admin principal.
func (n *Node) AdminCanSubscribe(channel string) bool {
	if _, err := CompileInterest(channel); err != nil {
		return false
	}
	p := n.adminPrincipal()
	if p.Caps&CapSubscribeAny != 0 {
		return n.authorizer.decideSubscribeSkipAllowLists(channel).Allow
	}
	return n.authorizer.Decide(p, ActionSubscribePattern, channel).Allow
}

// AdminCanPublish reports whether the admin API may publish to channel under
// the authorizer rules (PR-KA-A4 §8.4).
func (n *Node) AdminCanPublish(channel string) bool {
	return n.authorizer.Decide(n.adminPrincipal(), ActionPublish, channel).Allow
}

func (n *Node) dispatchSessionCommand(ctx context.Context, sessionID string, cmd *ClusterCommand) (*ClusterCommandResult, error) {
	if cmd == nil || sessionID == "" {
		return nil, nil
	}

	lease, err := n.resolveSessionLease(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return nil, nil
	}

	if cmd.CommandID == "" {
		cmd.CommandID = uuid.NewString()
	}
	cmd.IssuedAt = time.Now()
	cmd.TargetNodeID = lease.NodeID
	cmd.TargetIncarnationID = lease.IncarnationID
	cmd.SessionID = sessionID

	if !n.ClusterEnabled() || (lease.NodeID == n.ClusterNodeID() && lease.IncarnationID == n.ClusterIncarnationID()) {
		return n.handleClusterCommand(ctx, cmd)
	}

	return n.clusterCommandBus().SendCommand(ctx, cmd)
}

func (n *Node) resolveSessionLease(ctx context.Context, sessionID string) (*ClusterSessionLease, error) {
	if sessionID == "" {
		return nil, nil
	}

	if client := n.hub.LookupSession(sessionID); client != nil {
		lease := n.clusterSessionLease(client)
		if !n.ClusterEnabled() {
			lease.NodeID = ""
			lease.IncarnationID = ""
		}
		return lease, nil
	}

	if !n.ClusterEnabled() {
		return nil, nil
	}

	return n.clusterSessionDirectory().GetSessionLease(ctx, sessionID)
}

func (n *Node) handleClusterCommand(ctx context.Context, cmd *ClusterCommand) (*ClusterCommandResult, error) {
	result := &ClusterCommandResult{
		CommandID:     cmd.CommandID,
		SessionID:     cmd.SessionID,
		NodeID:        n.ClusterNodeID(),
		IncarnationID: n.ClusterIncarnationID(),
		Status:        ClusterCommandStatusSucceeded,
		Metadata:      map[string]string{},
	}

	switch cmd.Type {
	case ClusterCommandPublish:
		return n.handleClusterPublishCommand(ctx, cmd, result), nil
	case ClusterCommandDisconnect:
		return n.handleClusterDisconnectCommand(ctx, cmd, result), nil
	case ClusterCommandSubscribe:
		return n.handleClusterSubscribeCommand(ctx, cmd, result), nil
	case ClusterCommandUnsubscribe:
		return n.handleClusterUnsubscribeCommand(ctx, cmd, result), nil
	case ClusterCommandSurvey:
		return n.handleClusterSurveyCommand(ctx, cmd, result), nil
	case ClusterCommandTakeover:
		return n.handleClusterTakeoverCommand(ctx, cmd, result), nil
	default:
		result.Status = ClusterCommandStatusFailed
		result.ErrorCode = "UNSUPPORTED_CLUSTER_COMMAND"
		result.ErrorMessage = fmt.Sprintf("unsupported cluster command: %s", cmd.Type)
		return result, nil
	}
}

func (n *Node) handleClusterPublishCommand(ctx context.Context, cmd *ClusterCommand, result *ClusterCommandResult) *ClusterCommandResult {
	client := n.hub.LookupSession(cmd.SessionID)
	if client == nil {
		return clusterCommandSessionNotFound(result)
	}

	message := &clientpb.Message{}
	if err := proto.Unmarshal(cmd.Payload, message); err != nil {
		result.Status = ClusterCommandStatusFailed
		result.ErrorCode = "INVALID_CLUSTER_PAYLOAD"
		result.ErrorMessage = err.Error()
		return result
	}

	out := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Publication{
			Publication: &clientpb.Publication{Messages: []*clientpb.Message{message}},
		}
	})
	if err := client.Send(ctx, out); err != nil {
		result.Status = ClusterCommandStatusFailed
		result.ErrorCode = "SESSION_PUBLISH_FAILED"
		result.ErrorMessage = err.Error()
		if n.metrics != nil {
			n.metrics.DeliveryFailures.Inc()
		}
	} else if n.metrics != nil {
		n.metrics.MessagesDelivered.Inc()
	}
	return result
}

func (n *Node) handleClusterDisconnectCommand(ctx context.Context, cmd *ClusterCommand, result *ClusterCommandResult) *ClusterCommandResult {
	client := n.hub.LookupSession(cmd.SessionID)
	if client == nil {
		return clusterCommandSessionNotFound(result)
	}

	code, _ := strconv.ParseUint(cmd.Metadata[clusterCommandMetaDisconnectCode], 10, 32)
	disconnect := Disconnect{
		Code:   uint32(code),
		Reason: cmd.Metadata[clusterCommandMetaDisconnectReason],
	}
	if err := client.Close(disconnect); err != nil {
		result.Status = ClusterCommandStatusFailed
		result.ErrorCode = "SESSION_DISCONNECT_FAILED"
		result.ErrorMessage = err.Error()
	}
	return result
}

func (n *Node) handleClusterSubscribeCommand(ctx context.Context, cmd *ClusterCommand, result *ClusterCommandResult) *ClusterCommandResult {
	client := n.hub.LookupSession(cmd.SessionID)
	if client == nil {
		return clusterCommandSessionNotFound(result)
	}

	_, alreadySubscribed := n.hub.LookupSubscriber(cmd.Channel, client)
	if err := n.AddSubscription(ctx, cmd.Channel, NewSubscriber(client, false)); err != nil {
		result.Status = ClusterCommandStatusFailed
		result.ErrorCode = "SESSION_SUBSCRIBE_FAILED"
		result.ErrorMessage = err.Error()
		return result
	}
	if !alreadySubscribed {
		// presenceJoin gates on shouldTrackPresence: wildcard channels and
		// presence=false policies never enter the store here either.
		n.presenceJoin(ctx, cmd.Channel, client)
	}
	return result
}

func (n *Node) handleClusterUnsubscribeCommand(ctx context.Context, cmd *ClusterCommand, result *ClusterCommandResult) *ClusterCommandResult {
	client := n.hub.LookupSession(cmd.SessionID)
	if client == nil {
		return clusterCommandSessionNotFound(result)
	}

	stored, alreadySubscribed := n.hub.LookupSubscriber(cmd.Channel, client)
	ephemeral := alreadySubscribed && stored.Ephemeral
	if err := n.RemoveSubscription(cmd.Channel, client); err != nil {
		result.Status = ClusterCommandStatusFailed
		result.ErrorCode = "SESSION_UNSUBSCRIBE_FAILED"
		result.ErrorMessage = err.Error()
		return result
	}
	if alreadySubscribed {
		// Read Ephemeral before RemoveSubscription: a locally created
		// ephemeral subscription unsubscribed via Admin must not emit leave.
		n.presenceLeave(ctx, cmd.Channel, client.SessionID(), client.UserID(), ephemeral)
	}
	return result
}

func (n *Node) handleClusterTakeoverCommand(ctx context.Context, cmd *ClusterCommand, result *ClusterCommandResult) *ClusterCommandResult {
	client := n.hub.LookupSession(cmd.SessionID)
	if client == nil {
		return result
	}

	client.mu.RLock()
	currentLeaseVersion := client.clusterLeaseVersion
	client.mu.RUnlock()
	if currentLeaseVersion == 0 {
		currentLeaseVersion = 1
	}
	if cmd.LeaseVersion > 0 && currentLeaseVersion != cmd.LeaseVersion {
		result.Status = ClusterCommandStatusFailed
		result.ErrorCode = "LEASE_VERSION_MISMATCH"
		result.ErrorMessage = "cluster takeover lease version mismatch"
		return result
	}

	if err := client.Fence(DisconnectStale); err != nil {
		result.Status = ClusterCommandStatusFailed
		result.ErrorCode = "SESSION_TAKEOVER_FAILED"
		result.ErrorMessage = err.Error()
	}
	return result
}

func (n *Node) handleClusterSurveyCommand(ctx context.Context, cmd *ClusterCommand, result *ClusterCommandResult) *ClusterCommandResult {
	// count_only preflight (client survey gate): return the local matching
	// subscriber count and return without ever running localSurvey. The
	// Admin Node.Survey path never sets count_only and is unaffected.
	if cmd.Metadata[clusterCommandMetaSurveyCountOnly] == "true" {
		result.Metadata[clusterCommandMetaSurveyCount] = strconv.Itoa(len(n.hub.GetMatchingSubscribers(cmd.Channel)))
		return result
	}

	timeout := 5 * time.Second
	if rawTimeout, ok := cmd.Metadata[clusterCommandMetaSurveyTimeoutMS]; ok && rawTimeout != "" {
		if timeoutMS, err := strconv.ParseInt(rawTimeout, 10, 64); err == nil && timeoutMS > 0 {
			timeout = time.Duration(timeoutMS) * time.Millisecond
		}
	}

	surveyResults, err := n.localSurvey(ctx, cmd.Channel, cmd.Payload, timeout)
	if err != nil {
		result.Status = ClusterCommandStatusFailed
		result.ErrorCode = "SURVEY_EXECUTION_FAILED"
		result.ErrorMessage = err.Error()
		return result
	}
	encodedResults, err := encodeClusterSurveyResults(surveyResults)
	if err != nil {
		result.Status = ClusterCommandStatusFailed
		result.ErrorCode = "SURVEY_RESULT_ENCODING_FAILED"
		result.ErrorMessage = err.Error()
		return result
	}
	result.Metadata[clusterCommandMetaSurveyResults] = encodedResults
	return result
}

func clusterCommandSessionNotFound(result *ClusterCommandResult) *ClusterCommandResult {
	result.Status = ClusterCommandStatusFailed
	result.ErrorCode = "SESSION_NOT_FOUND"
	result.ErrorMessage = "session not found"
	return result
}

func clusterCommandSucceeded(result *ClusterCommandResult) bool {
	return result != nil && result.Status == ClusterCommandStatusSucceeded
}
