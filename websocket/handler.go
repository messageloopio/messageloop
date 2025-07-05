package websocket

import (
	"context"
	"github.com/deeploopdev/messageloop"
	clientv1 "github.com/deeploopdev/messageloop-protocol/gen/proto/go/client/v1"
	"github.com/gorilla/websocket"
	"github.com/lynx-go/x/log"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
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
				"messageloop+protojson",
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
	ctx = log.Context(ctx, log.FromContext(ctx), "client_id", client.ID())
	defer closeFn()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		msg := &clientv1.ClientMessage{}
		if err := marshaler.Unmarshal(data, msg); err != nil {
			log.ErrorContext(ctx, "decode error", err)
			continue
		}

		if err := client.HandleMessage(ctx, msg); err != nil {
			log.ErrorContext(ctx, "handle message error", err)
			continue
		}
	}
}

func (h *Handler) decodeMessage(ctx context.Context, data []byte, useProto bool) (*clientv1.ClientMessage, error) {
	var msg = &clientv1.ClientMessage{}
	if useProto {
		if err := proto.Unmarshal(data, msg); err != nil {
			return nil, err
		}
		return msg, nil
	} else {
		if err := protojson.Unmarshal(data, msg); err != nil {
			return nil, err
		}
	}

	return msg, nil
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
	return messageloop.DefaultProtoJsonMarshaler
}
