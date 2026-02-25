package v2_test

import (
	"context"
	"testing"

	v2 "github.com/messageloopio/messageloop/v2"
	v2pb "github.com/messageloopio/messageloop/shared/genproto/v2"
)

// benchTransport is a no-op transport for benchmarks.
type benchTransport struct{ done chan struct{} }

func newBenchTransport() *benchTransport { return &benchTransport{done: make(chan struct{})} }
func (b *benchTransport) Send(_ context.Context, _ []byte) error { return nil }
func (b *benchTransport) Receive(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (b *benchTransport) Close(_ v2pb.DisconnectCode, _ string) error { return nil }
func (b *benchTransport) Done() <-chan struct{}                        { return b.done }

func BenchmarkProtobufEncoder_EncodeFrame(b *testing.B) {
	enc := &v2.ProtobufEncoder{}
	frame := &v2pb.Frame{
		FrameId:   "bench-frame-id-1234-5678",
		SessionId: "bench-session-id",
		ClientSeq: 42,
		Timestamp: 1700000000000,
		Flags:     uint32(v2pb.FrameFlags_FLAG_ACK_REQUIRED),
		Encoding:  v2pb.Encoding_ENCODING_PROTOBUF,
		Payload:   []byte(`{"channel":"news","data":"hello"}`),
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = enc.EncodeFrame(frame)
	}
}

func BenchmarkProtobufEncoder_DecodeFrame(b *testing.B) {
	enc := &v2.ProtobufEncoder{}
	frame := &v2pb.Frame{
		FrameId:   "bench-frame-id",
		SessionId: "bench-session",
		ClientSeq: 1,
		Timestamp: 1700000000000,
		Encoding:  v2pb.Encoding_ENCODING_PROTOBUF,
		Payload:   []byte(`hello world`),
	}
	data, _ := enc.EncodeFrame(frame)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = enc.DecodeFrame(data)
	}
}

func BenchmarkJSONEncoder_EncodeFrame(b *testing.B) {
	enc := v2.NewJSONEncoder()
	frame := &v2pb.Frame{
		FrameId:   "bench-frame-id-1234-5678",
		SessionId: "bench-session-id",
		ClientSeq: 42,
		Timestamp: 1700000000000,
		Encoding:  v2pb.Encoding_ENCODING_JSON,
		Payload:   []byte(`{"channel":"news"}`),
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = enc.EncodeFrame(frame)
	}
}

func BenchmarkClientV2_Send(b *testing.B) {
	tr := newBenchTransport()
	enc := &v2.ProtobufEncoder{}
	client := v2.NewClientV2("bench-session", tr, enc)

	msg := &v2pb.Message{
		Payload: &v2pb.Message_Publication{
			Publication: &v2pb.Publication{
				Channel: "bench.channel",
				Data:    []byte(`{"value":42}`),
				Type:    "data",
			},
		},
	}

	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = client.Send(ctx, msg, 0)
	}
}

func BenchmarkDedupCache_IsDuplicate(b *testing.B) {
	cache := v2.NewDedupCache()
	// Pre-populate with some entries
	for i := uint64(0); i < 1000; i++ {
		cache.IsDuplicate("session-bench", i)
	}
	b.ResetTimer()
	// Benchmark new entries (not duplicates)
	for i := uint64(1000); i < uint64(1000+b.N); i++ {
		cache.IsDuplicate("session-bench", i)
	}
}

func BenchmarkUUIDGenerator(b *testing.B) {
	g := &v2.UUIDGenerator{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = g.Generate()
	}
}
