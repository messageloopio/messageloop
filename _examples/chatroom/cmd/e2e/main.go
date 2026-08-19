// Command e2e runs an automated end-to-end scenario against a running
// ChatRoom demo stack (backend + MessageLoop server). It exercises every
// core feature of the platform with real assertions:
//
//  1. connect/auth       4 clients + 1 invalid token rejection
//  2. subscribe/publish   publish-with-ack offsets, broadcast delivery
//  3. RPC                roll / stats / whoami via the backend
//  4. presence           snapshot queries
//  5. survey             channel poll with aggregated answers
//  6. transient          non-persisted messages reach nobody and no history
//  7. recovery           new subscriber replays channel history (recover)
//  8. admin API          server-side publish / channels / presence
//  9. ACL                private channel requires a subscription token
//  10. resume             auto-reconnect after admin kick, no message loss
//
// Start the stack first:
//
//	go run ./_examples/chatroom/cmd/backend
//	go run ./cmd/server --config ./_examples/chatroom/config.yaml
//	go run ./_examples/chatroom/cmd/e2e
//
// The process exits 0 when every assertion passes, 1 otherwise.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/messageloopio/messageloop/_examples/chatroom/internal/chatroom"
	messageloopgo "github.com/messageloopio/messageloop/sdks/go"
)

var (
	wsURL      = "ws://127.0.0.1:9080/ws"
	grpcURL    = "127.0.0.1:9090"
	adminAddr  = chatroom.DefaultAdminAddr
	adminToken = "chatroom-admin"

	mu        sync.Mutex
	passed    = 0
	failed    = 0
	stepLogs  []string
	startTime = time.Now()
)

// testClient wraps an SDK client with a thread-safe message/error log so
// assertions can poll asynchronously delivered events.
type testClient struct {
	name   string
	client messageloopgo.Client

	mu        sync.Mutex
	messages  []chatroom.ChatMessage
	raw       []string
	errors    []string
	connected bool
}

func newTestClient(name string) *testClient {
	return &testClient{name: name}
}

func (t *testClient) hook() {
	t.client.OnConnected(func(sessionID string) {
		t.mu.Lock()
		t.connected = true
		t.mu.Unlock()
		step("  [%s] connected session=%s", t.name, sessionID)
	})
	t.client.OnReconnected(func(sessionID string) {
		t.mu.Lock()
		t.connected = true
		t.mu.Unlock()
		step("  [%s] reconnected session=%s", t.name, sessionID)
	})
	t.client.OnError(func(err error) {
		t.mu.Lock()
		t.errors = append(t.errors, err.Error())
		t.mu.Unlock()
	})
	t.client.OnMessage(func(msgs []*messageloopgo.Message) {
		t.mu.Lock()
		defer t.mu.Unlock()
		for _, m := range msgs {
			var payload chatroom.ChatMessage
			if err := m.DataAs(&payload); err == nil && payload.Text != "" {
				t.messages = append(t.messages, payload)
			} else {
				t.raw = append(t.raw, m.String())
			}
		}
	})
	t.client.OnSurveyRequest(func(requestID, channel string, req *messageloopgo.Message) (*messageloopgo.Message, error) {
		return messageloopgo.NewMessageWithData("chat.poll.answer",
			messageloopgo.NewTextData("answer from "+t.name)), nil
	})
}

// hasText reports whether a chat message with the exact text was received.
func (t *testClient) hasText(text string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, m := range t.messages {
		if m.Text == text {
			return true
		}
	}
	return false
}

// hasErrorContaining reports whether an error containing substr arrived.
func (t *testClient) hasErrorContaining(substr string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.errors {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

func (t *testClient) connect(ctx context.Context, opts ...messageloopgo.Option) error {
	base := []messageloopgo.Option{
		messageloopgo.WithClientID("e2e-" + t.name),
		messageloopgo.WithClientType("e2e"),
		messageloopgo.WithToken(chatroom.TokenForName(t.name)),
		messageloopgo.WithAutoReconnect(true),
		messageloopgo.WithReconnectBackoff(300*time.Millisecond, 5*time.Second, 2.0),
		messageloopgo.WithReconnectMaxAttempts(20),
		messageloopgo.WithRPCTimeout(10 * time.Second),
	}
	opts = append(base, opts...)

	c, err := messageloopgo.Dial(wsURL, opts...)
	if err != nil {
		return err
	}
	t.client = c
	t.hook()
	return c.Connect(ctx)
}

// waitFor polls cond with a deadline and logs a failure on timeout.
func waitFor(timeout time.Duration, what string, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	check(false, "timed out waiting for %s", what)
	return false
}

// check records one assertion.
func check(ok bool, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	prefix := "PASS"
	if !ok {
		prefix = "FAIL"
		failed++
	} else {
		passed++
	}
	stepLogs = append(stepLogs, fmt.Sprintf("%s: %s", prefix, fmt.Sprintf(format, args...)))
}

// step logs a plain (non-assertion) progress line.
func step(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	stepLogs = append(stepLogs, "   | "+fmt.Sprintf(format, args...))
}

func main() {
	flag.StringVar(&wsURL, "ws-addr", wsURL, "websocket address")
	flag.StringVar(&grpcURL, "grpc-addr", grpcURL, "client gRPC address")
	flag.StringVar(&adminAddr, "admin-addr", adminAddr, "admin gRPC address")
	flag.StringVar(&adminToken, "admin-token", adminToken, "admin bearer token")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	admin, err := chatroom.NewAdminClient(ctx, adminAddr, adminToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect admin api: %v\n", err)
		os.Exit(1)
	}
	defer admin.Close()

	// Wait until the stack is reachable.
	step("waiting for the demo stack (backend + server)...")
	if !waitFor(30*time.Second, "admin api readiness", func() bool {
		_, err := admin.Channels(ctx)
		return err == nil
	}) {
		printSummary()
		os.Exit(1)
	}

	runPhase01Connect(ctx)
	runPhase02PubSub(ctx)
	runPhase03RPC(ctx)
	runPhase04Presence(ctx)
	runPhase05Survey(ctx)
	runPhase06Transient(ctx, admin)
	runPhase07Recovery(ctx)
	runPhase08AdminAPI(ctx, admin)
	runPhase09ACL(ctx)
	runPhase10Resume(ctx, admin)

	printSummary()
	if failed > 0 {
		os.Exit(1)
	}
}

func printSummary() {
	fmt.Printf("\n================ ChatRoom E2E ================\n")
	for _, l := range stepLogs {
		fmt.Println(l)
	}
	fmt.Printf("-----------------------------------------------\n")
	fmt.Printf("passed=%d failed=%d duration=%s\n", passed, failed, time.Since(startTime).Round(time.Millisecond))
	fmt.Printf("===============================================\n")
}

// ---------------------------------------------------------------------------
// Phase 1: connect + auth

func runPhase01Connect(ctx context.Context) {
	step("--- 1. connect & auth ---")

	alice := newTestClient("alice")
	if err := alice.connect(ctx); err != nil {
		check(false, "alice connect: %v", err)
		return
	}
	check(true, "alice (ws) connected, session=%s", alice.client.SessionID())

	bob := newTestClient("bob")
	bobClient, err := messageloopgo.DialGRPC(grpcURL,
		messageloopgo.WithClientID("e2e-bob"),
		messageloopgo.WithClientType("e2e"),
		messageloopgo.WithToken(chatroom.TokenForName("bob")),
		messageloopgo.WithAutoReconnect(true),
		messageloopgo.WithReconnectBackoff(300*time.Millisecond, 5*time.Second, 2.0),
	)
	if err != nil {
		check(false, "bob dial: %v", err)
		return
	}
	bob.client = bobClient
	bob.hook()
	if err := bobClient.Connect(ctx); err != nil {
		check(false, "bob (grpc) connect: %v", err)
		return
	}
	check(true, "bob (grpc) connected, session=%s", bobClient.SessionID())

	carol := newTestClient("carol")
	if err := carol.connect(ctx); err != nil {
		check(false, "carol connect: %v", err)
		return
	}
	check(true, "carol (ws) connected, session=%s", carol.client.SessionID())

	// Invalid token must be rejected at connect time.
	eve, err := messageloopgo.Dial(wsURL,
		messageloopgo.WithClientID("e2e-eve"),
		messageloopgo.WithToken("token-hacker"),
		messageloopgo.WithClientType("e2e"),
	)
	if err == nil {
		connectErr := eve.Connect(ctx)
		check(connectErr != nil, "invalid token rejected (err=%v)", connectErr)
		_ = eve.Close()
	} else {
		check(false, "eve dial failed unexpectedly: %v", err)
	}

	setGlobal("alice", alice)
	setGlobal("bob", bob)
	setGlobal("carol", carol)
}

// ---------------------------------------------------------------------------
// Phase 2: subscribe + publish

func runPhase02PubSub(ctx context.Context) {
	step("--- 2. subscribe & publish ---")
	alice, bob, carol, _ := globals()

	room := chatroom.Lobby
	if err := alice.client.Subscribe(room); err != nil {
		check(false, "alice subscribe: %v", err)
	}
	if err := bob.client.Subscribe(room); err != nil {
		check(false, "bob subscribe: %v", err)
	}
	if err := carol.client.Subscribe(room); err != nil {
		check(false, "carol subscribe: %v", err)
	}

	offset1, err := alice.client.PublishWithAck(ctx, room, chatMsg("alice", "hello from alice", "chat"))
	if err != nil {
		check(false, "alice publish: %v", err)
		return
	}
	check(offset1 > 0, "alice publish acked at offset %d", offset1)

	offset2, err := bob.client.PublishWithAck(ctx, room, chatMsg("bob", "hi from bob", "chat"))
	if err != nil {
		check(false, "bob publish: %v", err)
		return
	}
	check(offset2 > offset1, "offsets strictly increase (%d < %d)", offset1, offset2)

	waitFor(5*time.Second, "bob receives alice's message", func() bool { return bob.hasText("hello from alice") })
	check(bob.hasText("hello from alice"), "bob received alice's message")
	check(carol.hasText("hello from alice"), "carol received alice's message")
	check(alice.hasText("hi from bob"), "alice received bob's message")
}

// ---------------------------------------------------------------------------
// Phase 3: RPC via the backend

func runPhase03RPC(ctx context.Context) {
	step("--- 3. RPC (backend) ---")
	bob, _, _, _ := globals()
	room := chatroom.Lobby

	var resp messageloopgo.Message
	err := bob.client.RPC(ctx, room, "chat.roll", messageloopgo.NewMessageWithData("chat.rpc", messageloopgo.NewTextData("")), &resp)
	check(err == nil, "chat.roll rpc ok (err=%v)", err)
	check(strings.HasPrefix(resp.String(), "dice = "), "chat.roll returned a dice (got %q)", resp.String())

	resp = messageloopgo.Message{}
	err = bob.client.RPC(ctx, room, "chat.stats", messageloopgo.NewMessageWithData("chat.rpc", messageloopgo.NewTextData("")), &resp)
	check(err == nil, "chat.stats rpc ok (err=%v)", err)
	check(strings.Contains(resp.String(), room), "chat.stats mentions %s (got %q)", room, resp.String())

	resp = messageloopgo.Message{}
	err = bob.client.RPC(ctx, room, "chat.whoami", messageloopgo.NewMessageWithData("chat.rpc", messageloopgo.NewTextData("")), &resp)
	check(err == nil, "chat.whoami rpc ok (err=%v)", err)
	check(strings.Contains(resp.String(), "chat.whoami"), "chat.whoami echoes metadata (got %q)", resp.String())

	err = bob.client.RPC(ctx, room, "chat.kick", messageloopgo.NewMessageWithData("chat.rpc", messageloopgo.NewTextData("nobody")), &resp)
	check(err != nil, "chat.kick on unknown user returns error (err=%v)", err)
}

// ---------------------------------------------------------------------------
// Phase 4: presence

func runPhase04Presence(ctx context.Context) {
	step("--- 4. presence ---")
	alice, _, _, _ := globals()

	snap, err := alice.client.Presence(ctx, chatroom.Lobby)
	check(err == nil, "presence query ok (err=%v)", err)
	if err == nil {
		users := map[string]bool{}
		for _, info := range snap.Clients {
			users[info.UserID] = true
		}
		check(users["user-alice"] && users["user-bob"] && users["user-carol"],
			"presence snapshot contains alice/bob/carol (got %v)", users)
	}
}

// ---------------------------------------------------------------------------
// Phase 5: survey

func runPhase05Survey(ctx context.Context) {
	step("--- 5. survey ---")
	alice, _, _, _ := globals()

	answers, err := alice.client.Survey(ctx, chatroom.Lobby,
		messageloopgo.NewMessageWithData("chat.poll",
			messageloopgo.NewJSONData(map[string]any{"user": "alice", "kind": "poll", "text": "tea or coffee?"})),
		5*time.Second)
	check(err == nil, "survey completed (err=%v)", err)
	check(len(answers) >= 2, "survey collected >=2 answers (got %d)", len(answers))
	for _, a := range answers {
		step("  answer from %s: %s", a.UserID, a.Payload.String())
	}
}

// ---------------------------------------------------------------------------
// Phase 6: transient publish

func runPhase06Transient(ctx context.Context, admin *chatroom.AdminClient) {
	step("--- 6. transient publish ---")
	alice, bob, _, _ := globals()

	err := bob.client.Publish(chatroom.Lobby, chatMsg("bob", "invisible whisper", "whisper"), true)
	check(err == nil, "transient publish ok (err=%v)", err)

	// Transient messages ARE delivered to currently online subscribers...
	waitFor(5*time.Second, "transient delivered to online subscriber", func() bool {
		return alice.hasText("invisible whisper")
	})
	check(alice.hasText("invisible whisper"), "transient message delivered to online subscriber")

	// ...but they never enter the persisted history.
	history, err := admin.History(ctx, chatroom.Lobby, 0, 100)
	check(err == nil, "admin GetHistory ok (err=%v)", err)
	transientInHistory := false
	for _, pub := range history {
		if strings.Contains(pub.Id, "invisible-whisper") {
			transientInHistory = true
		}
	}
	check(!transientInHistory, "transient message absent from history")
}

// ---------------------------------------------------------------------------
// Phase 7: recovery for a new subscriber

func runPhase07Recovery(ctx context.Context) {
	step("--- 7. recovery (history replay) ---")
	dave := newTestClient("dave")
	if err := dave.connect(ctx); err != nil {
		check(false, "dave connect: %v", err)
		return
	}
	check(true, "dave (ws) connected")

	if err := dave.client.SubscribeWith(chatroom.Lobby, messageloopgo.WithFresh()); err != nil {
		check(false, "dave subscribe with recover: %v", err)
		return
	}
	waitFor(5*time.Second, "dave replays history", func() bool {
		return dave.hasText("hello from alice") && dave.hasText("hi from bob")
	})
	check(dave.hasText("hello from alice") && dave.hasText("hi from bob"),
		"dave recovered persisted history on subscribe")
	check(!dave.hasText("invisible whisper"),
		"transient message not replayed to recovery subscriber")
	setGlobal("dave", dave)
}

// ---------------------------------------------------------------------------
// Phase 8: admin API

func runPhase08AdminAPI(ctx context.Context, admin *chatroom.AdminClient) {
	step("--- 8. admin API ---")
	alice, bob, carol, dave := globals()

	err := admin.PublishToChannel(ctx, chatroom.Lobby, "admin-announcement",
		&chatroom.ChatMessage{User: "system", Kind: "system", Text: "announcement from admin"}, true)
	check(err == nil, "admin Publish ok (err=%v)", err)

	waitFor(5*time.Second, "everyone receives the admin message", func() bool {
		return alice.hasText("announcement from admin") && bob.hasText("announcement from admin") &&
			carol.hasText("announcement from admin") && dave.hasText("announcement from admin")
	})
	check(alice.hasText("announcement from admin") && bob.hasText("announcement from admin") &&
		carol.hasText("announcement from admin") && dave.hasText("announcement from admin"),
		"admin message delivered to all 4 subscribers")

	channels, err := admin.Channels(ctx)
	check(err == nil, "admin GetChannels ok (err=%v)", err)
	found := false
	for _, ch := range channels {
		if ch.Name == chatroom.Lobby {
			found = true
		}
	}
	check(found, "admin GetChannels lists %s", chatroom.Lobby)

	presence, err := admin.Presence(ctx, chatroom.Lobby)
	check(err == nil, "admin GetPresence ok (err=%v)", err)
	check(len(presence) >= 4, "admin GetPresence sees >=4 clients (got %d)", len(presence))
}

// ---------------------------------------------------------------------------
// Phase 9: ACL

func runPhase09ACL(ctx context.Context) {
	step("--- 9. ACL (private channel) ---")
	alice, _, _, _ := globals()
	private := "private:alice-bob"

	// An anonymous client (no token) must be denied subscription.
	anon := newTestClient("anon")
	anonClient, err := messageloopgo.Dial(wsURL,
		messageloopgo.WithClientID("e2e-anon"),
		messageloopgo.WithClientType("e2e"),
		messageloopgo.WithToken(""),
	)
	if err != nil {
		check(false, "anon dial: %v", err)
		return
	}
	anon.client = anonClient
	anon.hook()
	if err := anonClient.Connect(ctx); err != nil {
		check(false, "anon connect: %v", err)
		return
	}
	check(true, "anonymous client connected (auth is optional)")

	if err := anonClient.Subscribe(private); err != nil {
		check(false, "anon subscribe send: %v", err)
		return
	}
	waitFor(5*time.Second, "anonymous subscribe denied by ACL", func() bool {
		return anon.hasErrorContaining("PROXY_ERROR")
	})
	check(anon.hasErrorContaining("PROXY_ERROR"), "anonymous subscribe denied (PROXY_ERROR)")
	_ = anonClient.Close()

	// An authenticated client with a per-subscription token is allowed.
	err = alice.client.SubscribeWith(private, messageloopgo.WithSubscriptionToken(chatroom.TokenForName("alice")))
	check(err == nil, "alice subscribe private with token ok (err=%v)", err)
	time.Sleep(500 * time.Millisecond)
	check(!alice.hasErrorContaining("PERMISSION_DENIED"), "alice not denied on private channel")
	_ = alice.client.Unsubscribe(private)
}

// ---------------------------------------------------------------------------
// Phase 10: session resume after admin kick

func runPhase10Resume(ctx context.Context, admin *chatroom.AdminClient) {
	step("--- 10. resume after disconnect ---")
	alice, bob, _, _ := globals()

	results, err := admin.DisconnectUser(ctx, "user-alice", 3400, "e2e phase 10")
	check(err == nil, "admin Disconnect ok (err=%v)", err)
	check(len(results) > 0, "admin Disconnect targeted alice's session (results=%v)", results)

	// Give the disconnect a moment to propagate, then publish while alice is
	// (possibly still) offline. Alice's SDK auto-reconnects with its
	// subscription recovery offsets and must not lose this message.
	time.Sleep(500 * time.Millisecond)
	offset, err := bob.client.PublishWithAck(ctx, chatroom.Lobby, chatMsg("bob", "during alice outage", "chat"))
	check(err == nil && offset > 0, "bob published during outage (offset=%d err=%v)", offset, err)

	// Alice's SDK auto-reconnects with subscription + recovery offsets.
	waitFor(15*time.Second, "alice auto-reconnects and recovers the missed message", func() bool {
		return alice.hasText("during alice outage")
	})
	check(alice.hasText("during alice outage"), "alice recovered the message published while offline")
}

// ---------------------------------------------------------------------------
// helpers

var globalsMu sync.Mutex
var globalsMap = map[string]*testClient{}

func setGlobal(name string, c *testClient) {
	globalsMu.Lock()
	globalsMap[name] = c
	globalsMu.Unlock()
}

func globals() (*testClient, *testClient, *testClient, *testClient) {
	globalsMu.Lock()
	defer globalsMu.Unlock()
	return globalsMap["alice"], globalsMap["bob"], globalsMap["carol"], globalsMap["dave"]
}

// chatMsg builds a JSON chat message payload.
func chatMsg(user, text, kind string) *messageloopgo.Message {
	return messageloopgo.NewMessageWithData("chat.message",
		messageloopgo.NewJSONData(map[string]any{"user": user, "text": text, "kind": kind}))
}
