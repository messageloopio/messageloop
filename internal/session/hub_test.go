package session

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/pkg/topics"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// mockTransport is a mock implementation of Transport for testing
type mockTransport struct {
	mu          sync.Mutex
	closed      bool
	messages    [][]byte
	closeCount  int
	closeReason Disconnect
	sendErr     error
}

func (m *mockTransport) Write(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("transport closed")
	}
	if m.sendErr != nil {
		return m.sendErr
	}
	m.messages = append(m.messages, data)
	return nil
}

func (m *mockTransport) WriteMany(data ...[]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("transport closed")
	}
	m.messages = append(m.messages, data...)
	return nil
}

func (m *mockTransport) Close(disconnect Disconnect) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.closeCount++
	m.closeReason = disconnect
	return nil
}

func (m *mockTransport) RemoteAddr() string {
	return "127.0.0.1:12345"
}

func (m *mockTransport) getMessageCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.messages)
}

func (m *mockTransport) getMessage(i int) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.messages) {
		return nil
	}
	return m.messages[i]
}

func newTestClient(t *testing.T, sessionID, userID string) *Session {
	return newTestClientWithTransport(t, sessionID, userID, &mockTransport{})
}

func newTestClientWithTransport(t *testing.T, sessionID, userID string, transport Transport) *Session {
	ctx := context.Background()
	node := newFakeRuntime()
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	// Set the user and client fields for testing
	client.mu.Lock()
	client.session = sessionID
	client.user = userID
	client.client = "client-" + sessionID
	client.mu.Unlock()
	// Attach the initial attachment so the writer goroutine drains the send
	// queue: tests send and then assert on the transport synchronously.
	if err := client.Attach(client.attachment); err != nil {
		t.Fatalf("Failed to attach test client: %v", err)
	}
	return client
}

func TestNewHub(t *testing.T) {
	h := newHub(0, 0)
	if h == nil {
		t.Fatal("newHub() should not return nil")
	}
	if len(h.connShards) != numHubShards {
		t.Errorf("len(connShards) = %d, want %d", len(h.connShards), numHubShards)
	}
	if len(h.subShards) != numHubShards {
		t.Errorf("len(subShards) = %d, want %d", len(h.subShards), numHubShards)
	}
	if h.sessions == nil {
		t.Error("sessions map should be initialized")
	}
}

func TestNewConnShard(t *testing.T) {
	shard := newConnShard()
	if shard == nil {
		t.Fatal("newConnShard() should not return nil")
	}
	if shard.clients == nil {
		t.Error("clients map should be initialized")
	}
	if shard.users == nil {
		t.Error("users map should be initialized")
	}
}

func TestNewSubShard(t *testing.T) {
	const maxTimeLag = 1000
	shard := newSubShard(maxTimeLag)
	if shard == nil {
		t.Fatal("newSubShard() should not return nil")
	}
	if shard.subs == nil {
		t.Error("subs map should be initialized")
	}
	if shard.maxTimeLagMilli != maxTimeLag {
		t.Errorf("maxTimeLagMilli = %d, want %d", shard.maxTimeLagMilli, maxTimeLag)
	}
}

func TestConnShard_Add(t *testing.T) {
	shard := newConnShard()
	client := newTestClient(t, "session-1", "user-1")

	_ = shard.addWithLimit(client, 0)

	shard.mu.RLock()
	defer shard.mu.RUnlock()

	if len(shard.clients) != 1 {
		t.Errorf("len(clients) = %d, want 1", len(shard.clients))
	}
	if _, ok := shard.clients["session-1"]; !ok {
		t.Error("client session-1 should be in clients map")
	}
	if len(shard.users) != 1 {
		t.Errorf("len(users) = %d, want 1", len(shard.users))
	}
	if _, ok := shard.users["user-1"]; !ok {
		t.Error("user-1 should be in users map")
	}
	if _, ok := shard.users["user-1"]["session-1"]; !ok {
		t.Error("session-1 should be in user-1's session set")
	}
}

func TestConnShard_Add_MultipleSessionsSameUser(t *testing.T) {
	shard := newConnShard()
	client1 := newTestClient(t, "session-1", "user-1")
	client2 := newTestClient(t, "session-2", "user-1")

	_ = shard.addWithLimit(client1, 0)
	_ = shard.addWithLimit(client2, 0)

	shard.mu.RLock()
	defer shard.mu.RUnlock()

	if len(shard.clients) != 2 {
		t.Errorf("len(clients) = %d, want 2", len(shard.clients))
	}
	if len(shard.users) != 1 {
		t.Errorf("len(users) = %d, want 1", len(shard.users))
	}
	if len(shard.users["user-1"]) != 2 {
		t.Errorf("user-1 should have 2 sessions, got %d", len(shard.users["user-1"]))
	}
}

func TestConnShard_Add_MultipleUsers(t *testing.T) {
	shard := newConnShard()
	client1 := newTestClient(t, "session-1", "user-1")
	client2 := newTestClient(t, "session-2", "user-2")

	_ = shard.addWithLimit(client1, 0)
	_ = shard.addWithLimit(client2, 0)

	shard.mu.RLock()
	defer shard.mu.RUnlock()

	if len(shard.clients) != 2 {
		t.Errorf("len(clients) = %d, want 2", len(shard.clients))
	}
	if len(shard.users) != 2 {
		t.Errorf("len(users) = %d, want 2", len(shard.users))
	}
}

func TestSubShard_NumSubscribers_Empty(t *testing.T) {
	shard := newSubShard(0)
	count := shard.NumSubscribers("test-channel")
	if count != 0 {
		t.Errorf("NumSubscribers() = %d, want 0", count)
	}
}

func TestSubShard_NumSubscribers(t *testing.T) {
	shard := newSubShard(0)
	client1 := newTestClient(t, "session-1", "user-1")
	client2 := newTestClient(t, "session-2", "user-2")

	_, _ = shard.addSub("test-channel", Subscriber{Session: client1, Ephemeral: false})
	_, _ = shard.addSub("test-channel", Subscriber{Session: client2, Ephemeral: true})

	count := shard.NumSubscribers("test-channel")
	if count != 2 {
		t.Errorf("NumSubscribers() = %d, want 2", count)
	}
}

func TestSubShard_AddSub_NewChannel(t *testing.T) {
	shard := newSubShard(0)
	client := newTestClient(t, "session-1", "user-1")

	first, err := shard.addSub("test-channel", Subscriber{Session: client, Ephemeral: false})
	if err != nil {
		t.Fatalf("addSub() error = %v", err)
	}
	if !first {
		t.Error("addSub() should return true for first subscriber")
	}

	count := shard.NumSubscribers("test-channel")
	if count != 1 {
		t.Errorf("NumSubscribers() = %d, want 1", count)
	}
}

func TestSubShard_AddSub_ExistingChannel(t *testing.T) {
	shard := newSubShard(0)
	client1 := newTestClient(t, "session-1", "user-1")
	client2 := newTestClient(t, "session-2", "user-2")

	_, _ = shard.addSub("test-channel", Subscriber{Session: client1, Ephemeral: false})

	first, err := shard.addSub("test-channel", Subscriber{Session: client2, Ephemeral: false})
	if err != nil {
		t.Fatalf("addSub() error = %v", err)
	}
	if first {
		t.Error("addSub() should return false for subsequent subscribers")
	}

	count := shard.NumSubscribers("test-channel")
	if count != 2 {
		t.Errorf("NumSubscribers() = %d, want 2", count)
	}
}

func TestSubShard_RemoveSub_NotFound(t *testing.T) {
	shard := newSubShard(0)
	client := newTestClient(t, "session-1", "user-1")

	empty, found := shard.removeSub("test-channel", client)
	if !empty {
		t.Error("removeSub() should return true for empty (first return value)")
	}
	if found {
		t.Error("removeSub() should return false for not found (second return value)")
	}
}

func TestSubShard_RemoveSub_Success(t *testing.T) {
	shard := newSubShard(0)
	client := newTestClient(t, "session-1", "user-1")

	_, _ = shard.addSub("test-channel", Subscriber{Session: client, Ephemeral: false})

	// Remove the subscription
	empty, found := shard.removeSub("test-channel", client)
	if !empty {
		t.Error("removeSub() should return true for empty after removing last subscriber")
	}
	if !found {
		t.Error("removeSub() should return true for found")
	}

	// Verify channel was removed
	count := shard.NumSubscribers("test-channel")
	if count != 0 {
		t.Errorf("NumSubscribers() = %d, want 0 after removal", count)
	}
}

func TestSubShard_RemoveSub_OneOfMany(t *testing.T) {
	shard := newSubShard(0)
	client1 := newTestClient(t, "session-1", "user-1")
	client2 := newTestClient(t, "session-2", "user-2")

	_, _ = shard.addSub("test-channel", Subscriber{Session: client1, Ephemeral: false})
	_, _ = shard.addSub("test-channel", Subscriber{Session: client2, Ephemeral: false})

	// Remove one subscription
	empty, found := shard.removeSub("test-channel", client1)
	if empty {
		t.Error("removeSub() should return false for empty when other subscribers remain")
	}
	if !found {
		t.Error("removeSub() should return true for found")
	}

	count := shard.NumSubscribers("test-channel")
	if count != 1 {
		t.Errorf("NumSubscribers() = %d, want 1", count)
	}
}

func TestHub_Add(t *testing.T) {
	h := newHub(0, 0)
	client := newTestClient(t, "session-1", "user-1")

	_ = h.Add(client)
	h.mu.RLock()
	if len(h.sessions) != 1 {
		h.mu.RUnlock()
		t.Errorf("len(sessions) = %d, want 1", len(h.sessions))
	}
	if _, ok := h.sessions["session-1"]; !ok {
		h.mu.RUnlock()
		t.Error("session-1 should be in sessions map")
	}
	h.mu.RUnlock()

	// Check connShard
	shardIdx := index("user-1", numHubShards)
	shard := h.connShards[shardIdx]
	shard.mu.RLock()
	if len(shard.clients) != 1 {
		shard.mu.RUnlock()
		t.Errorf("len(connShard.clients) = %d, want 1", len(shard.clients))
	}
	shard.mu.RUnlock()
}

func TestHub_NumSubscribers(t *testing.T) {
	h := newHub(0, 0)
	client1 := newTestClient(t, "session-1", "user-1")
	client2 := newTestClient(t, "session-2", "user-2")

	shardIdx := index("test-channel", numHubShards)
	shard := h.subShards[shardIdx]
	_, _ = shard.addSub("test-channel", Subscriber{Session: client1, Ephemeral: false})
	_, _ = shard.addSub("test-channel", Subscriber{Session: client2, Ephemeral: false})

	count := h.NumSubscribers("test-channel")
	if count != 2 {
		t.Errorf("NumSubscribers() = %d, want 2", count)
	}
}

func TestHub_AddSub(t *testing.T) {
	h := newHub(0, 0)
	client := newTestClient(t, "session-1", "user-1")

	first, err := h.AddSub("test-channel", Subscriber{Session: client, Ephemeral: false})
	if err != nil {
		t.Fatalf("addSub() error = %v", err)
	}
	if !first {
		t.Error("addSub() should return true for first subscriber")
	}

	count := h.NumSubscribers("test-channel")
	if count != 1 {
		t.Errorf("NumSubscribers() = %d, want 1", count)
	}
}

func TestHub_RemoveSub(t *testing.T) {
	h := newHub(0, 0)
	client := newTestClient(t, "session-1", "user-1")

	_, _ = h.AddSub("test-channel", Subscriber{Session: client, Ephemeral: false})

	empty, found := h.RemoveSub("test-channel", client)
	if !empty {
		t.Error("removeSub() should return true for empty")
	}
	if !found {
		t.Error("removeSub() should return true for found")
	}

	count := h.NumSubscribers("test-channel")
	if count != 0 {
		t.Errorf("NumSubscribers() = %d, want 0", count)
	}
}

// TestHub_BroadcastPublication covers the Hub-level broadcast (the former
// subShard-level duplicate implementation was removed); see
// TestHub_BroadcastPublication below for the subscriber-delivery assertions.
func TestHub_BroadcastPublication_NoSubscribers(t *testing.T) {
	h := newHub(0, 0)

	pub := &Publication{
		Channel: "test-channel",
		Offset:  1,
		Payload: []byte("test payload"),
		Time:    time.Now().UnixMilli(),
	}

	err := h.BroadcastPublication("test-channel", pub)
	if err != nil {
		t.Fatalf("broadcastPublication() error = %v", err)
	}
}

func TestHub_BroadcastPublication_ShardLevelNoSubscribers(t *testing.T) {
	h := newHub(0, 0)

	pub := &Publication{
		Channel: "test-channel",
		Offset:  1,
		Payload: []byte("test payload"),
		Time:    time.Now().UnixMilli(),
	}

	err := h.BroadcastPublication("test-channel", pub)
	if err != nil {
		t.Fatalf("broadcastPublication() error = %v", err)
	}
}

func TestHub_BroadcastPublication(t *testing.T) {
	h := newHub(0, 0)

	transport1 := &mockTransport{}
	transport2 := &mockTransport{}
	client1 := newTestClientWithTransport(t, "session-1", "user-1", transport1)
	client2 := newTestClientWithTransport(t, "session-2", "user-2", transport2)

	_, _ = h.AddSub("test-channel", Subscriber{Session: client1, Ephemeral: false})
	_, _ = h.AddSub("test-channel", Subscriber{Session: client2, Ephemeral: false})

	pub := &Publication{
		Channel: "test-channel",
		Offset:  1,
		Payload: []byte("test payload"),
		Time:    time.Now().UnixMilli(),
	}

	err := h.BroadcastPublication("test-channel", pub)
	if err != nil {
		t.Fatalf("broadcastPublication() error = %v", err)
	}

	if transport1.getMessageCount() != 1 {
		t.Errorf("client1 received %d messages, want 1", transport1.getMessageCount())
	}
	if transport2.getMessageCount() != 1 {
		t.Errorf("client2 received %d messages, want 1", transport2.getMessageCount())
	}
}

// countingMarshaler wraps a Marshaler and counts MarshalAppend calls, to
// pin the "one marshal per encoding per broadcast" contract (C2) — byte
// equality of frames alone cannot distinguish one shared marshal from N
// identical marshals.
type countingMarshaler struct {
	Marshaler
	appends atomic.Int32
}

func (c *countingMarshaler) MarshalAppend(buf []byte, msg any) ([]byte, error) {
	c.appends.Add(1)
	return c.Marshaler.MarshalAppend(buf, msg)
}

// TestHub_BroadcastPublication_MixedEncodings covers the per-encoding
// serialization of the broadcast path: subscribers sharing a marshaler receive
// identical frame bytes, each frame decodes correctly with its own encoding,
// and each distinct encoding pays exactly one MarshalAppend per broadcast.
// The JSON subscribers use the production ProtoJSONMarshaler (the WS/QUIC
// JSON wire), not the test-only JSONMarshaler.
func TestHub_BroadcastPublication_MixedEncodings(t *testing.T) {
	h := newHub(0, 0)

	newEncodedClient := func(sessionID string, m Marshaler) (*Session, *mockTransport) {
		transport := &mockTransport{}
		client, _, err := NewClient(context.Background(), newFakeRuntime(), transport, m)
		require.NoError(t, err)
		client.mu.Lock()
		client.session = sessionID
		client.user = "user-" + sessionID
		client.client = "client-" + sessionID
		client.mu.Unlock()
		// Attach so the writer goroutine drains the send queue: the broadcast
		// waits on each frame's done channel, so writes land synchronously.
		require.NoError(t, client.Attach(client.attachment))
		return client, transport
	}

	jsonClient1, jsonTransport1 := newEncodedClient("json-1", ProtoJSONMarshaler)
	jsonClient2, jsonTransport2 := newEncodedClient("json-2", ProtoJSONMarshaler)
	counting := &countingMarshaler{Marshaler: ProtoJSONMarshaler}
	countClient1, countTransport1 := newEncodedClient("count-1", counting)
	countClient2, countTransport2 := newEncodedClient("count-2", counting)
	protoClient, protoTransport := newEncodedClient("proto-1", ProtobufMarshaler{})

	for _, client := range []*Session{jsonClient1, jsonClient2, countClient1, countClient2, protoClient} {
		_, _ = h.AddSub("test-channel", Subscriber{Session: client, Ephemeral: false})
	}

	pub := &Publication{
		Channel: "test-channel",
		Offset:  1,
		Payload: []byte("test payload"),
		Time:    time.Now().UnixMilli(),
	}
	require.NoError(t, h.BroadcastPublication("test-channel", pub))

	// Each subscriber received exactly one frame.
	require.Equal(t, 1, jsonTransport1.getMessageCount())
	require.Equal(t, 1, jsonTransport2.getMessageCount())
	require.Equal(t, 1, protoTransport.getMessageCount())

	// One MarshalAppend for the counting encoding, though two subscribers
	// share it; a second broadcast adds exactly one more.
	assert.EqualValues(t, 1, counting.appends.Load(),
		"two subscribers of one encoding must share a single marshal")
	require.NoError(t, h.BroadcastPublication("test-channel", &Publication{
		Channel: "test-channel",
		Offset:  2,
		Payload: []byte("second"),
		Time:    time.Now().UnixMilli(),
	}))
	assert.EqualValues(t, 2, counting.appends.Load())
	require.Equal(t, 2, countTransport1.getMessageCount())
	require.Equal(t, 2, countTransport2.getMessageCount())

	// Subscribers with the same encoding share identical frame bytes.
	assert.Equal(t, jsonTransport1.getMessage(0), jsonTransport2.getMessage(0))

	// Each frame decodes with its own encoding and carries the publication.
	var jsonOut, protoOut clientpb.OutboundMessage
	require.NoError(t, ProtoJSONMarshaler.Unmarshal(jsonTransport1.getMessage(0), &jsonOut))
	require.NoError(t, ProtobufMarshaler{}.Unmarshal(protoTransport.getMessage(0), &protoOut))
	for _, out := range []*clientpb.OutboundMessage{&jsonOut, &protoOut} {
		publication := out.GetPublication()
		require.NotNil(t, publication, "frame must carry a publication envelope")
		require.Len(t, publication.Messages, 1)
		assert.Equal(t, "test-channel", publication.Messages[0].Channel)
		assert.Equal(t, []byte("test payload"), publication.Messages[0].Payload.GetBinary())
	}
}

// TestHub_BroadcastPublication_JSONPassthrough covers the raw-JSON splice on
// the broadcast path: JSON-encoding subscribers receive the stored payload
// bytes verbatim (skipping the structpb round trip that would mangle big
// integers and key order), while protobuf subscribers get the structpb form.
// Both JSON-family marshalers splice: the production ProtoJSONMarshaler (WS/
// QUIC JSON wire) and the test/SDK JSONMarshaler.
func TestHub_BroadcastPublication_JSONPassthrough(t *testing.T) {
	h := newHub(0, 0)

	newEncodedClient := func(sessionID string, m Marshaler) (*Session, *mockTransport) {
		transport := &mockTransport{}
		client, _, err := NewClient(context.Background(), newFakeRuntime(), transport, m)
		require.NoError(t, err)
		client.mu.Lock()
		client.session = sessionID
		client.user = "user-" + sessionID
		client.client = "client-" + sessionID
		client.mu.Unlock()
		require.NoError(t, client.Attach(client.attachment))
		return client, transport
	}

	protoJSONClient, protoJSONTransport := newEncodedClient("pjson-1", ProtoJSONMarshaler)
	jsonClient, jsonTransport := newEncodedClient("json-1", JSONMarshaler{})
	protoClient, protoTransport := newEncodedClient("proto-1", ProtobufMarshaler{})
	for _, client := range []*Session{protoJSONClient, jsonClient, protoClient} {
		_, _ = h.AddSub("json-ch", Subscriber{Session: client, Ephemeral: false})
	}

	// An integer beyond float64 precision and out-of-order keys would not
	// survive a json.Unmarshal→structpb→protojson round trip verbatim.
	raw := []byte(`{"z":9007199254740993,"a":{"k":"v"}}`)
	pub := &Publication{
		Channel: "json-ch",
		Kind:    PayloadKindJSON,
		Offset:  1,
		Payload: raw,
		Time:    time.Now().UnixMilli(),
	}
	require.NoError(t, h.BroadcastPublication("json-ch", pub))

	require.Equal(t, 1, protoJSONTransport.getMessageCount())
	require.Equal(t, 1, jsonTransport.getMessageCount())
	require.Equal(t, 1, protoTransport.getMessageCount())

	// Both JSON-family frames embed the stored payload bytes verbatim — the
	// production protojson wire included (regression: the splice once
	// compared against the JSONMarshaler name only and never fired for it).
	protoJSONFrame := protoJSONTransport.getMessage(0)
	jsonFrame := jsonTransport.getMessage(0)
	assert.True(t, bytes.Contains(protoJSONFrame, raw), "protojson frame must splice the raw payload: %s", protoJSONFrame)
	assert.True(t, bytes.Contains(jsonFrame, raw), "json frame must splice the raw payload: %s", jsonFrame)

	// All frames still decode with their own encoding and keep the json oneof.
	var protoJSONOut, jsonOut, protoOut clientpb.OutboundMessage
	require.NoError(t, ProtoJSONMarshaler.Unmarshal(protoJSONFrame, &protoJSONOut))
	require.NoError(t, JSONMarshaler{}.Unmarshal(jsonFrame, &jsonOut))
	require.NoError(t, ProtobufMarshaler{}.Unmarshal(protoTransport.getMessage(0), &protoOut))
	for _, payload := range []*sharedv2.Payload{
		protoJSONOut.GetPublication().GetMessages()[0].GetPayload(),
		jsonOut.GetPublication().GetMessages()[0].GetPayload(),
		protoOut.GetPublication().GetMessages()[0].GetPayload(),
	} {
		require.NotNil(t, payload.GetJson(), "frame payload must stay the json oneof")
	}
	assert.Equal(t, "v", protoOut.GetPublication().GetMessages()[0].GetPayload().GetJson().GetFields()["a"].GetStructValue().GetFields()["k"].GetStringValue())
}

// TestSpliceRawJSONPayload_MissingPlaceholder pins the defensive contract: a
// frame without the empty-Struct splice point reports ok=false so the caller
// re-marshals with the real payload instead of shipping an empty object (and
// instead of aliasing the caller's pooled buffer).
func TestSpliceRawJSONPayload_MissingPlaceholder(t *testing.T) {
	frame := []byte(`{"publication":{"messages":[{"payload":{"text":"x"}}]}}`)
	out, ok := spliceRawJSONPayload(frame, []byte(`{"a":1}`))
	assert.False(t, ok, "a frame without the \"json\":{} placeholder must not be spliced")
	assert.Nil(t, out, "the failure path must not return a frame aliasing the caller's buffer")
}

// TestHub_BroadcastPublication_JSONNonObjectDegradesToText pins the legacy
// fallback: JSON-kind payloads that are not JSON objects cannot be spliced
// (structpb renders as an object), so both encodings get the text variant.
func TestHub_BroadcastPublication_JSONNonObjectDegradesToText(t *testing.T) {
	h := newHub(0, 0)

	newEncodedClient := func(sessionID string, m Marshaler) (*Session, *mockTransport) {
		transport := &mockTransport{}
		client, _, err := NewClient(context.Background(), newFakeRuntime(), transport, m)
		require.NoError(t, err)
		client.mu.Lock()
		client.session = sessionID
		client.mu.Unlock()
		require.NoError(t, client.Attach(client.attachment))
		return client, transport
	}

	jsonClient, jsonTransport := newEncodedClient("json-1", JSONMarshaler{})
	protoClient, protoTransport := newEncodedClient("proto-1", ProtobufMarshaler{})
	for _, client := range []*Session{jsonClient, protoClient} {
		_, _ = h.AddSub("json-arr", Subscriber{Session: client, Ephemeral: false})
	}

	raw := []byte(`[1,2,3]`)
	pub := &Publication{
		Channel: "json-arr",
		Kind:    PayloadKindJSON,
		Offset:  1,
		Payload: raw,
		Time:    time.Now().UnixMilli(),
	}
	require.NoError(t, h.BroadcastPublication("json-arr", pub))

	var jsonOut, protoOut clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(jsonTransport.getMessage(0), &jsonOut))
	require.NoError(t, ProtobufMarshaler{}.Unmarshal(protoTransport.getMessage(0), &protoOut))
	assert.Equal(t, string(raw), jsonOut.GetPublication().GetMessages()[0].GetPayload().GetText())
	assert.Equal(t, string(raw), protoOut.GetPublication().GetMessages()[0].GetPayload().GetText())
}

func TestIndex(t *testing.T) {
	tests := []struct {
		name       string
		s          string
		numBuckets int
		wantRange  [2]int // [min, max]
	}{
		{
			name:       "single bucket",
			s:          "any-string",
			numBuckets: 1,
			wantRange:  [2]int{0, 0},
		},
		{
			name:       "64 buckets",
			s:          "test-channel",
			numBuckets: 64,
			wantRange:  [2]int{0, 63},
		},
		{
			name:       "100 buckets",
			s:          "another-channel",
			numBuckets: 100,
			wantRange:  [2]int{0, 99},
		},
		{
			name:       "16384 buckets (subLocks)",
			s:          "subscription-channel",
			numBuckets: 16384,
			wantRange:  [2]int{0, 16383},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := index(tt.s, tt.numBuckets)
			if got < tt.wantRange[0] || got > tt.wantRange[1] {
				t.Errorf("index() = %d, want in range %v", got, tt.wantRange)
			}
		})
	}
}

func TestIndex_Consistency(t *testing.T) {
	s := "test-string"
	numBuckets := 64

	// Same string should always map to same bucket
	idx1 := index(s, numBuckets)
	idx2 := index(s, numBuckets)
	if idx1 != idx2 {
		t.Errorf("index() inconsistent: %d != %d", idx1, idx2)
	}
}

func TestIndex_Distribution(t *testing.T) {
	// Test that index distributes strings reasonably well across buckets
	numBuckets := 64
	buckets := make([]int, numBuckets)

	for i := 0; i < 1000; i++ {
		s := fmt.Sprintf("channel-%d", i)
		idx := index(s, numBuckets)
		buckets[idx]++
	}

	// Check that each bucket got at least some hits
	minHits := buckets[0]
	maxHits := buckets[0]
	for _, hits := range buckets {
		if hits < minHits {
			minHits = hits
		}
		if hits > maxHits {
			maxHits = hits
		}
	}

	// With 1000 items and 64 buckets, we expect ~16 per bucket
	// Allow for some variance but check distribution isn't terrible
	if minHits == 0 {
		t.Error("Some buckets got no hits, distribution may be poor")
	}
	if maxHits > 50 {
		t.Errorf("Max hits %d seems too high for poor distribution", maxHits)
	}
}

func TestHub_ConcurrentAdd(t *testing.T) {
	h := newHub(0, 0)
	const numClients = 100
	var wg sync.WaitGroup

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			client := newTestClient(t, fmt.Sprintf("session-%d", n), fmt.Sprintf("user-%d", n))
			_ = h.Add(client)
		}(i)
	}

	wg.Wait()

	h.mu.RLock()
	if len(h.sessions) != numClients {
		h.mu.RUnlock()
		t.Errorf("len(sessions) = %d, want %d", len(h.sessions), numClients)
	}
	h.mu.RUnlock()
}

func TestHub_ConcurrentSubscribe(t *testing.T) {
	h := newHub(0, 0)
	const numSubscribers = 100
	var wg sync.WaitGroup

	for i := 0; i < numSubscribers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			client := newTestClient(t, fmt.Sprintf("session-%d", n), fmt.Sprintf("user-%d", n))
			_, _ = h.AddSub("test-channel", Subscriber{Session: client, Ephemeral: false})
		}(i)
	}

	wg.Wait()

	count := h.NumSubscribers("test-channel")
	if count != numSubscribers {
		t.Errorf("NumSubscribers() = %d, want %d", count, numSubscribers)
	}
}

func TestSubShard_ConcurrentOperations(t *testing.T) {
	shard := newSubShard(0)
	const numOps = 100
	var wg sync.WaitGroup

	// Add subscribers
	for i := 0; i < numOps; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			client := newTestClient(t, fmt.Sprintf("session-%d", n), fmt.Sprintf("user-%d", n))
			_, _ = shard.addSub("test-channel", Subscriber{Session: client, Ephemeral: false})
		}(i)
	}

	wg.Wait()

	count := shard.NumSubscribers("test-channel")
	if count != numOps {
		t.Errorf("NumSubscribers() = %d, want %d", count, numOps)
	}
}

func TestHub_BroadcastPublication_MultipleChannels(t *testing.T) {
	h := newHub(0, 0)

	transport1 := &mockTransport{}
	transport2 := &mockTransport{}
	client1 := newTestClientWithTransport(t, "session-1", "user-1", transport1)
	client2 := newTestClientWithTransport(t, "session-2", "user-2", transport2)

	_, _ = h.AddSub("channel-1", Subscriber{Session: client1, Ephemeral: false})
	_, _ = h.AddSub("channel-2", Subscriber{Session: client2, Ephemeral: false})

	pub1 := &Publication{
		Channel: "channel-1",
		Offset:  1,
		Payload: []byte("payload-1"),
		Time:    time.Now().UnixMilli(),
	}

	err := h.BroadcastPublication("channel-1", pub1)
	if err != nil {
		t.Fatalf("broadcastPublication() error = %v", err)
	}

	// Only client1 should receive the message
	if transport1.getMessageCount() != 1 {
		t.Errorf("client1 received %d messages, want 1", transport1.getMessageCount())
	}
	if transport2.getMessageCount() != 0 {
		t.Errorf("client2 received %d messages, want 0", transport2.getMessageCount())
	}
}

func TestSubShard_AddSub_EphemeralFlag(t *testing.T) {
	shard := newSubShard(0)
	client := newTestClient(t, "session-1", "user-1")

	// Add ephemeral subscription
	first, err := shard.addSub("test-channel", Subscriber{Session: client, Ephemeral: true})
	if err != nil {
		t.Fatalf("addSub() error = %v", err)
	}
	if !first {
		t.Error("addSub() should return true for first subscriber")
	}

	shard.mu.RLock()
	subs, ok := shard.subs["test-channel"]
	shard.mu.RUnlock()

	if !ok {
		t.Fatal("channel should have subscribers")
	}

	sub := subs["session-1"]
	if !sub.Ephemeral {
		t.Error("ephemeral flag should be preserved")
	}
}

func TestHub_MultipleChannels(t *testing.T) {
	h := newHub(0, 0)
	client := newTestClient(t, "session-1", "user-1")

	channels := []string{"channel-1", "channel-2", "channel-3"}
	for _, ch := range channels {
		_, _ = h.AddSub(ch, Subscriber{Session: client, Ephemeral: false})
	}

	for _, ch := range channels {
		count := h.NumSubscribers(ch)
		if count != 1 {
			t.Errorf("NumSubscribers(%q) = %d, want 1", ch, count)
		}
	}
}

// --- Fix task 11: GetActiveChannels must not list wildcard patterns or
// double-count exact + wildcard subscriptions ---

func TestHub_GetActiveChannels_ExcludesWildcardPatterns(t *testing.T) {
	h := newHub(0, 0)
	transport1 := &mockTransport{}
	transport2 := &mockTransport{}
	client1 := newTestClientWithTransport(t, "session-1", "user-1", transport1)
	client2 := newTestClientWithTransport(t, "session-2", "user-2", transport2)

	require.NoError(t, h.Add(client1))
	require.NoError(t, h.Add(client2))

	// client1 subscribes to chat.x exactly and via chat.*; client2 to chat.y.
	_, err := h.AddSub("chat.x", Subscriber{Session: client1, Ephemeral: false})
	require.NoError(t, err)
	_, err = h.AddSub("chat.*", Subscriber{Session: client1, Ephemeral: false})
	require.NoError(t, err)
	_, err = h.AddSub("chat.y", Subscriber{Session: client2, Ephemeral: false})
	require.NoError(t, err)

	channels := h.GetActiveChannels()
	require.Len(t, channels, 2, "wildcard patterns must not be listed as active channels")
	assert.Equal(t, "chat.x", channels[0].Name)
	assert.Equal(t, 1, channels[0].Subscribers, "exact + wildcard subscription must not double-count")
	assert.Equal(t, "chat.y", channels[1].Name)
	assert.Equal(t, 1, channels[1].Subscribers)
}

// --- P1-13: exact + wildcard double subscription must not double-deliver ---

func newTestPublication(channel string, offset uint64) *Publication {
	return &Publication{
		Channel: channel,
		Offset:  offset,
		Payload: []byte("test payload"),
		Time:    time.Now().UnixMilli(),
	}
}

// assertSinglePublication asserts the transport captured exactly one message
// and that it is a Publication envelope carrying exactly one message.
func assertSinglePublication(t *testing.T, transport *mockTransport, channel string) {
	t.Helper()
	require.Equal(t, 1, transport.getMessageCount())
	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(0), &out))
	pub := out.GetPublication()
	require.NotNil(t, pub, "outbound message should be a publication")
	require.Len(t, pub.GetMessages(), 1)
	assert.Equal(t, channel, pub.GetMessages()[0].GetChannel())
}

func TestHub_BroadcastPublication_DedupExactAndWildcard(t *testing.T) {
	h := newHub(0, 0)
	transport := &mockTransport{}
	client := newTestClientWithTransport(t, "session-1", "user-1", transport)

	_, err := h.AddSub("chat.x", Subscriber{Session: client, Ephemeral: false})
	require.NoError(t, err)
	_, err = h.AddSub("chat.*", Subscriber{Session: client, Ephemeral: false})
	require.NoError(t, err)

	err = h.BroadcastPublication("chat.x", newTestPublication("chat.x", 1))
	require.NoError(t, err)

	// Subscribed both exactly and via wildcard: exactly one copy is delivered.
	assertSinglePublication(t, transport, "chat.x")
}

func TestHub_BroadcastPublication_DedupExactAndWildcard_MixedSubscribers(t *testing.T) {
	h := newHub(0, 0)

	transportBoth := &mockTransport{}
	transportWildcard := &mockTransport{}
	transportExact := &mockTransport{}
	clientBoth := newTestClientWithTransport(t, "session-both", "user-both", transportBoth)
	clientWildcard := newTestClientWithTransport(t, "session-wild", "user-wild", transportWildcard)
	clientExact := newTestClientWithTransport(t, "session-exact", "user-exact", transportExact)

	for _, sub := range []struct {
		channel string
		client  *Client
	}{
		{"chat.x", clientBoth},
		{"chat.*", clientBoth},
		{"chat.*", clientWildcard},
		{"chat.x", clientExact},
	} {
		_, err := h.AddSub(sub.channel, Subscriber{Session: sub.client, Ephemeral: false})
		require.NoError(t, err)
	}

	err := h.BroadcastPublication("chat.x", newTestPublication("chat.x", 1))
	require.NoError(t, err)

	// Every client receives exactly one copy regardless of how it subscribed.
	assertSinglePublication(t, transportBoth, "chat.x")
	assertSinglePublication(t, transportWildcard, "chat.x")
	assertSinglePublication(t, transportExact, "chat.x")
}

func TestHub_BroadcastPublication_ExactOnly_SingleDelivery(t *testing.T) {
	h := newHub(0, 0)
	transport := &mockTransport{}
	client := newTestClientWithTransport(t, "session-1", "user-1", transport)

	_, err := h.AddSub("chat.x", Subscriber{Session: client, Ephemeral: false})
	require.NoError(t, err)

	err = h.BroadcastPublication("chat.x", newTestPublication("chat.x", 1))
	require.NoError(t, err)

	assertSinglePublication(t, transport, "chat.x")
}

func TestHub_BroadcastPublication_WildcardOnly_SingleDelivery(t *testing.T) {
	h := newHub(0, 0)
	transport := &mockTransport{}
	client := newTestClientWithTransport(t, "session-1", "user-1", transport)

	_, err := h.AddSub("chat.*", Subscriber{Session: client, Ephemeral: false})
	require.NoError(t, err)

	err = h.BroadcastPublication("chat.x", newTestPublication("chat.x", 1))
	require.NoError(t, err)

	assertSinglePublication(t, transport, "chat.x")
}

// --- P2-16: large fan-out must deliver to all subscribers via bounded concurrency ---

func TestHub_BroadcastPublication_LargeFanOut(t *testing.T) {
	h := newHub(0, 0)
	const n = 200
	transports := make([]*mockTransport, n)
	for i := 0; i < n; i++ {
		transports[i] = &mockTransport{}
		client := newTestClientWithTransport(t, fmt.Sprintf("session-%d", i), fmt.Sprintf("user-%d", i), transports[i])
		_, err := h.AddSub("fan.ch", Subscriber{Session: client, Ephemeral: false})
		require.NoError(t, err)
	}

	err := h.BroadcastPublication("fan.ch", newTestPublication("fan.ch", 1))
	require.NoError(t, err)

	for i, transport := range transports {
		assert.Equal(t, 1, transport.getMessageCount(), "client %d should receive exactly one message", i)
	}
}

// --- P1-3: realtime delivery message IDs must be stable (channel-offset) ---

func TestHub_BroadcastPublication_StableMessageID(t *testing.T) {
	h := newHub(0, 0)
	transport := &mockTransport{}
	client := newTestClientWithTransport(t, "session-1", "user-1", transport)

	_, err := h.AddSub("stable.ch", Subscriber{Session: client, Ephemeral: false})
	require.NoError(t, err)

	err = h.BroadcastPublication("stable.ch", newTestPublication("stable.ch", 42))
	require.NoError(t, err)

	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(0), &out))
	pub := out.GetPublication()
	require.NotNil(t, pub)
	require.Len(t, pub.GetMessages(), 1)
	assert.Equal(t, "stable.ch-42", pub.GetMessages()[0].GetId())
}

// --- PR-KA-B1: local resume must keep the Session pointer stable ---

// countingMatcher wraps a topics.Matcher and counts Subscribe/Unsubscribe
// calls, so tests can prove a resume does not rebuild wildcard matcher
// entries just to swap a pointer.
type countingMatcher struct {
	topics.Matcher
	subscribes   int
	unsubscribes int
}

func (m *countingMatcher) Subscribe(topic string, sub topics.Subscriber) (*topics.Subscription, error) {
	m.subscribes++
	return m.Matcher.Subscribe(topic, sub)
}

func (m *countingMatcher) Unsubscribe(sub *topics.Subscription) {
	m.unsubscribes++
	m.Matcher.Unsubscribe(sub)
}

// TestHub_Resume_KeepsSessionPointerStable verifies §6 / §9.1: a local
// resume (Detach + Attach on the same session) must not touch the hub
// registries — LookupSession returns the same pointer, the subShard records
// keep the same Subscriber.Session and the wildcard matcher is not rebuilt.
func TestHub_Resume_KeepsSessionPointerStable(t *testing.T) {
	h := newHub(0, 0)
	matcher := &countingMatcher{Matcher: topics.NewCSTrieMatcher()}
	h.matcher = matcher

	oldTransport := &mockTransport{}
	newTransport := &mockTransport{}
	session := newTestClientWithTransport(t, "session-1", "user-1", oldTransport)
	require.NoError(t, h.Add(session))
	_, err := h.AddSub("chat.exact", Subscriber{Session: session, Ephemeral: false})
	require.NoError(t, err)
	_, err = h.AddSub("chat.*", Subscriber{Session: session, Ephemeral: false})
	require.NoError(t, err)

	// Snapshot the matcher subscription count after setup.
	subscribesBefore := matcher.subscribes
	unsubscribesBefore := matcher.unsubscribes

	// Local takeover: tear off the old attachment, bind the new one.
	session.Detach(Disconnect{})
	newAtt := &Attachment{Transport: newTransport, Marshaler: JSONMarshaler{}, Protocol: "ws"}
	require.NoError(t, session.Attach(newAtt))

	// 1. Pointer identity: the hub still holds the same session object.
	assert.Same(t, session, h.LookupSession("session-1"), "LookupSession must return the same pointer before and after resume")

	// 2. Exact subShard: the Subscriber.Session pointer is unchanged.
	exact, ok := h.LookupSubscriber("chat.exact", session)
	require.True(t, ok)
	assert.Same(t, session, exact.Session, "the exact subShard record must keep the same Session pointer")

	// 3. Wildcard matcher: the Subscriber.Session pointer is unchanged and no
	//    Subscribe/Unsubscribe ran for the pointer swap.
	wildcard, ok := h.LookupSubscriber("chat.*", session)
	require.True(t, ok)
	assert.Same(t, session, wildcard.Session, "the matcher record must keep the same Session pointer")
	assert.Equal(t, subscribesBefore, matcher.subscribes, "resume must not re-subscribe matcher entries")
	assert.Equal(t, unsubscribesBefore, matcher.unsubscribes, "resume must not unsubscribe matcher entries")

	// 4. Deliveries reach the new attachment.
	err = h.BroadcastPublication("chat.exact", newTestPublication("chat.exact", 1))
	require.NoError(t, err)
	assert.Equal(t, 1, newTransport.getMessageCount(), "the new attachment must receive deliveries")
	assert.Equal(t, 0, oldTransport.getMessageCount(), "the old attachment must not receive deliveries")
}

// TestHub_PrepareSessionUser_EnforcesMaxConnsPerUser verifies §6.5: the
// per-user limit is checked before a cross-user resume touches the old
// session; a same-user resume is always allowed.
func TestHub_PrepareSessionUser_EnforcesMaxConnsPerUser(t *testing.T) {
	h := newHub(0, 1) // 1 connection per user

	// user-a owns session-1; user-b already occupies its single slot.
	clientA := newTestClientWithTransport(t, "session-1", "user-a", &mockTransport{})
	require.NoError(t, h.Add(clientA))
	clientB := newTestClientWithTransport(t, "session-2", "user-b", &mockTransport{})
	require.NoError(t, h.Add(clientB))

	// Moving session-1 to user-b must hit the connection limit.
	err := h.PrepareSessionUser("session-1", clientA, "user-b")
	require.Error(t, err, "PrepareSessionUser must enforce maxConnsPerUser")
	assert.ErrorIs(t, err, DisconnectConnectionLimit)

	// Same-user stays within the limit and succeeds.
	require.NoError(t, h.PrepareSessionUser("session-1", clientA, "user-a"))
}

// TestHub_PrepareSessionUser_FailureKeepsOldSessionIntact guards §9.3: a
// failed cross-user limit check must not mutate the hub at all — the old
// session keeps its hub entry, its connShard registration and its
// subscriptions (they still deliver), and the session stays Attached.
func TestHub_PrepareSessionUser_FailureKeepsOldSessionIntact(t *testing.T) {
	h := newHub(0, 1) // 1 connection per user

	transportA := &mockTransport{}
	clientA := newTestClientWithTransport(t, "session-1", "user-a", transportA)
	require.NoError(t, h.Add(clientA))
	_, err := h.AddSub("zombie-ch", Subscriber{Session: clientA, Ephemeral: false})
	require.NoError(t, err)

	clientB := newTestClientWithTransport(t, "session-2", "user-b", &mockTransport{})
	require.NoError(t, h.Add(clientB))

	// user-b sits at the limit, so this migration must fail before any
	// mutation.
	err = h.PrepareSessionUser("session-1", clientA, "user-b")
	require.Error(t, err)
	assert.ErrorIs(t, err, DisconnectConnectionLimit)

	// The old session is still fully registered and Attached...
	assert.Same(t, clientA, h.LookupSession("session-1"))
	shard := h.connShards[index("user-a", numHubShards)]
	shard.mu.RLock()
	_, inConnShard := shard.clients["session-1"]
	shard.mu.RUnlock()
	assert.True(t, inConnShard, "old session must keep its connShard entry")
	assert.Equal(t, SessionAttached, clientA.State())
	assert.Equal(t, 1, h.NumSubscribers("zombie-ch"))
	sub, ok := h.LookupSubscriber("zombie-ch", clientA)
	require.True(t, ok)
	assert.Same(t, clientA, sub.Session)

	// ...and still receives deliveries.
	err = h.BroadcastPublication("zombie-ch", newTestPublication("zombie-ch", 1))
	require.NoError(t, err)
	assert.Equal(t, 1, transportA.getMessageCount(), "old session must keep receiving deliveries")

	// The unrelated B session is untouched.
	assert.Same(t, clientB, h.LookupSession("session-2"))
}

// TestHubAddSubRejectsMalformedExactChannel pins B1: the exact-subscription
// entry must reject channels with explicit empty segments ("a.", ".a",
// "a..b") and the empty channel with ErrBadTopic instead of silently
// registering them.
func TestHubAddSubRejectsMalformedExactChannel(t *testing.T) {
	h := newHub(0, 0)
	client := newTestClient(t, "session-1", "user-1")
	sub := Subscriber{Session: client, Ephemeral: false}

	for _, ch := range []string{"a.", ".a", "a..b", ""} {
		_, err := h.AddSub(ch, sub)
		assert.ErrorIs(t, err, topics.ErrBadTopic, "addSub(%q)", ch)
	}

	// The rejected channels must not be registered anywhere.
	assert.Zero(t, h.NumSubscribers("a."))
	_, ok := h.LookupSubscriber("a.", client)
	assert.False(t, ok)

	// Valid exact channels still work, including wildcard-pattern channels
	// that go through the matcher.
	first, err := h.AddSub("valid.channel", sub)
	assert.NoError(t, err)
	assert.True(t, first)
	_, err = h.AddSub("a.**", sub)
	assert.NoError(t, err)
	_, err = h.AddSub("a.**.b", sub)
	assert.ErrorIs(t, err, topics.ErrBadTopic, "addSub(%q)", "a.**.b")
}

// TestHub_SessionsByUser verifies the user→sessions lookup: two sessions of
// the same user are both returned, other users never leak in, and an empty
// user ID returns empty even when anonymous connections are registered.
func TestHub_SessionsByUser(t *testing.T) {
	h := newHub(0, 0)
	clientA := newTestClient(t, "session-a-1", "user-a")
	clientA2 := newTestClient(t, "session-a-2", "user-a")
	clientB := newTestClient(t, "session-b", "user-b")
	anon := newTestClient(t, "session-anon", "")
	for _, c := range []*Client{clientA, clientA2, clientB, anon} {
		require.NoError(t, h.Add(c))
	}

	sessions := h.SessionsByUser("user-a")
	require.Len(t, sessions, 2)
	got := []string{sessions[0].SessionID(), sessions[1].SessionID()}
	assert.Equal(t, []string{"session-a-1", "session-a-2"}, got, "sessions must be sorted and must not mix other users")

	sessionsB := h.SessionsByUser("user-b")
	require.Len(t, sessionsB, 1)
	assert.Equal(t, "session-b", sessionsB[0].SessionID())

	assert.Empty(t, h.SessionsByUser("user-unknown"))
	// Empty user ID must stay empty even though the shard holds anonymous
	// connections under the empty key.
	assert.Empty(t, h.SessionsByUser(""))

	// Removed sessions disappear from the lookup.
	require.True(t, h.RemoveSessionIfMatches("session-a-1", clientA))
	assert.Len(t, h.SessionsByUser("user-a"), 1)
}
