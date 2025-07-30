package websocket

import (
	protocol "github.com/deeplooplabs/messageloop-protocol"
	clientv1 "github.com/deeplooplabs/messageloop-protocol/gen/proto/go/client/v1"
	sharedv1 "github.com/deeplooplabs/messageloop-protocol/gen/proto/go/shared/v1"
	"github.com/deeplooplabs/messageloop/messageloop"
	"github.com/gorilla/websocket"
	"github.com/lynx-go/x/log"
	"net/http"
	"strings"
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
	client, closeFn, err := messageloop.NewClient(ctx, h.node, transport, marshaler)
	if err != nil {
		log.ErrorContext(r.Context(), "create client error", err)
		rw.WriteHeader(http.StatusInternalServerError)
		return
	}
	ctx = log.Context(ctx, log.FromContext(ctx), "client_id", client.SessionID())
	defer closeFn()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		msg := &clientv1.ClientMessage{}
		if err := marshaler.Unmarshal(data, msg); err != nil {
			log.ErrorContext(ctx, "decode client message error", err)
			_ = client.Send(ctx, messageloop.MakeServerMessage(nil, func(out *clientv1.ServerMessage) {
				out.Envelope = &clientv1.ServerMessage_Error{
					Error: &sharedv1.Error{
						Code:    int32(messageloop.DisconnectBadRequest.Code),
						Reason:  messageloop.DisconnectBadRequest.Reason,
						Message: "BadRequest",
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
func (h *Handler) marshaler(subProtocols []string) protocol.Marshaler {
	for _, subProtocol := range subProtocols {
		for _, marshaler := range protocol.Marshalers {
			if strings.Contains(subProtocol, marshaler.Name()) {
				return marshaler
			}
		}
	}
	return protocol.ProtoJSONMarshaler
}
