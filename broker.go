package messageloop

import "context"

// Publication is a message published to a channel.
// Offset is 0 when history is disabled for the channel.
type Publication struct {
	Channel string
	Offset  uint64
	Payload []byte
	IsText  bool
	Time    int64
}

// PublicationHandler is called by the broker for each incoming publication
// on a subscribed channel.
type PublicationHandler func(ch string, pub *Publication) error

// Broker manages pub/sub message routing and optional per-channel history.
//
// Lifecycle: Start must be called once before Publish/Subscribe/History.
// Start blocks until the provided context is cancelled — call it as a goroutine:
//
//	go broker.Start(ctx, handler)
type Broker interface {
	// Start initializes the broker and processes events until ctx is done.
	Start(ctx context.Context, handler PublicationHandler) error

	// Subscribe registers the node's interest in ch.
	// Called when the first local client subscribes to a channel.
	Subscribe(ch string) error

	// Unsubscribe removes the node's interest in ch.
	// Called when the last local client unsubscribes from a channel.
	Unsubscribe(ch string) error

	// Publish sends payload to all subscribers of ch.
	// Returns the offset assigned to this publication (0 if history is disabled).
	Publish(ch string, payload []byte, isText bool) (uint64, error)

	// History returns publications stored for ch with offset >= sinceOffset.
	// limit <= 0 means return all available entries.
	// Returns an empty slice (not an error) when history is disabled or empty.
	History(ch string, sinceOffset uint64, limit int) ([]*Publication, error)
}
