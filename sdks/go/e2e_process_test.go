package messageloopgo

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// This file is the two-process black-box smoke e2e (PR-KA-D9): the test builds
// the real cmd/server binary, spawns it as a child process with a generated
// config on pre-allocated free ports, and drives it with the real Go SDK over
// real sockets. It backs every contract wired in cmd/server: config loading,
// listener setup, the D2 version gate (the SDK default version "2.0.0"), the
// WS and gRPC transports, history/recovery, and the admin gRPC API.
//
// All synchronization is done via readiness polling and message waits; there
// are no fixed sleeps. The child process is always killed and reaped via
// t.Cleanup, even when an assertion fails.

const (
	// e2eHealthTimeout bounds the /health readiness poll of the spawned server.
	e2eHealthTimeout = 15 * time.Second
	// e2eStepTimeout bounds every individual message wait / RPC of a scenario.
	e2eStepTimeout = 10 * time.Second
	// e2eRedisDB is the dedicated Redis logical DB for the broker-redis variant
	// (DB 14 and 15 are used by other integration suites).
	e2eRedisDB = 13
)

// e2eServerProcess describes a spawned cmd/server child process.
type e2eServerProcess struct {
	wsURL      string
	grpcAddr   string
	adminAddr  string
	adminToken string
	pid        int
}

// e2eLogBuffer is a goroutine-safe buffer collecting the child process output
// so it can be dumped on failure.
type e2eLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *e2eLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *e2eLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestE2EProcess builds the real server binary once and runs the black-box
// scenarios against it: the memory broker is the default path and always runs;
// the Redis variant re-runs the WS full flow against broker.type=redis when a
// real Redis is reachable (CI provides one; locally it skips otherwise).
func TestE2EProcess(t *testing.T) {
	bin := buildE2EServerBinary(t)

	t.Run("MemoryBroker", func(t *testing.T) {
		srv := startE2EServer(t, bin, "memory", "", "")
		runE2EScenarios(t, srv)
	})

	t.Run("RedisBroker", func(t *testing.T) {
		redisAddr, redisPassword := requireE2ERedis(t)
		srv := startE2EServer(t, bin, "redis", redisAddr, redisPassword)
		// Spec scenario 6: re-run the WS full flow to prove the real
		// Stream/PubSub path.
		runE2EWSFlow(t, srv, e2eNamespace()+".chat")
	})
}

// runE2EScenarios runs scenarios 2-5 of the spec against one spawned server:
// WS full flow, history replay, gRPC transport, admin gRPC smoke. Clients stay
// connected for the whole function so the admin checks observe live state.
func runE2EScenarios(t *testing.T, srv *e2eServerProcess) {
	ns := e2eNamespace()
	chatCh := ns + ".chat"
	histCh := ns + ".hist"
	grpcCh := ns + ".grpc"

	// Scenario 2: WS full flow (connect + subscribe + publish + receive). The
	// subscriber stays connected until the end of the function (admin smoke
	// below asserts its presence).
	runE2EWSFlow(t, srv, chatCh)

	// Scenario 3: history / recovery. Two persisted publishes (ack-ordered),
	// then a fresh subscriber replays them from the start in order.
	pub := dialE2EWS(t, srv)
	connectE2E(t, pub)
	t.Cleanup(func() { _ = pub.Close() })

	histPayloads := []string{"e2e-hist-1", "e2e-hist-2"}
	for _, text := range histPayloads {
		ctx, cancel := context.WithTimeout(context.Background(), e2eStepTimeout)
		_, err := pub.PublishWithAck(ctx, histCh, NewMessageWithData("e2e.test", NewTextData(text)))
		cancel()
		if err != nil {
			t.Fatalf("PublishWithAck(%q) failed: %v", text, err)
		}
	}

	freshReceived := make(chan *Message, 8)
	fresh := dialE2EWS(t, srv)
	fresh.OnMessage(collectE2EChannel(freshReceived, histCh))
	connectE2E(t, fresh)
	t.Cleanup(func() { _ = fresh.Close() })
	if err := fresh.SubscribeWith(histCh, WithFresh()); err != nil {
		t.Fatalf("SubscribeWith(WithFresh) failed: %v", err)
	}
	for i, want := range histPayloads {
		msg := waitE2EMessage(t, freshReceived, fmt.Sprintf("replayed history message %d", i))
		if got := msg.Data.AsText(); got != want {
			t.Fatalf("replayed message %d payload = %q, want %q (in-order replay)", i, got, want)
		}
	}

	// Scenario 4: gRPC streaming transport: connect/subscribe/publish/receive
	// entirely over the second transport.
	grpcReceived := make(chan *Message, 8)
	gsub, err := DialGRPC(srv.grpcAddr, WithAutoSubscribe(grpcCh))
	if err != nil {
		t.Fatalf("DialGRPC failed: %v", err)
	}
	gsub.OnMessage(collectE2EChannel(grpcReceived, grpcCh))
	connectE2E(t, gsub)
	t.Cleanup(func() { _ = gsub.Close() })

	grpcPayload := []byte("e2e-grpc-payload\x00\x01")
	if err := gsub.Publish(grpcCh, NewMessageWithData("e2e.test", NewBinaryData(grpcPayload))); err != nil {
		t.Fatalf("gRPC publish failed: %v", err)
	}
	if msg := waitE2EMessage(t, grpcReceived, "gRPC-delivered message"); !bytes.Equal(msg.Data.AsBinary(), grpcPayload) {
		t.Fatalf("gRPC payload = %v, want %v (byte-equal)", msg.Data.AsBinary(), grpcPayload)
	}

	// Scenario 5: admin gRPC smoke with Bearer auth: GetChannels lists the
	// live channels, GetPresence reports at least one online session.
	runE2EAdminSmoke(t, srv, chatCh, grpcCh)
}

// runE2EWSFlow connects a WS SDK client subscribed to channel, publishes one
// binary message from a second WS client, and asserts byte-equal delivery.
// Both clients are kept open by cleanup.
func runE2EWSFlow(t *testing.T, srv *e2eServerProcess, channel string) {
	t.Helper()

	received := make(chan *Message, 8)
	sub := dialE2EWS(t, srv, WithAutoSubscribe(channel))
	sub.OnMessage(collectE2EChannel(received, channel))
	connectE2E(t, sub)
	t.Cleanup(func() { _ = sub.Close() })

	pub := dialE2EWS(t, srv)
	connectE2E(t, pub)
	t.Cleanup(func() { _ = pub.Close() })

	payload := []byte("e2e-ws-payload\x00\x01" + channel)
	if err := pub.Publish(channel, NewMessageWithData("e2e.test", NewBinaryData(payload))); err != nil {
		t.Fatalf("WS publish failed: %v", err)
	}
	if msg := waitE2EMessage(t, received, "WS-delivered message"); !bytes.Equal(msg.Data.AsBinary(), payload) {
		t.Fatalf("WS payload = %v, want %v (byte-equal)", msg.Data.AsBinary(), payload)
	}
}

// runE2EAdminSmoke calls GetChannels and GetPresence on the admin gRPC API
// with Bearer-token metadata and asserts the live channels and sessions.
func runE2EAdminSmoke(t *testing.T, srv *e2eServerProcess, wantChannels ...string) {
	t.Helper()

	conn, err := grpc.NewClient(srv.adminAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("admin gRPC dial failed: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), e2eStepTimeout)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+srv.adminToken)

	admin := serverv2.NewAPIServiceClient(conn)
	channelsResp, err := admin.GetChannels(ctx, &serverv2.GetChannelsRequest{})
	if err != nil {
		t.Fatalf("admin GetChannels failed: %v", err)
	}
	seen := make(map[string]bool, len(channelsResp.GetChannels()))
	for _, ch := range channelsResp.GetChannels() {
		seen[ch.GetName()] = true
	}
	for _, want := range wantChannels {
		if !seen[want] {
			t.Fatalf("admin GetChannels missing %q, got %v", want, seen)
		}
	}

	presenceResp, err := admin.GetPresence(ctx, &serverv2.GetPresenceRequest{Channel: wantChannels[0]})
	if err != nil {
		t.Fatalf("admin GetPresence failed: %v", err)
	}
	if len(presenceResp.GetClients()) < 1 {
		t.Fatalf("admin GetPresence(%q) returned no clients, want at least one online session", wantChannels[0])
	}
}

// buildE2EServerBinary compiles cmd/server from the repository root into a
// temp binary and returns its path (.exe on Windows).
func buildE2EServerBinary(t *testing.T) string {
	t.Helper()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	name := "messageloop-e2e-server"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binPath := filepath.Join(t.TempDir(), name)

	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/server")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/server failed: %v\n%s", err, out)
	}
	t.Logf("built server binary: %s", binPath)
	return binPath
}

// startE2EServer writes a generated config (four pre-allocated free ports),
// spawns the server binary, and polls /health until ready (15s cap). The
// process is killed and reaped by t.Cleanup even when assertions fail.
// brokerType is "memory" or "redis"; the redis variant needs addr/password.
func startE2EServer(t *testing.T, binPath, brokerType, redisAddr, redisPassword string) *e2eServerProcess {
	t.Helper()

	httpAddr := e2eFreeAddr(t)
	wsAddr := e2eFreeAddr(t)
	srv := &e2eServerProcess{
		wsURL:      "ws://" + wsAddr + "/ws",
		grpcAddr:   e2eFreeAddr(t),
		adminAddr:  e2eFreeAddr(t),
		adminToken: e2eAdminToken(),
	}

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "e2e-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(e2eConfigYAML(httpAddr, srv.adminAddr, srv.adminToken, wsAddr, srv.grpcAddr, brokerType, redisAddr, redisPassword)), 0o600); err != nil {
		t.Fatalf("write server config: %v", err)
	}

	logs := &e2eLogBuffer{}
	cmd := exec.Command(binPath, "--config", cfgPath, "--log-level", "warn")
	cmd.Dir = cfgDir
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server process: %v", err)
	}
	srv.pid = cmd.Process.Pid

	// Reap in exactly one goroutine; cleanup kills then waits for the reaper.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})

	t.Logf("spawned server pid=%d broker=%s ws=%s grpc=%s admin=%s http=%s",
		srv.pid, brokerType, wsAddr, srv.grpcAddr, srv.adminAddr, httpAddr)

	healthURL := "http://" + httpAddr + "/health"
	healthClient := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(e2eHealthTimeout)
	for {
		select {
		case err := <-exited:
			t.Fatalf("server exited before becoming ready: %v\nserver logs:\n%s", err, logs.String())
		default:
		}
		resp, err := healthClient.Get(healthURL)
		if err == nil {
			code := resp.StatusCode
			_ = resp.Body.Close()
			if code == http.StatusOK {
				return srv
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server not healthy within %s (%s)\nserver logs:\n%s", e2eHealthTimeout, healthURL, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// e2eConfigYAML renders a full server config for the given listeners and
// broker. The admin token is always set (admin auth is mandatory by config
// validation). The redis variant selects the dedicated e2e logical DB and the
// required stream_approximate flag.
func e2eConfigYAML(httpAddr, adminAddr, adminToken, wsAddr, grpcAddr, brokerType, redisAddr, redisPassword string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "server:\n  http:\n    addr: %q\n  grpc_admin:\n    addr: %q\n    auth_token: %q\n",
		httpAddr, adminAddr, adminToken)
	fmt.Fprintf(&b, "transport:\n  websocket:\n    addr: %q\n    path: \"/ws\"\n  grpc:\n    addr: %q\n",
		wsAddr, grpcAddr)
	if brokerType == "redis" {
		fmt.Fprintf(&b, "broker:\n  type: redis\n  redis:\n    addr: %q\n    db: %d\n    stream_approximate: true\n",
			redisAddr, e2eRedisDB)
		if redisPassword != "" {
			fmt.Fprintf(&b, "    password: %q\n", redisPassword)
		}
	} else {
		b.WriteString("broker:\n  type: memory\n")
	}
	return b.String()
}

// e2eFreeAddr reserves a free loopback TCP port by binding :0 and releasing
// it; no port is ever hard-coded.
func e2eFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

// e2eAdminToken generates a per-run admin token.
func e2eAdminToken() string {
	return "e2e-admin-" + uuid.NewString()
}

// e2eNamespace returns a run-unique channel prefix so repeated runs (and the
// Redis variant sharing one Redis) never observe each other's state.
func e2eNamespace() string {
	return "e2e." + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
}

// dialE2EWS dials the spawned server's WebSocket endpoint with the SDK.
func dialE2EWS(t *testing.T, srv *e2eServerProcess, opts ...Option) Client {
	t.Helper()
	c, err := Dial(srv.wsURL, opts...)
	if err != nil {
		t.Fatalf("SDK Dial(%s) failed: %v", srv.wsURL, err)
	}
	return c
}

// connectE2E connects the client, failing the test on error. The SDK default
// version "2.0.0" implicitly backs the D2 generation gate.
func connectE2E(t *testing.T, c Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), e2eStepTimeout)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
}

// collectE2EChannel returns an OnMessage handler forwarding messages of one
// channel into ch (non-blocking; ch is sized for the scenario).
func collectE2EChannel(ch chan<- *Message, channel string) func([]*Message) {
	return func(msgs []*Message) {
		for _, m := range msgs {
			if m.GetMetadata("channel") != channel {
				continue
			}
			select {
			case ch <- m:
			default:
			}
		}
	}
}

// waitE2EMessage waits up to e2eStepTimeout for one message from ch.
func waitE2EMessage(t *testing.T, ch <-chan *Message, what string) *Message {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(e2eStepTimeout):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// requireE2ERedis probes a real Redis (env MESSAGELOOP_TEST_REDIS_ADDR,
// default 127.0.0.1:6379) and skips the test when none answers PING. Password
// comes from MESSAGELOOP_TEST_REDIS_PASSWORD, falling back to REDIS_PASSWORD.
func requireE2ERedis(t *testing.T) (addr, password string) {
	t.Helper()

	addr = os.Getenv("MESSAGELOOP_TEST_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	password = os.Getenv("MESSAGELOOP_TEST_REDIS_PASSWORD")
	if password == "" {
		password = os.Getenv("REDIS_PASSWORD")
	}

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Skipf("no Redis at %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	if password != "" {
		if _, err := fmt.Fprintf(conn, "AUTH %s\r\n", password); err != nil {
			t.Skipf("Redis AUTH write failed: %v", err)
		}
		reply := make([]byte, 256)
		n, err := conn.Read(reply)
		if err != nil || !strings.HasPrefix(string(reply[:n]), "+OK") {
			t.Skipf("Redis AUTH failed: err=%v reply=%q", err, reply[:max(n, 0)])
		}
	}
	if _, err := conn.Write([]byte("PING\r\n")); err != nil {
		t.Skipf("Redis PING write failed: %v", err)
	}
	reply := make([]byte, 256)
	n, err := conn.Read(reply)
	if err != nil || !strings.HasPrefix(string(reply[:n]), "+PONG") {
		t.Skipf("Redis at %s did not answer PING: err=%v reply=%q", addr, err, reply[:max(n, 0)])
	}
	return addr, password
}
