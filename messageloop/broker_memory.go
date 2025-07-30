package messageloop

import (
	"time"
)

func newMemoryBroker(node *Node) *memoryBroker {
	return &memoryBroker{
		node: node,
	}
}

type memoryBroker struct {
	eventHandler BrokerEventHandler
	node         *Node
}

func (b *memoryBroker) RegisterBrokerEventHandler(handler BrokerEventHandler) error {
	b.eventHandler = handler
	return nil
}

func (b *memoryBroker) Subscribe(ch string) error {
	return nil
}

func (b *memoryBroker) Unsubscribe(ch string) error {
	return nil
}

func (b *memoryBroker) Publish(ch string, payload []byte, opts PublishOptions) (StreamPosition, bool, error) {
	return StreamPosition{}, false, b.eventHandler.HandlePublication(ch, &Publication{
		Channel:  ch,
		Offset:   0,
		Metadata: nil,
		Payload:  payload,
		Time:     time.Now().UnixMilli(),
		IsBlob:   opts.AsBytes,
	})
}

func (b *memoryBroker) PublishJoin(ch string, info *ClientInfo) error {
	return nil
}

func (b *memoryBroker) PublishLeave(ch string, info *ClientInfo) error {
	return nil
}

func (b *memoryBroker) History(ch string, opts HistoryOptions) ([]*Publication, StreamPosition, error) {
	return nil, StreamPosition{}, nil
}

func (b *memoryBroker) RemoveHistory(ch string) error {
	return nil
}

var _ Broker = new(memoryBroker)
