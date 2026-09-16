package admin

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/pkg/transport/grpc"
	"github.com/messageloopio/messageloop/proxy"
	"github.com/messageloopio/messageloop/shared"
	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// The admin API key rejection semantics matrix (design §2.4), driven end to
// end: real gRPC server + auth interceptor + scope layer + handler. Two
// tenants (acme, beta) each hold one live session; every identity class of
// the matrix (scoped acme / scoped beta / global ["*"] key / static token)
// is exercised against every carrier (namespace parameter, channel list,
// single channel, session ID, user list).
//
// Matrix principle: named parameters reject whole-RPC with
// PermissionDenied; ID-addressed data becomes invisible (not-found shapes,
// never errors); channel lists are per-item; the ns:topic grammar gate
// (G4) rejects InvalidArgument for every identity.

const (
	matrixAcmeKey    = "sk-acme-tenant-key-0123456789abcdef"
	matrixBetaKey    = "sk-beta-tenant-key-fedcba9876543210"
	matrixGlobalKey  = "sk-platform-global-key-001122334455"
	matrixStaticTok  = "static-superadmin-token-0123456789abcdef"
	matrixNsAcme     = "acme"
	matrixNsBeta     = "beta"
	matrixAcmeSess   = "matrix-acme-session"
	matrixBetaSess   = "matrix-beta-session"
	matrixAcmeUser   = "matrix-acme-user"
	matrixAcmeCh     = "acme:chat.room1"
	matrixBetaCh     = "beta:chat.room1"
	matrixAcmeChan2  = "acme:chat.room2"
	matrixBareCh     = "chat.general" // no namespace: fails the G4 grammar gate
	matrixMaxAgeSecs = 60
)

// matrixFullCaps is the full non-global capability label set (the node
// ceiling is DefaultAdminCapabilities: every bit except pattern.global).
var matrixFullCaps = []string{
	"session.act",
	"user.fanout",
	"subscribe.any",
	"history.read",
	"presence.read",
	"channels.list",
	"survey.bypass_gate",
	"presence.large_snapshot",
}

// keyedAdminAuthProxy answers AuthenticateAdmin from a presented-key →
// identity table (the fake findProxy path: scoped identities enter through
// the real auth chain). Everything else is inherited no-op from
// fakeAdminAuthProxy.
type keyedAdminAuthProxy struct {
	fakeAdminAuthProxy
	mu         sync.Mutex
	identities map[string]*proxy.AdminIdentityInfo
	verifyalls int
}

func (f *keyedAdminAuthProxy) AuthenticateAdmin(ctx context.Context, req *proxy.AuthenticateAdminProxyRequest) (*proxy.AuthenticateAdminProxyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifyalls++
	identity, ok := f.identities[req.APIKey]
	if !ok {
		return &proxy.AuthenticateAdminProxyResponse{
			Error: &sharedv2.Error{Code: "KEY_NOT_FOUND", Message: "unknown admin api key"},
		}, nil
	}
	return &proxy.AuthenticateAdminProxyResponse{Identity: identity}, nil
}

// matrixServer bundles the e2e fixtures of one scenario.
type matrixServer struct {
	node *runtime.Node
	api  serverv2.APIServiceClient
}

// startMatrixServer boots a real admin gRPC server: static auth_tokens
// (superadmin) + a fake admin_auth proxy issuing scoped identities. All
// keys share max_age 60s (positive cache 30s default — stable within a
// test).
func startMatrixServer(t *testing.T, cfg *config.Server, extraIdentities map[string]*proxy.AdminIdentityInfo) *matrixServer {
	t.Helper()
	ctx := t.Context()
	node := runtime.NewNode(cfg)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	p := &keyedAdminAuthProxy{identities: map[string]*proxy.AdminIdentityInfo{
		matrixAcmeKey:   {KeyID: "key-acme-1", Namespaces: []string{matrixNsAcme}, Capabilities: matrixFullCaps, MaxAgeSeconds: matrixMaxAgeSecs},
		matrixBetaKey:   {KeyID: "key-beta-1", Namespaces: []string{matrixNsBeta}, Capabilities: matrixFullCaps, MaxAgeSeconds: matrixMaxAgeSecs},
		matrixGlobalKey: {KeyID: "key-global-1", Namespaces: []string{"*"}, Capabilities: matrixFullCaps, MaxAgeSeconds: matrixMaxAgeSecs},
	}}
	for key, identity := range extraIdentities {
		p.identities[key] = identity
	}

	server, err := PrepareAdminServer(grpc.Options{
		Addr:           "127.0.0.1:0",
		AuthTokens:     []string{matrixStaticTok},
		AdminFindProxy: func() proxy.Proxy { return p },
	}, node, nil, nil)
	require.NoError(t, err)
	go func() { _ = server.Start(ctx) }()
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	conn, err := googlegrpc.NewClient(server.Addr(), googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return &matrixServer{node: node, api: serverv2.NewAPIServiceClient(conn)}
}

// matrixCtx returns an outgoing context carrying the Bearer credential.
func matrixCtx(key string) context.Context {
	return metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+key))
}

// addMatrixSession registers a live session in the hub under the given
// namespace and returns its capture transport.
func addMatrixSession(t *testing.T, node *runtime.Node, sessionID, userID, namespace string) *captureTransport {
	t.Helper()
	transport := &captureTransport{}
	client, _, err := runtime.NewClient(context.Background(), node, transport, shared.JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs(sessionID, userID, "client-"+sessionID)
	client.SetNamespaceForTest(namespace)
	require.NoError(t, node.AddClient(client))
	return transport
}

// requireResultsEntry asserts the map explicitly contains the key with the
// given value (absent keys read false in Go — the distinction between
// "explicitly failed" and "invisible" is part of the matrix contract).
func requireResultsEntry(t *testing.T, results map[string]bool, key string, want bool) {
	t.Helper()
	require.Contains(t, results, key, "results must explicitly contain %q", key)
	require.Equal(t, want, results[key], "results[%q]", key)
}

// TestAdminMatrix_Publish_ChannelLists covers the channel-carrier row of the
// matrix: out-of-namespace channels count failed while in-scope ones still
// deliver (partial success preserved); an all-out-of-scope request fails
// the whole RPC; global/static identities cross namespaces freely.
func TestAdminMatrix_Publish_ChannelLists(t *testing.T) {
	ms := startMatrixServer(t, nil, nil)

	pub := func(key, id string, channels ...string) *serverv2.PublishRequest {
		return &serverv2.PublishRequest{
			RequestId: id,
			Publications: []*serverv2.Publication{{
				Id:          id,
				Destination: &serverv2.Publication_Destination{Channels: channels},
				Payload:     &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: id}},
				Options:     &serverv2.Publication_Options{AddHistory: true},
			}},
		}
	}

	// acme → mixed list: partial success (no error), beta channel untouched.
	_, err := ms.api.Publish(matrixCtx(matrixAcmeKey), pub(matrixAcmeKey, "mixed-pub", matrixAcmeCh, matrixBetaCh))
	require.NoError(t, err, "an out-of-namespace channel counts failed; the in-scope channel still delivers (partial success)")

	page, err := ms.node.Broker().History(matrixAcmeCh, 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Pubs(), 1, "the in-scope channel must receive the publication")
	page, err = ms.node.Broker().History(matrixBetaCh, 0, 10)
	require.NoError(t, err)
	require.Empty(t, page.Pubs(), "the out-of-namespace channel must receive nothing (filtered by the scope layer)")

	// acme → beta only: every attempt failed → the RPC reports it.
	_, err = ms.api.Publish(matrixCtx(matrixAcmeKey), pub(matrixAcmeKey, "beta-only", matrixBetaCh))
	require.Error(t, err, "a request whose every target is out of scope must fail")
	require.Equal(t, codes.Internal, status.Code(err), "all-failed publish reports Internal (same shape as broker failure)")

	// beta → its own channel works.
	_, err = ms.api.Publish(matrixCtx(matrixBetaKey), pub(matrixBetaKey, "beta-self", matrixBetaCh))
	require.NoError(t, err)

	// global key and static token cross namespaces freely.
	for _, key := range []string{matrixGlobalKey, matrixStaticTok} {
		_, err = ms.api.Publish(matrixCtx(key), pub(key, "cross-ns", matrixBetaCh))
		require.NoError(t, err, "global/static identities are not namespace-scoped")
	}
}

// TestAdminMatrix_Publish_Sessions_InvisibleErased covers the session-ID
// carrier: invisible sessions are erased, not reported — the response shape
// equals the "session not found" behavior (attempted, skipped, no error).
func TestAdminMatrix_Publish_Sessions_InvisibleErased(t *testing.T) {
	ms := startMatrixServer(t, nil, nil)
	acmeTransport := addMatrixSession(t, ms.node, matrixAcmeSess, matrixAcmeUser, matrixNsAcme)
	betaTransport := addMatrixSession(t, ms.node, matrixBetaSess, "matrix-beta-user", matrixNsBeta)

	textPub := func(id string, sessions ...string) *serverv2.PublishRequest {
		return &serverv2.PublishRequest{
			RequestId: id,
			Publications: []*serverv2.Publication{{
				Id:          id,
				Destination: &serverv2.Publication_Destination{Sessions: sessions},
				Payload:     &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: id}},
			}},
		}
	}

	// acme key → beta session: erased, the RPC answers like not-found.
	resp, err := ms.api.Publish(matrixCtx(matrixAcmeKey), textPub("erase-beta", matrixBetaSess))
	require.NoError(t, err, "an invisible session must degrade to not-found, never an error")
	require.NotNil(t, resp)
	require.Never(t, func() bool {
		return transportContainsText(betaTransport, "erase-beta")
	}, 300*time.Millisecond, 50*time.Millisecond, "the invisible session must not receive the publication")

	// acme key → acme session: delivered.
	resp, err = ms.api.Publish(matrixCtx(matrixAcmeKey), textPub("deliver-acme", matrixAcmeSess))
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Eventually(t, func() bool {
		return transportContainsText(acmeTransport, "deliver-acme")
	}, 2*time.Second, 20*time.Millisecond, "the in-scope session must receive the publication")

	// mixed: in-scope delivered, invisible erased, still no error.
	_, err = ms.api.Publish(matrixCtx(matrixAcmeKey), textPub("mixed-sessions", matrixAcmeSess, matrixBetaSess))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return transportContainsText(acmeTransport, "mixed-sessions")
	}, 2*time.Second, 20*time.Millisecond)

	// static token sees everything.
	_, err = ms.api.Publish(matrixCtx(matrixStaticTok), textPub("static-beta", matrixBetaSess))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return transportContainsText(betaTransport, "static-beta")
	}, 2*time.Second, 20*time.Millisecond, "static identities address sessions in every namespace")
}

// TestAdminMatrix_Publish_Users_NamespaceParam covers the namespace
// parameter carrier on Publish: mismatch rejects the whole RPC before any
// expansion; the in-scope expansion fans out.
func TestAdminMatrix_Publish_Users_NamespaceParam(t *testing.T) {
	ms := startMatrixServer(t, nil, nil)
	acmeTransport := addMatrixSession(t, ms.node, matrixAcmeSess, matrixAcmeUser, matrixNsAcme)

	// namespace parameter outside the identity scope → whole-RPC deny.
	_, err := ms.api.Publish(matrixCtx(matrixAcmeKey), &serverv2.PublishRequest{
		RequestId: "ns-beta",
		Publications: []*serverv2.Publication{{
			Id:          "ns-beta",
			Destination: &serverv2.Publication_Destination{Namespace: matrixNsBeta, Users: []string{"anyone"}},
			Payload:     &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "x"}},
		}},
	})
	require.Equal(t, codes.PermissionDenied, status.Code(err),
		"a namespace parameter outside the scope rejects the whole RPC (named parameters reject)")

	// in-scope user fan-out works.
	_, err = ms.api.Publish(matrixCtx(matrixAcmeKey), &serverv2.PublishRequest{
		RequestId: "ns-acme",
		Publications: []*serverv2.Publication{{
			Id:          "ns-acme",
			Destination: &serverv2.Publication_Destination{Namespace: matrixNsAcme, Users: []string{matrixAcmeUser}},
			Payload:     &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "fanout-acme"}},
		}},
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return transportContainsText(acmeTransport, "fanout-acme")
	}, 2*time.Second, 20*time.Millisecond, "the in-scope user expansion must fan out")
}

// TestAdminMatrix_Disconnect_Sessions covers Disconnect per the matrix: the
// invisible session is absent from the results map (reads false — not-found,
// never an error), the in-scope session reports true, static identities see
// everything.
func TestAdminMatrix_Disconnect_Sessions(t *testing.T) {
	ms := startMatrixServer(t, nil, nil)
	acmeTransport := addMatrixSession(t, ms.node, matrixAcmeSess, matrixAcmeUser, matrixNsAcme)
	betaTransport := addMatrixSession(t, ms.node, matrixBetaSess, "matrix-beta-user", matrixNsBeta)

	// acme key → beta session: invisible, results lack the key entirely.
	resp, err := ms.api.Disconnect(matrixCtx(matrixAcmeKey), &serverv2.DisconnectRequest{
		Sessions: []string{matrixBetaSess},
		Code:     3500,
	})
	require.NoError(t, err, "an invisible session must not error (not-found, not denied)")
	require.NotContains(t, resp.Results, matrixBetaSess,
		"the invisible session must not appear in the results at all (existence not leaked)")
	require.False(t, resp.Results[matrixBetaSess])
	require.False(t, betaTransport.closed, "the beta session must stay connected")

	// acme key → acme session: true and actually disconnected.
	resp, err = ms.api.Disconnect(matrixCtx(matrixAcmeKey), &serverv2.DisconnectRequest{
		Sessions: []string{matrixAcmeSess},
		Code:     3500,
	})
	require.NoError(t, err)
	requireResultsEntry(t, resp.Results, matrixAcmeSess, true)
	require.True(t, acmeTransport.closed)

	// static token → beta session: visible, disconnected.
	resp, err = ms.api.Disconnect(matrixCtx(matrixStaticTok), &serverv2.DisconnectRequest{
		Sessions: []string{matrixBetaSess},
		Code:     3500,
	})
	require.NoError(t, err)
	requireResultsEntry(t, resp.Results, matrixBetaSess, true)
	require.True(t, betaTransport.closed)
}

// TestAdminMatrix_Disconnect_Users_NamespaceParam covers the namespace
// parameter carrier on Disconnect: mismatch rejects the whole RPC, in-scope
// user fan-out disconnects the tenant's own sessions.
func TestAdminMatrix_Disconnect_Users_NamespaceParam(t *testing.T) {
	ms := startMatrixServer(t, nil, nil)
	acmeTransport := addMatrixSession(t, ms.node, matrixAcmeSess, matrixAcmeUser, matrixNsAcme)
	addMatrixSession(t, ms.node, matrixBetaSess, "matrix-beta-user", matrixNsBeta)

	// ns parameter out of scope → whole-RPC PermissionDenied, nothing runs.
	_, err := ms.api.Disconnect(matrixCtx(matrixAcmeKey), &serverv2.DisconnectRequest{
		Namespace: matrixNsBeta,
		Users:     []string{"anyone"},
		Code:      3500,
	})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// in-scope user fan-out disconnects only the tenant's sessions.
	resp, err := ms.api.Disconnect(matrixCtx(matrixAcmeKey), &serverv2.DisconnectRequest{
		Namespace: matrixNsAcme,
		Users:     []string{matrixAcmeUser},
		Code:      3500,
	})
	require.NoError(t, err)
	requireResultsEntry(t, resp.Results, matrixAcmeSess, true)
	require.True(t, acmeTransport.closed)
}

// TestAdminMatrix_Subscribe_Channels covers the Subscribe channel carrier:
// out-of-namespace channels report explicit results=false while the
// in-scope channel subscribes; static identities subscribe everywhere.
func TestAdminMatrix_Subscribe_Channels(t *testing.T) {
	ms := startMatrixServer(t, nil, nil)
	addMatrixSession(t, ms.node, matrixAcmeSess, matrixAcmeUser, matrixNsAcme)
	addMatrixSession(t, ms.node, matrixBetaSess, "matrix-beta-user", matrixNsBeta)

	resp, err := ms.api.Subscribe(matrixCtx(matrixAcmeKey), &serverv2.SubscribeRequest{
		SessionId: matrixAcmeSess,
		Channels:  []string{matrixAcmeCh, matrixBetaCh},
	})
	require.NoError(t, err)
	requireResultsEntry(t, resp.Results, matrixAcmeCh, true)
	requireResultsEntry(t, resp.Results, matrixBetaCh, false)

	// the subscription actually landed for the in-scope channel only.
	require.Equal(t, 1, ms.node.Hub().NumSubscribers(matrixAcmeCh))
	require.Equal(t, 0, ms.node.Hub().NumSubscribers(matrixBetaCh))

	// static token: cross-namespace subscribe succeeds.
	resp, err = ms.api.Subscribe(matrixCtx(matrixStaticTok), &serverv2.SubscribeRequest{
		SessionId: matrixAcmeSess,
		Channels:  []string{matrixBetaCh},
	})
	require.NoError(t, err)
	requireResultsEntry(t, resp.Results, matrixBetaCh, true)
}

// TestAdminMatrix_Subscribe_InvisibleSession_ShortCircuit is the rethink-fix
// regression: an acme key addressing only a beta session gets a synthesized
// not-found response (results[ch]=false for every channel) — never the
// handler's "session_id and user_id must not both be empty"
// InvalidArgument, which would leak that the session was special-cased.
func TestAdminMatrix_Subscribe_InvisibleSession_ShortCircuit(t *testing.T) {
	ms := startMatrixServer(t, nil, nil)
	addMatrixSession(t, ms.node, matrixAcmeSess, matrixAcmeUser, matrixNsAcme)
	addMatrixSession(t, ms.node, matrixBetaSess, "matrix-beta-user", matrixNsBeta)

	resp, err := ms.api.Subscribe(matrixCtx(matrixAcmeKey), &serverv2.SubscribeRequest{
		SessionId: matrixBetaSess,
		Channels:  []string{matrixAcmeCh, matrixAcmeChan2},
	})
	require.NoError(t, err, "the short-circuit must synthesize a not-found response, not InvalidArgument")
	requireResultsEntry(t, resp.Results, matrixAcmeCh, false)
	requireResultsEntry(t, resp.Results, matrixAcmeChan2, false)
	require.Zero(t, ms.node.Hub().NumSubscribers(matrixAcmeCh), "no subscription may land")

	// Unsubscribe is symmetric.
	resp2, err := ms.api.Unsubscribe(matrixCtx(matrixAcmeKey), &serverv2.UnsubscribeRequest{
		SessionId: matrixBetaSess,
		Channels:  []string{matrixAcmeCh},
	})
	require.NoError(t, err)
	requireResultsEntry(t, resp2.Results, matrixAcmeCh, false)
}

// TestAdminMatrix_Subscribe_UserID_NamespaceParam covers the user_id carrier
// on Subscribe/Unsubscribe: the namespace parameter must stay in scope.
func TestAdminMatrix_Subscribe_UserID_NamespaceParam(t *testing.T) {
	ms := startMatrixServer(t, nil, nil)
	addMatrixSession(t, ms.node, matrixAcmeSess, matrixAcmeUser, matrixNsAcme)

	_, err := ms.api.Subscribe(matrixCtx(matrixAcmeKey), &serverv2.SubscribeRequest{
		Namespace: matrixNsBeta,
		UserId:    "anyone",
		Channels:  []string{matrixBetaCh},
	})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	_, err = ms.api.Unsubscribe(matrixCtx(matrixAcmeKey), &serverv2.UnsubscribeRequest{
		Namespace: matrixNsBeta,
		UserId:    "anyone",
		Channels:  []string{matrixBetaCh},
	})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// in-scope user_id subscription works.
	resp, err := ms.api.Subscribe(matrixCtx(matrixAcmeKey), &serverv2.SubscribeRequest{
		Namespace: matrixNsAcme,
		UserId:    matrixAcmeUser,
		Channels:  []string{matrixAcmeCh},
	})
	require.NoError(t, err)
	requireResultsEntry(t, resp.Results, matrixAcmeCh, true)
}

// TestAdminMatrix_SingleChannelRPCs_Namespace covers the single-channel
// matrix row (Survey/GetPresence/GetHistory): an out-of-namespace channel
// rejects the RPC with PermissionDenied for scoped keys; in-scope channels
// are served; global/static identities are unaffected.
func TestAdminMatrix_SingleChannelRPCs_Namespace(t *testing.T) {
	ms := startMatrixServer(t, nil, nil)
	addMatrixSession(t, ms.node, matrixAcmeSess, matrixAcmeUser, matrixNsAcme)

	// acme key against the beta channel: PermissionDenied on all three.
	_, err := ms.api.Survey(matrixCtx(matrixAcmeKey), &serverv2.SurveyRequest{Channel: matrixBetaCh})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "Survey to an out-of-namespace channel must be denied")
	_, err = ms.api.GetPresence(matrixCtx(matrixAcmeKey), &serverv2.GetPresenceRequest{Channel: matrixBetaCh})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "GetPresence to an out-of-namespace channel must be denied")
	_, err = ms.api.GetHistory(matrixCtx(matrixAcmeKey), &serverv2.GetHistoryRequest{Channel: matrixBetaCh})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "GetHistory to an out-of-namespace channel must be denied")

	// acme key against its own channel: served (the key holds the full label
	// set, so Survey bypasses the gate and history/presence read normally).
	_, err = ms.api.Survey(matrixCtx(matrixAcmeKey), &serverv2.SurveyRequest{Channel: matrixAcmeCh})
	require.NoError(t, err)
	presence, err := ms.api.GetPresence(matrixCtx(matrixAcmeKey), &serverv2.GetPresenceRequest{Channel: matrixAcmeCh})
	require.NoError(t, err)
	require.Empty(t, presence.GetClients())
	history, err := ms.api.GetHistory(matrixCtx(matrixAcmeKey), &serverv2.GetHistoryRequest{Channel: matrixAcmeCh})
	require.NoError(t, err)
	require.Empty(t, history.GetPublications())

	// beta key sees its own namespace, not acme's.
	_, err = ms.api.GetHistory(matrixCtx(matrixBetaKey), &serverv2.GetHistoryRequest{Channel: matrixBetaCh})
	require.NoError(t, err)
	_, err = ms.api.GetHistory(matrixCtx(matrixBetaKey), &serverv2.GetHistoryRequest{Channel: matrixAcmeCh})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// global key and static token read across namespaces.
	for _, key := range []string{matrixGlobalKey, matrixStaticTok} {
		_, err = ms.api.GetHistory(matrixCtx(key), &serverv2.GetHistoryRequest{Channel: matrixBetaCh})
		require.NoError(t, err, "global/static identities read history across namespaces")
		_, err = ms.api.GetPresence(matrixCtx(key), &serverv2.GetPresenceRequest{Channel: matrixBetaCh})
		require.NoError(t, err, "global/static identities read presence across namespaces")
	}
}

// TestAdminMatrix_ChannelGrammarGate is the G4 tightening: every identity —
// including the static token and global keys — gets InvalidArgument for
// channel references without a namespace, and the grammar gate fires before
// the namespace scope check (a syntactically invalid channel in a foreign
// namespace is still InvalidArgument, never PermissionDenied).
func TestAdminMatrix_ChannelGrammarGate(t *testing.T) {
	ms := startMatrixServer(t, nil, nil)
	addMatrixSession(t, ms.node, matrixAcmeSess, matrixAcmeUser, matrixNsAcme)

	for _, key := range []string{matrixStaticTok, matrixGlobalKey, matrixAcmeKey} {
		_, err := ms.api.Publish(matrixCtx(key), &serverv2.PublishRequest{
			RequestId: "grammar",
			Publications: []*serverv2.Publication{{
				Id:          "grammar",
				Destination: &serverv2.Publication_Destination{Channels: []string{matrixBareCh}},
				Payload:     &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "x"}},
			}},
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "identity %s: a channel without a namespace fails the grammar gate", key)

		_, err = ms.api.Subscribe(matrixCtx(key), &serverv2.SubscribeRequest{
			SessionId: matrixAcmeSess,
			Channels:  []string{matrixBareCh},
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "identity %s: a channel without a namespace fails the grammar gate", key)

		_, err = ms.api.Survey(matrixCtx(key), &serverv2.SurveyRequest{Channel: matrixBareCh})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "identity %s: a channel without a namespace fails the grammar gate", key)
		_, err = ms.api.GetPresence(matrixCtx(key), &serverv2.GetPresenceRequest{Channel: matrixBareCh})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "identity %s: a channel without a namespace fails the grammar gate", key)
		_, err = ms.api.GetHistory(matrixCtx(key), &serverv2.GetHistoryRequest{Channel: matrixBareCh})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "identity %s: a channel without a namespace fails the grammar gate", key)
	}

	// Grammar gate before namespace scope: "beta:" is foreign AND invalid —
	// the grammar gate wins (InvalidArgument, not PermissionDenied).
	_, err := ms.api.Survey(matrixCtx(matrixAcmeKey), &serverv2.SurveyRequest{Channel: "beta:"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestAdminMatrix_GetChannels_FilteredByNamespace pins the response-side
// namespace filter: scoped keys list only their own channels (channels that
// do not parse under ns:topic are hidden too); global/static keys see all.
func TestAdminMatrix_GetChannels_FilteredByNamespace(t *testing.T) {
	ms := startMatrixServer(t, nil, nil)
	addMatrixSession(t, ms.node, matrixAcmeSess, matrixAcmeUser, matrixNsAcme)
	addMatrixSession(t, ms.node, matrixBetaSess, "matrix-beta-user", matrixNsBeta)

	// Materialize one channel per namespace through real subscriptions.
	_, err := ms.api.Subscribe(matrixCtx(matrixAcmeKey), &serverv2.SubscribeRequest{
		SessionId: matrixAcmeSess,
		Channels:  []string{matrixAcmeCh},
	})
	require.NoError(t, err)
	_, err = ms.api.Subscribe(matrixCtx(matrixStaticTok), &serverv2.SubscribeRequest{
		SessionId: matrixBetaSess,
		Channels:  []string{matrixBetaCh},
	})
	require.NoError(t, err)

	listChannels := func(key string) map[string]int32 {
		resp, err := ms.api.GetChannels(matrixCtx(key), &serverv2.GetChannelsRequest{})
		require.NoError(t, err)
		names := make(map[string]int32)
		for _, ch := range resp.GetChannels() {
			names[ch.GetName()] = ch.GetSubscribers()
		}
		return names
	}

	acmeView := listChannels(matrixAcmeKey)
	require.Contains(t, acmeView, matrixAcmeCh)
	require.NotContains(t, acmeView, matrixBetaCh, "a scoped key must not see other namespaces' channels")

	require.Contains(t, listChannels(matrixBetaKey), matrixBetaCh)
	for _, key := range []string{matrixGlobalKey, matrixStaticTok} {
		view := listChannels(key)
		require.Contains(t, view, matrixAcmeCh, "global/static keys see every channel")
		require.Contains(t, view, matrixBetaCh, "global/static keys see every channel")
	}
}

// TestAdminMatrix_DenyAll_BindsEveryIdentity pins "deny 不可打洞": an
// authorizer deny_all rule binds scoped keys exactly like static/global
// identities — no capability or namespace scope punches through it.
func TestAdminMatrix_DenyAll_BindsEveryIdentity(t *testing.T) {
	cfg := &config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{{Pattern: "acme:**", DenyAll: true}},
		},
	}
	ms := startMatrixServer(t, cfg, nil)
	addMatrixSession(t, ms.node, matrixAcmeSess, matrixAcmeUser, matrixNsAcme)

	pubReq := &serverv2.PublishRequest{
		RequestId: "denied",
		Publications: []*serverv2.Publication{{
			Id:          "denied",
			Destination: &serverv2.Publication_Destination{Channels: []string{matrixAcmeCh}},
			Payload:     &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "x"}},
		}},
	}

	// acme key: scoped, in its own namespace — still denied by the rule.
	_, err := ms.api.Publish(matrixCtx(matrixAcmeKey), pubReq)
	require.Error(t, err, "deny_all binds scoped keys (deny cannot be punched through)")
	resp, err := ms.api.Subscribe(matrixCtx(matrixAcmeKey), &serverv2.SubscribeRequest{
		SessionId: matrixAcmeSess,
		Channels:  []string{matrixAcmeCh},
	})
	require.NoError(t, err)
	requireResultsEntry(t, resp.Results, matrixAcmeCh, false)

	// static token: denied too.
	_, err = ms.api.Publish(matrixCtx(matrixStaticTok), pubReq)
	require.Error(t, err, "deny_all binds static identities as well")
}

// TestAdminMatrix_CapabilityGates pins the capability half of the chain:
// the key's own clamped bits decide (not the node ceiling) — a key without
// history.read is denied GetHistory while its other bits keep working, and
// a key without survey.bypass_gate walks the AdminDecide gate path (its
// survey is rejected by the default survey-off policy). A zero-label key
// authenticates but every capability-gated RPC denies (D24) while plain
// channel publish stays available (D23); a zero-scope key is denied
// everywhere (D5 fail-closed).
func TestAdminMatrix_CapabilityGates(t *testing.T) {
	noHistoryKey := "sk-acme-nohistory-key-9988776655443322"
	noBypassKey := "sk-acme-nobypass-key-1122334455667788"
	zeroLabelKey := "sk-acme-zerolabel-key-5566778899001122"
	zeroScopeKey := "sk-acme-zeroscope-key-aabbccddeeff0011"

	capsWithout := func(without ...string) []string {
		filtered := make([]string, 0, len(matrixFullCaps))
		for _, name := range matrixFullCaps {
			drop := false
			for _, w := range without {
				if name == w {
					drop = true
					break
				}
			}
			if !drop {
				filtered = append(filtered, name)
			}
		}
		return filtered
	}

	ms := startMatrixServer(t, nil, map[string]*proxy.AdminIdentityInfo{
		noHistoryKey: {KeyID: "key-acme-nohistory", Namespaces: []string{matrixNsAcme}, Capabilities: capsWithout("history.read"), MaxAgeSeconds: matrixMaxAgeSecs},
		noBypassKey:  {KeyID: "key-acme-nobypass", Namespaces: []string{matrixNsAcme}, Capabilities: capsWithout("survey.bypass_gate"), MaxAgeSeconds: matrixMaxAgeSecs},
	})
	addMatrixSession(t, ms.node, matrixAcmeSess, matrixAcmeUser, matrixNsAcme)

	// history.read missing → PermissionDenied; the other bits still work.
	_, err := ms.api.GetHistory(matrixCtx(noHistoryKey), &serverv2.GetHistoryRequest{Channel: matrixAcmeCh})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "a key without history.read must not read history")
	_, err = ms.api.GetPresence(matrixCtx(noHistoryKey), &serverv2.GetPresenceRequest{Channel: matrixAcmeCh})
	require.NoError(t, err, "the other capability bits of the key are untouched")

	// survey.bypass_gate missing → the survey runs through the AdminDecide
	// gate (default policy: survey off) and is rejected; the full key and
	// the static token skip the gate.
	_, err = ms.api.Survey(matrixCtx(noBypassKey), &serverv2.SurveyRequest{Channel: matrixAcmeCh})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "without bypass_gate the survey takes the gated path (survey denied by policy)")
	_, err = ms.api.Survey(matrixCtx(matrixAcmeKey), &serverv2.SurveyRequest{Channel: matrixAcmeCh})
	require.NoError(t, err, "with bypass_gate the survey skips the gate")
	_, err = ms.api.Survey(matrixCtx(matrixStaticTok), &serverv2.SurveyRequest{Channel: matrixAcmeCh})
	require.NoError(t, err, "the static token holds the ceiling including bypass_gate")

	// D24: a zero-label key authenticates but every capability-gated RPC
	// denies, while plain channel publishing stays available (D23).
	msZeroLabel := startMatrixServer(t, nil, map[string]*proxy.AdminIdentityInfo{
		zeroLabelKey: {KeyID: "key-acme-zero", Namespaces: []string{matrixNsAcme}, Capabilities: nil, MaxAgeSeconds: matrixMaxAgeSecs},
	})
	_, err = msZeroLabel.api.GetChannels(matrixCtx(zeroLabelKey), &serverv2.GetChannelsRequest{})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "a zero-label key gets zero capabilities (D24)")
	_, err = msZeroLabel.api.Publish(matrixCtx(zeroLabelKey), &serverv2.PublishRequest{
		RequestId: "zero",
		Publications: []*serverv2.Publication{{
			Id:          "zero",
			Destination: &serverv2.Publication_Destination{Channels: []string{matrixAcmeCh}},
			Payload:     &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "x"}},
		}},
	})
	require.NoError(t, err, "channel publishing is not capability-gated (D23)")

	// D5: a zero-scope key (empty namespace grant) is denied everywhere.
	msZeroScope := startMatrixServer(t, nil, map[string]*proxy.AdminIdentityInfo{
		zeroScopeKey: {KeyID: "key-acme-zeroscope", Namespaces: []string{}, Capabilities: matrixFullCaps, MaxAgeSeconds: matrixMaxAgeSecs},
	})
	_, err = msZeroScope.api.GetHistory(matrixCtx(zeroScopeKey), &serverv2.GetHistoryRequest{Channel: matrixAcmeCh})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "an empty namespace grant denies everything (fail-closed)")
}
