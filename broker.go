package messageloop

import "time"

type Publication struct {
	Channel  string
	Offset   uint64
	Metadata map[string]interface{}
	IsBlob   bool
	Payload  []byte
	Time     int64
}
type StreamPosition struct {
	// Offset defines publication incremental offset inside a stream.
	Offset uint64
	// Epoch allows handling situations when storage
	// lost stream entirely for some reason (expired or lost after restart) and we
	// want to track this fact to prevent successful recovery from another stream.
	// I.e. for example we have a stream [1, 2, 3], then it's lost and new stream
	// contains [1, 2, 3, 4], client that recovers from position 3 will only receive
	// publication 4 missing 1, 2, 3 from new stream. With epoch, we can tell client
	// that correct recovery is not possible.
	Epoch string
}

// HistoryFilter allows filtering history according to fields set.
type HistoryFilter struct {
	// Since used to extract publications from stream since provided StreamPosition.
	Since *StreamPosition
	// Limit number of publications to return.
	// -1 means no limit - i.e. return all publications currently in stream.
	// 0 means that caller only interested in current stream top position so
	// Broker should not return any publications.
	Limit int
	// Reverse direction.
	Reverse bool
}

// HistoryOptions define some fields to alter History method behaviour.
type HistoryOptions struct {
	// Filter for history publications.
	Filter HistoryFilter
	// MetaTTL allows overriding default (set in Config.HistoryMetaTTL) history
	// meta information expiration time.
	MetaTTL time.Duration
}

type PublishOption func(*PublishOptions)

// WithClientDesc adds ClientDesc to Publication.
func WithClientDesc(info *ClientDesc) PublishOption {
	return func(opts *PublishOptions) {
		opts.ClientDesc = info
	}
}

func WithAsBytes(asBytes bool) PublishOption {
	return func(opts *PublishOptions) {
		opts.AsBytes = asBytes
	}
}

type PublishOptions struct {
	ClientDesc *ClientDesc
	AsBytes    bool
}

// BrokerEventHandler can handle messages received from PUB/SUB system.
type BrokerEventHandler interface {
	// HandlePublication to handle received Publications.
	HandlePublication(ch string, pub *Publication) error
	// HandleJoin to handle received Join messages.
	HandleJoin(ch string, info *ClientDesc) error
	// HandleLeave to handle received Leave messages.
	HandleLeave(ch string, info *ClientDesc) error
}

type Broker interface {
	// RegisterEventHandler called once on start when Broker already set to Node. At
	// this moment node is ready to process broker events.
	RegisterEventHandler(BrokerEventHandler) error

	// Subscribe node on channel to listen all messages coming from channel.
	Subscribe(ch string) error
	// Unsubscribe node from channel to stop listening messages from it.
	Unsubscribe(ch string) error

	// Publish allows sending data into channel. Data should be
	// delivered to all clients subscribed to this channel at moment on any
	// Centrifuge node (with at most once delivery guarantee).
	//
	// Broker can optionally maintain publication history inside channel according
	// to PublishOptions provided. See History method for rules that should be implemented
	// for accessing publications from history stream.
	//
	// Saving message to a history stream and publish to PUB/SUB should be an atomic
	// operation per channel. If this is not true – then publication to one channel
	// must be serialized on the caller side, i.e. publish requests must be issued one
	// after another. Otherwise, the order of publications and stable behaviour of
	// subscribers with positioning/recovery enabled can't be guaranteed.
	//
	// StreamPosition returned here describes stream epoch and offset assigned to
	// the publication. For channels without history this StreamPosition should be
	// zero value.
	// Second bool value returned here means whether Publish was suppressed due to
	// the use of PublishOptions.IdempotencyKey. In this case StreamPosition is
	// returned from the cache maintained by Broker.
	Publish(ch string, data []byte, opts PublishOptions) (StreamPosition, bool, error)
	// PublishJoin publishes Join Push message into channel.
	PublishJoin(ch string, info *ClientDesc) error
	// PublishLeave publishes Leave Push message into channel.
	PublishLeave(ch string, info *ClientDesc) error

	// History used to extract Publications from history stream.
	// Publications returned according to HistoryFilter which allows to set several
	// filtering options. StreamPosition returned describes current history stream
	// top offset and epoch.
	History(ch string, opts HistoryOptions) ([]*Publication, StreamPosition, error)
	// RemoveHistory removes history from channel. This is in general not
	// needed as history expires automatically (based on history_lifetime)
	// but sometimes can be useful for application logic.
	RemoveHistory(ch string) error
}
