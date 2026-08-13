package websocket

import (
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
)

type Handler struct {
	node     *messageloop.Node
	opt      *Options
	upgrader *websocket.Upgrader
}

func NewHandler(node *messageloop.Node, opt Options) *Handler {
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
	client, closeFn, err := messageloop.NewClient(ctx, h.node, transport, marshaler, messageloop.WithProtocol("ws"))
	if err != nil {
		log.ErrorContext(r.Context(), "create client error", err)
		// The connection is already upgraded; rw can no longer carry an HTTP
		// response. Close the upgraded connection and leave.
		_ = conn.Close()
		return
	}
	ctx = log.Context(ctx, log.FromContext(ctx), "client_id", client.SessionID())
	defer func() { _ = closeFn() }()

	// Set max message size
	if maxSize := h.node.MaxMessageSize(); maxSize > 0 {
		conn.SetReadLimit(int64(maxSize))
	}

	// Set read deadline based on heartbeat configuration
	readTimeout := 60 * time.Second
	if idleTimeout := h.node.GetHeartbeatIdleTimeout(); idleTimeout > 0 {
		// Use 2 * IdleTimeout as the read deadline to allow for missed heartbeats
		readTimeout = 2 * idleTimeout
	}
	if h.opt.ReadTimeout > 0 {
		readTimeout = h.opt.ReadTimeout
	}
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.InfoContext(ctx, "websocket closed normally")
			} else {
				log.ErrorContext(ctx, "websocket read error", err)
			}
			break
		}
		// Reset read deadline after successful read
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))

		msg := &clientpb.InboundMessage{}
		if err := marshaler.Unmarshal(data, msg); err != nil {
			log.ErrorContext(ctx, "decode client message error", err)
			_ = client.Send(ctx, messageloop.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
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
func (h *Handler) marshaler(subProtocol string) messageloop.Marshaler {
	switch subProtocol {
	case "messageloop+proto":
		return messageloop.ProtobufMarshaler{}
	default:
		return messageloop.ProtoJSONMarshaler
	}
}
