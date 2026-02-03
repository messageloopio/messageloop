package websocket

import (
	"net/http"
	"strings"
	"time"

	"github.com/fleetlit/messageloop"
	sharedpb "github.com/fleetlit/messageloop/genproto/shared/v1"
	clientpb "github.com/fleetlit/messageloop/genproto/v1"
	"github.com/gorilla/websocket"
	"github.com/lynx-go/x/log"
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
		},
	}
	return handler
}

func (h *Handler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(rw, r, nil)
	if err != nil {
		log.ErrorContext(r.Context(), "websocket upgrade error", err)
		rw.WriteHeader(http.StatusInternalServerError)
		return
	}

	subProtocols := websocket.Subprotocols(r)
	marshaler := h.marshaler(subProtocols)
	transport := newTransport(conn, marshaler)
	ctx := r.Context()
	client, closeFn, err := messageloop.NewClientSession(ctx, h.node, transport, marshaler)
	if err != nil {
		log.ErrorContext(r.Context(), "create client error", err)
		rw.WriteHeader(http.StatusInternalServerError)
		return
	}
	ctx = log.Context(ctx, log.FromContext(ctx), "client_id", client.SessionID())
	defer closeFn()

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

// 通过 subProtocols 确定 marshaler
func (h *Handler) marshaler(subProtocols []string) messageloop.Marshaler {
	for _, subProtocol := range subProtocols {
		for _, marshaler := range messageloop.Marshalers {
			if strings.Contains(subProtocol, marshaler.Name()) {
				return marshaler
			}
		}
	}
	return messageloop.ProtoJSONMarshaler
}
