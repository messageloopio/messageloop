package ws

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lynx-go/x/log"

	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/internal/session"
	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// Handler upgrades HTTP requests to WebSocket connections and binds each
// connection to a session via the Hub.
type Handler struct {
	node     *runtime.Node
	opt      *Options
	upgrader *websocket.Upgrader
}

// NewHandler creates a WebSocket handler bound to the node. The negotiated
// subprotocol (messageloop, messageloop+json, messageloop+proto) selects the
// marshaler and frame type per connection.
func NewHandler(node *runtime.Node, opt Options) *Handler {
	handler := &Handler{
		node: node,
		opt:  &opt,
		upgrader: &websocket.Upgrader{
			Subprotocols: []string{
				"messageloop",
				"messageloop+json",
				"messageloop+proto",
			},
			CheckOrigin:       opt.CheckOrigin,
			EnableCompression: opt.Compression,
		},
	}
	return handler
}

// ServeHTTP upgrades the request to WebSocket and runs the client session
// until the connection closes. It returns only after the session ends.
func (h *Handler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(rw, r, nil)
	if err != nil {
		// The upgrader has already written the handshake error response.
		log.ErrorContext(r.Context(), "websocket upgrade error", err)
		return
	}

	// The negotiated subprotocol decides both the marshaler and the frame
	// type. Reading the client's offer list (websocket.Subprotocols) instead
	// would desync the two: gorilla negotiates against the server-side list,
	// so offer order does not determine the result.
	subProtocol := conn.Subprotocol()
	marshaler := h.marshaler(subProtocol)
	transport := newTransport(conn, msgTypeFromSubprotocol(subProtocol), h.opt.WriteTimeout)
	ctx := r.Context()
	client, closeFn, err := runtime.NewClient(ctx, h.node, transport, marshaler, session.WithProtocol("ws"))
	if err != nil {
		log.ErrorContext(r.Context(), "create client error", err)
		// The connection is already upgraded; rw can no longer carry an HTTP
		// response. Close the upgraded connection and leave.
		_ = conn.Close()
		return
	}
	ctx = log.Context(ctx, log.FromContext(ctx), "client_id", client.SessionID())
	defer func() { _ = closeFn() }()

	// Set max message size. SetReadLimit only caps the compressed bytes on
	// the wire (gorilla enforces it against the frame payload length before
	// decompression); the decompressed stream is capped independently in the
	// read loop below, see readAllBounded.
	maxSize := h.node.MaxMessageSize()
	if maxSize > 0 {
		conn.SetReadLimit(int64(maxSize))
	}

	// Set read deadline based on heartbeat configuration
	heartbeat := h.node.GetHeartbeatConfig()
	readTimeout := heartbeatReadTimeout(heartbeat.IdleTimeout, heartbeat.PingInterval, h.opt.ReadTimeout)
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))

	for {
		_, r, err := conn.NextReader()
		if err != nil {
			// Control frames (ping/pong/close) are consumed inside
			// NextReader exactly as with ReadMessage, so the close-error
			// classification below is unchanged.
			logReadError(ctx, err)
			break
		}
		data, err := readAllBounded(r, int64(maxSize))
		if err != nil {
			// An over-limit decompressed message takes the same path as a
			// transport read error: break and let the deferred session close
			// shut the connection down, matching the SetReadLimit behavior.
			// No per-frame error envelope is sent (uniform oversize feedback
			// across transports is a separate mechanism task).
			logReadError(ctx, err)
			break
		}
		// Reset read deadline after successful read
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))

		msg := &clientpb.InboundMessage{}
		if err := marshaler.Unmarshal(data, msg); err != nil {
			log.ErrorContext(ctx, "decode client message error", err)
			_ = client.Send(ctx, session.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: &sharedpb.Error{
						Code:    "BAD_REQUEST",
						Type:    "client_error",
						Message: "Failed to decode message",
					},
				}
			}))
			continue
		}

		if err := client.HandleMessage(ctx, msg); err != nil {
			log.ErrorContext(ctx, "handle message error", err)
			continue
		}
	}
}

// marshaler maps the negotiated subprotocol to a Marshaler. The mapping must
// stay in lockstep with msgTypeFromSubprotocol: "messageloop+proto" speaks
// binary protobuf frames, every other negotiated value speaks JSON text
// frames. Unknown subprotocols fall back to JSON rather than matching by
// substring, so names containing "proto" cannot accidentally select the
// protobuf marshaler.
func (h *Handler) marshaler(subProtocol string) shared.Marshaler {
	switch subProtocol {
	case "messageloop+proto":
		return shared.ProtobufMarshaler{}
	default:
		return shared.ProtoJSONMarshaler
	}
}

// errMessageTooLarge reports that the decompressed message exceeded the
// configured max message size. It is only logged and never sent to the peer:
// oversize feedback is handled uniformly across transports elsewhere.
var errMessageTooLarge = errors.New("websocket message exceeds max message size")

// readAllBounded reads a whole message while capping the decompressed output
// at maxSize bytes. This closes a decompression-bomb hole: with
// permessage-deflate negotiated, gorilla's SetReadLimit (and therefore
// ReadMessage's readLimit check) only bounds the compressed bytes on the
// wire, while DEFLATE reaches compression ratios of ~1000:1 — a single
// 64KB frame can expand to ~64MB of buffered output before any decoder
// rejects it, so a handful of concurrent frames can OOM the process.
// Reading maxSize+1 bytes distinguishes an exactly-maxSize message from an
// oversized one. maxSize <= 0 keeps the legacy unlimited behavior.
func readAllBounded(r io.Reader, maxSize int64) ([]byte, error) {
	if maxSize <= 0 {
		return io.ReadAll(r)
	}
	data, err := io.ReadAll(io.LimitReader(r, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSize {
		return nil, errMessageTooLarge
	}
	return data, nil
}

// logReadError classifies a read-loop failure: a close frame from the peer
// (1000/1001) is a normal end of session, everything else — including
// errMessageTooLarge — is logged as an error.
func logReadError(ctx context.Context, err error) {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		log.InfoContext(ctx, "websocket closed normally")
	} else {
		log.ErrorContext(ctx, "websocket read error", err)
	}
}

// heartbeatReadTimeout computes the WebSocket read deadline from the
// heartbeat configuration:
//
//   - idle == 0 && ping == 0: 60s, overridden by an explicit configured
//     value — a heartbeat-disabled connection must not be hit with a 10s
//     floor (that would disconnect silent-but-alive clients).
//   - otherwise: a floor of max(2*idle, 3*ping, 10s) that guarantees the
//     probing window (idle check plus ping deadline) can never be cut short
//     by the read deadline; an explicit configured value may raise but never
//     lower it.
func heartbeatReadTimeout(idle, ping, configured time.Duration) time.Duration {
	if idle == 0 && ping == 0 {
		if configured > 0 {
			return configured
		}
		return 60 * time.Second
	}
	floor := 10 * time.Second
	if t := 2 * idle; t > floor {
		floor = t
	}
	if t := 3 * ping; t > floor {
		floor = t
	}
	if configured > floor {
		return configured
	}
	return floor
}
