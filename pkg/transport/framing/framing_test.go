package framing

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/internal/protocol"
	"github.com/messageloopio/messageloop/shared"
)

// failingMarshaler fails every Marshal so the disconnect-frame error path can
// be exercised without a real encoding.
type failingMarshaler struct{}

func (failingMarshaler) Marshal(any) ([]byte, error)               { return nil, errors.New("nope") }
func (failingMarshaler) MarshalAppend([]byte, any) ([]byte, error) { return nil, errors.New("nope") }
func (failingMarshaler) Unmarshal([]byte, any) error               { return errors.New("nope") }
func (failingMarshaler) Name() string                              { return "failing" }
func (failingMarshaler) IsJSONWire() bool                          { return false }

// recordingDeadlines captures every SetWriteDeadline call so tests can pin
// the per-write budget discipline without a real deadline-enforcing stream.
type recordingDeadlines struct {
	mu  sync.Mutex
	set []time.Time
}

func (r *recordingDeadlines) setDeadline(t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.set = append(r.set, t)
	return nil
}

func (r *recordingDeadlines) calls() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.set...)
}

func TestWriterWriteManyRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	dl := &recordingDeadlines{}
	w := NewWriter(&buf, dl.setDeadline, time.Second, errors.New("closed sentinel"))

	require.NoError(t, w.WriteMany([]byte("frame-one"), []byte("frame-two")))

	r := bytes.NewReader(buf.Bytes())
	first, err := shared.ReadFrame(r, 0)
	require.NoError(t, err)
	require.Equal(t, []byte("frame-one"), first)
	second, err := shared.ReadFrame(r, 0)
	require.NoError(t, err)
	require.Equal(t, []byte("frame-two"), second)

	// One deadline armed per frame (each frame gets a fresh full budget),
	// cleared once after the batch.
	calls := dl.calls()
	require.Len(t, calls, 3, "deadline armed per frame, cleared once after the batch")
	require.False(t, calls[0].IsZero())
	require.False(t, calls[1].IsZero())
	require.True(t, calls[len(calls)-1].IsZero())
}

func TestWriterMarkClosedAndSentinel(t *testing.T) {
	var buf bytes.Buffer
	sentinel := errors.New("transport closed")
	w := NewWriter(&buf, nil, 0, sentinel)

	require.True(t, w.MarkClosed(), "first MarkClosed reports the transition")
	require.False(t, w.MarkClosed(), "second MarkClosed is a no-op")

	err := w.WriteMany([]byte("x"))
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, 0, buf.Len(), "no bytes may be written after MarkClosed")
}

func TestWriterEffectiveTimeout(t *testing.T) {
	require.Equal(t, DefaultWriteTimeout, NewWriter(nil, nil, 0, nil).EffectiveTimeout())
	require.Equal(t, 250*time.Millisecond, NewWriter(nil, nil, 250*time.Millisecond, nil).EffectiveTimeout())
}

// TestWriteDisconnectFrameAfterMarkClosed pins the ordering contract of the
// Close path (ADR-0009): the disconnect frame is NOT gated by the closed
// flag — adapters call MarkClosed first and then WriteDisconnectFrame, so the
// goodbye frame still goes out on the wire.
func TestWriteDisconnectFrameAfterMarkClosed(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, nil, 0, errors.New("closed sentinel"))
	require.True(t, w.MarkClosed())

	require.NoError(t, w.WriteDisconnectFrame(shared.ProtobufMarshaler{}, protocol.Disconnect{Code: 3500, Reason: "bye"}))
	frame, err := shared.ReadFrame(bytes.NewReader(buf.Bytes()), 0)
	require.NoError(t, err)
	require.NotEmpty(t, frame, "disconnect frame must still be written after MarkClosed")
}

func TestWriteDisconnectFrameDeadlineBudget(t *testing.T) {
	var buf bytes.Buffer
	dl := &recordingDeadlines{}
	w := NewWriter(&buf, dl.setDeadline, 30*time.Second, nil)

	before := time.Now()
	require.NoError(t, w.WriteDisconnectFrame(shared.ProtobufMarshaler{}, protocol.Disconnect{Code: 3512, Reason: "slow"}))

	calls := dl.calls()
	require.Len(t, calls, 2)
	budget := calls[0].Sub(before)
	require.Greater(t, budget, time.Duration(0))
	require.LessOrEqual(t, budget, DisconnectFrameTimeout+100*time.Millisecond,
		"disconnect frame must be bounded by DisconnectFrameTimeout, not the 30s write timeout")
	require.True(t, calls[1].IsZero(), "deadline must be cleared after the frame")
}

func TestWriteDisconnectFrameMarshalError(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, nil, 0, nil)

	err := w.WriteDisconnectFrame(failingMarshaler{}, protocol.Disconnect{Code: 3500, Reason: "bye"})
	require.ErrorContains(t, err, "marshal disconnect frame")
	require.Equal(t, 0, buf.Len(), "nothing may reach the wire when marshaling fails")
}

func TestDisconnectMessageShape(t *testing.T) {
	msg := DisconnectMessage(3512, "write timeout")
	env := msg.GetError()
	require.NotNil(t, env)
	require.Equal(t, "DISCONNECT_ERROR", env.GetCode())
	require.Equal(t, "transport_error", env.GetType())
	require.Equal(t, "write timeout", env.GetMessage())
	require.Equal(t, float64(3512), env.GetMetadata().GetFields()["disconnect_code"].GetNumberValue())
}

// TestWriterConcurrentFramesIntact pins the serialization discipline: frames
// from concurrent WriteMany calls must never interleave — every frame comes
// back out byte-for-byte intact.
func TestWriterConcurrentFramesIntact(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, nil, 0, nil)

	const writers = 16
	payloads := make([][]byte, writers)
	for i := range payloads {
		payloads[i] = []byte(fmt.Sprintf("writer-%02d-payload", i))
	}
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = w.WriteMany(payloads[i])
		}(i)
	}
	wg.Wait()

	got := make(map[string]bool)
	r := bytes.NewReader(buf.Bytes())
	for {
		frame, err := shared.ReadFrame(r, 0)
		if err != nil {
			break
		}
		got[string(frame)] = true
	}
	require.Len(t, got, writers, "every frame must be present exactly once and intact")
	for _, p := range payloads {
		require.True(t, got[string(p)], "frame %q was torn or lost", p)
	}
}
