package runtime

import (
	"context"
	"time"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
)

// ServerAPIRuntime is the Node slice the Server API handlers depend on
// (architecture review D16). The handlers program against this seam instead
// of Node's full surface: transports use a three-method slice,
// session.Runtime is the session plane's thirty-eight-method seam, and this
// is the server plane's own — one named interface per caller, all satisfied
// by the same *Node implementation (KD-K26 keeps internal/runtime the facade
// home; nothing moves out of the package).
type ServerAPIRuntime interface {
	// Authorization (principal travels with the call, S4).
	APIDecide(p Principal, action Action, channel string) Decision
	APICanPublish(p Principal, channel string) bool
	APISessionNamespace(ctx context.Context, sessionID string) (string, bool)

	// Per-user fan-out.
	ExpandUserSessions(ctx context.Context, namespace, userID string) []string
	ObserveAPIUserFanout(op string, sessions int)

	// Per-session delivery and lifecycle (cluster commands).
	PublishToSession(ctx context.Context, sessionID string, msg *clientpb.Message) (bool, error)
	DisconnectSession(ctx context.Context, sessionID string, disconnect Disconnect) (bool, error)
	SubscribeSession(ctx context.Context, p Principal, sessionID, channel string) (bool, error)
	UnsubscribeSession(ctx context.Context, p Principal, sessionID, channel string) (bool, error)

	// Survey, presence, channels.
	Survey(ctx context.Context, channel string, payload []byte, timeout time.Duration) ([]*SurveyResult, error)
	Presence(ctx context.Context, ch string) (map[string]*PresenceInfo, error)
	PresenceSnapshotLimit(ch string) int
	Channels(ctx context.Context) ([]ChannelInfo, error)
	CountMatchingSubscribers(ctx context.Context, ch string) (int, error)
	ChannelPolicy(ch string) ChannelPolicy

	// Broker access (GetHistory) and the recover-contract epoch check.
	Broker() Broker
	StreamEpoch() string

	// Server-side publication honoring add_history.
	PublishForAPI(ch string, pub *Publication, addHistory bool) error
}

// *Node is the one production adapter of the ServerAPIRuntime seam.
var _ ServerAPIRuntime = (*Node)(nil)
