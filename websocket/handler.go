package websocket

import (
	clientv1 "github.com/deeplooplabs/messageloop-protocol/gen/proto/go/client/v1"
	sharedv1 "github.com/deeplooplabs/messageloop-protocol/gen/proto/go/shared/v1"
	messageloop2 "github.com/deeplooplabs/messageloop/messageloop"
	"github.com/gorilla/websocket"
	"github.com/lynx-go/x/log"
	"net/http"
	"strings"
)

type Handler struct {
	node     *messageloop2.Node
	opt      *Options
	upgrader *websocket.Upgrader
}

func NewHandler(node *messageloop2.Node, opt Options) *Handler {
	handler := &Handler{
		node: node,
		opt:  &opt,
		upgrader: &websocket.Upgrader{
			Subprotocols: []string{
				"messageloop",
				"messageloop+json",
				"messageloop+proto",
				//"messageloop+protojson",
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
	client, closeFn, err := messageloop2.NewClient(ctx, h.node, transport, marshaler)
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
			_ = client.Send(ctx, messageloop2.MakeServerMessage(nil, func(out *clientv1.ServerMessage) {
				out.Envelope = &clientv1.ServerMessage_Error{
					Error: &sharedv1.Error{
						Code:    int32(messageloop2.DisconnectBadRequest.Code),
						Reason:  messageloop2.DisconnectBadRequest.Reason,
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
func (h *Handler) marshaler(subProtocols []string) messageloop2.Marshaler {
	for _, subProtocol := range subProtocols {
		for _, marshaler := range messageloop2.Marshalers {
			if strings.Contains(subProtocol, marshaler.Name()) {
				return marshaler
			}
		}
	}
	return messageloop2.DefaultProtoJsonMarshaler
}
