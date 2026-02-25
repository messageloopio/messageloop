package v2_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	v2 "github.com/messageloopio/messageloop/v2"
	v2pb "github.com/messageloopio/messageloop/shared/genproto/v2"
)

// mockTransport implements v2.Transport for testing.
type mockTransport struct {
	mu      sync.Mutex
	sent    [][]byte
	recvCh  chan []byte
	done    chan struct{}
	closed  bool
	closeCode v2pb.DisconnectCode
}

func newMockTransport() *mockTransport {
	return &mockTransport{
		recvCh: make(chan []byte, 16),
		done:   make(chan struct{}),
	}
}

func (m *mockTransport) Send(_ context.Context, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	m.sent = append(m.sent, cp)
	return nil
}

func (m *mockTransport) Receive(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case data, ok := <-m.recvCh:
		if !ok {
			return nil, errors.New("transport closed")
		}
		return data, nil
	}
}

func (m *mockTransport) Close(code v2pb.DisconnectCode, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		m.closeCode = code
		close(m.done)
	}
	return nil
}

func (m *mockTransport) Done() <-chan struct{} { return m.done }

func (m *mockTransport) inject(data []byte) { m.recvCh <- data }

func (m *mockTransport) sentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func (m *mockTransport) lastSent() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		return nil
	}
	return m.sent[len(m.sent)-1]
}

// Helper: create a ClientV2 with a mock transport and protobuf encoder.
func newTestClient(t *testing.T) (*v2.ClientV2, *mockTransport, *v2.ProtobufEncoder) {
	t.Helper()
	tr := newMockTransport()
	enc := &v2.ProtobufEncoder{}
	client := v2.NewClientV2("test-session", tr, enc)
	return client, tr, enc
}

func TestClientV2_Send_WrapsInFrame(t *testing.T) {
	client, tr, enc := newTestClient(t)

	msg := &v2pb.Message{
		Payload: &v2pb.Message_Ping{Ping: &v2pb.Ping{Timestamp: 42}},
	}

	if err := client.Send(context.Background(), msg, 0); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if tr.sentCount() != 1 {
		t.Fatalf("expected 1 sent message, got %d", tr.sentCount())
	}

	// Decode the sent frame
	frame, err := enc.DecodeFrame(tr.lastSent())
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}

	if frame.FrameId == "" {
		t.Error("frame_id should be non-empty")
	}
	if frame.SessionId != "test-session" {
		t.Errorf("session_id mismatch: got %q", frame.SessionId)
	}
	if frame.Encoding != v2pb.Encoding_ENCODING_PROTOBUF {
		t.Errorf("encoding mismatch: got %v", frame.Encoding)
	}
}

func TestClientV2_Publish_SendsPublication(t *testing.T) {
	client, tr, enc := newTestClient(t)

	err := client.Publish(context.Background(), "news.sports", []byte(`{"score":1}`), "score-update", nil)
	if err != nil {
		t.Fatalf("Publish failed: %v", err)
	}

	frame, err := enc.DecodeFrame(tr.lastSent())
	if err != nil {
		t.Fatalf("DecodeFrame failed: %v", err)
	}

	decoded, err := enc.DecodeMessage(frame.Payload)
	if err != nil {
		t.Fatalf("DecodeMessage failed: %v", err)
	}

	pub, ok := decoded.Payload.(*v2pb.Message_Publication)
	if !ok {
		t.Fatalf("expected Publication, got %T", decoded.Payload)
	}
	if pub.Publication.Channel != "news.sports" {
		t.Errorf("channel mismatch: got %q", pub.Publication.Channel)
	}
}

func TestClientV2_HandleFrame_DispatchesMessage(t *testing.T) {
	tr := newMockTransport()
	enc := &v2.ProtobufEncoder{}

	var received *v2pb.Message
	var wg sync.WaitGroup
	wg.Add(1)

	client := v2.NewClientV2("session-42", tr, enc,
		v2.WithOnMessage(func(msg *v2pb.Message) {
			received = msg
			wg.Done()
		}),
	)

	// Start the receive loop in background
	go client.Start()

	// Build a frame with a Ping message
	pingMsg := &v2pb.Message{
		Payload: &v2pb.Message_Ping{Ping: &v2pb.Ping{Timestamp: 999}},
	}
	payload, _ := enc.EncodeMessage(pingMsg)
	frame := &v2pb.Frame{
		FrameId:   "frame-1",
		SessionId: "session-42",
		ClientSeq: 1,
		Timestamp: time.Now().UnixMilli(),
		Encoding:  v2pb.Encoding_ENCODING_PROTOBUF,
		Payload:   payload,
	}
	data, _ := enc.EncodeFrame(frame)
	tr.inject(data)

	// Wait for message to be dispatched
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for message dispatch")
	}

	if received == nil {
		t.Fatal("expected message to be received")
	}
	ping, ok := received.Payload.(*v2pb.Message_Ping)
	if !ok || ping.Ping.Timestamp != 999 {
		t.Errorf("unexpected message: %v", received)
	}

	// Close the transport to unblock Start()
	tr.Close(v2pb.DisconnectCode_DISCONNECT_NORMAL, "")
}

func TestClientV2_HandleFrame_ACKRequired(t *testing.T) {
	tr := newMockTransport()
	enc := &v2.ProtobufEncoder{}
	client := v2.NewClientV2("sess-ack", tr, enc)

	go client.Start()

	// Send a frame with ACK_REQUIRED flag
	frame := &v2pb.Frame{
		FrameId:   "frame-ack-1",
		SessionId: "sess-ack",
		ClientSeq: 1,
		Timestamp: time.Now().UnixMilli(),
		Flags:     uint32(v2pb.FrameFlags_FLAG_ACK_REQUIRED),
		Encoding:  v2pb.Encoding_ENCODING_PROTOBUF,
		Payload:   []byte{},
	}
	data, _ := enc.EncodeFrame(frame)
	tr.inject(data)

	// Give time for ACK to be sent
	time.Sleep(50 * time.Millisecond)

	// Verify an ACK was sent back
	if tr.sentCount() == 0 {
		t.Fatal("expected ACK frame to be sent")
	}
	ackFrame, err := enc.DecodeFrame(tr.lastSent())
	if err != nil {
		t.Fatalf("decode ACK frame failed: %v", err)
	}
	if ackFrame.Flags&uint32(v2pb.FrameFlags_FLAG_IS_ACK) == 0 {
		t.Error("expected IS_ACK flag to be set")
	}

	tr.Close(v2pb.DisconnectCode_DISCONNECT_NORMAL, "")
}

func TestClientV2_Disconnect_SendsDisconnectAndCloses(t *testing.T) {
	client, tr, _ := newTestClient(t)

	err := client.Disconnect(v2pb.DisconnectCode_DISCONNECT_KICKED, "kicked by admin", false)
	if err != nil {
		t.Fatalf("Disconnect failed: %v", err)
	}

	if !tr.closed {
		t.Error("transport should be closed after Disconnect")
	}
	if tr.closeCode != v2pb.DisconnectCode_DISCONNECT_KICKED {
		t.Errorf("expected KICKED close code, got %v", tr.closeCode)
	}
}
