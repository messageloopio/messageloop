package messageloop

import (
	"context"
	"encoding/json"
	"time"

	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
	"google.golang.org/protobuf/types/known/structpb"
)

// PayloadKind identifies the original Payload oneof variant of a publication.
type PayloadKind int

const (
	// PayloadKindBinary marks a binary payload.
	PayloadKindBinary PayloadKind = iota
	// PayloadKindText marks a text payload.
	PayloadKindText
	// PayloadKindJSON marks a JSON payload (Payload_Payload_Json on the wire).
	PayloadKindJSON
)

// Publication is a message published to a channel.
// Offset is 0 when history is disabled for the channel.
type Publication struct {
	Channel     string
	Payload     []byte // payload bytes (JSON text for JSON kind)
	Kind        PayloadKind
	ContentType string // MIME content type, may be empty
	Id          string // publisher-provided message id, may be empty
	Metadata    map[string]string
	Offset      uint64
	// Seq is the dense per-channel sequence number (1, 2, 3, ...) assigned
	// atomically with the history stream append (Redis broker, C4). It is 0
	// for transient publications, legacy entries written before C4, and the
	// memory broker (whose Offset already is the dense seq).
	Seq         uint64
	Time        int64 // Unix milliseconds
	Epoch       string
	// HistorySize caps this publication's channel history ring/stream when
	// the channel is first created. 0 = the broker global default. Only
	// Publish uses it; PublishTransient never writes history.
	HistorySize int
	// HistoryTTL overrides the broker's history retention TTL for this
	// publication (Redis only; the memory broker ignores it and warns once
	// per channel). 0 = the broker global default.
	HistoryTTL time.Duration
}

// PayloadProtoV2 rebuilds the shared.v2 Payload message from the
// publication, preserving the original oneof variant (Binary/Text/JSON).
func (p *Publication) PayloadProtoV2() *sharedv2.Payload {
	if p == nil || len(p.Payload) == 0 {
		return nil
	}
	switch p.Kind {
	case PayloadKindText:
		return &sharedv2.Payload{
			ContentType: p.ContentType,
			Data:        &sharedv2.Payload_Text{Text: string(p.Payload)},
		}
	case PayloadKindJSON:
		var object map[string]any
		if err := json.Unmarshal(p.Payload, &object); err == nil {
			if st, err := structpb.NewStruct(object); err == nil {
				return &sharedv2.Payload{
					ContentType: p.ContentType,
					Data:        &sharedv2.Payload_Json{Json: st},
				}
			}
		}
		// Not valid JSON after all: degrade to text and let the caller log.
		return &sharedv2.Payload{
			ContentType: p.ContentType,
			Data:        &sharedv2.Payload_Text{Text: string(p.Payload)},
		}
	default:
		return &sharedv2.Payload{
			ContentType: p.ContentType,
			Data:        &sharedv2.Payload_Binary{Binary: p.Payload},
		}
	}
}

// PublicationHandler is called by the broker for each incoming publication
// on a subscribed channel.
type PublicationHandler func(ch string, pub *Publication) error

// OccupancyHandler is invoked for live occupancy events. It must not be the
// publication handler. Errors are logged; they never fail Join/Leave
// (KD-K14).
type OccupancyHandler func(channel string, evt OccupancyEvent) error

// CatchUpGap describes one hole detected during reconnect catch-up (C6).
type CatchUpGap struct {
	Channel string
	// Reason is HistoryGapMiddle (dense-seq discontinuity inside the replayed
	// range) or HistoryGapReplayTruncated (replay batch truncated by the
	// limit while newer entries exist).
	Reason HistoryGapReason
	// LastGoodSeq is the dense seq of the last continuous entry before the
	// hole (middle gaps); 0 = unknown.
	LastGoodSeq uint64
	// LastGoodOffset is the ts<<20|seq offset of the last safely delivered
	// entry; 0 = unknown.
	LastGoodOffset uint64
}

// GapHandler is invoked when reconnect catch-up detects a hole (C6). It runs
// on the catch-up path: a panic is recovered and logged, never propagated.
type GapHandler func(gap CatchUpGap)

// HistoryGapReason classifies why a history page cannot prove full coverage
// of the requested offset range.
type HistoryGapReason int

const (
	// HistoryGapNone means the retained entries provably cover the requested
	// offset range (including reading from the beginning with sinceOffset 0).
	HistoryGapNone HistoryGapReason = iota
	// HistoryGapHeadTrimmed means retained entries start after sinceOffset:
	// the head was trimmed and entries between sinceOffset and the first
	// retained offset may be missing.
	HistoryGapHeadTrimmed
	// HistoryGapEmptyExpired means nothing is retained and it cannot be
	// proven that the retained region still covers sinceOffset (e.g. the
	// channel never had entries, or its history expired/never existed).
	// Prefer false positives: a client-supplied garbage offset gets this
	// reason too.
	HistoryGapEmptyExpired
	// HistoryGapMiddle means two adjacent retained entries carry dense
	// sequence numbers that are not consecutive: at least one entry was
	// deleted from the middle of the retained range (e.g. XDEL). Entries
	// without a dense seq (legacy) break the evidence chain and never
	// trigger this reason.
	HistoryGapMiddle
	// HistoryGapReplayTruncated means a reconnect catch-up replay batch was
	// cut short by the replay limit while the stream still held newer
	// entries: the tail beyond the replayed range was not delivered live.
	// The entries remain in history and clients can recover them (C6).
	HistoryGapReplayTruncated
)

// HistoryPage is one page of channel history plus gap metadata.
type HistoryPage struct {
	Publications  []*Publication
	Truncated     bool            // len(Publications) == limit && limit > 0
	Gap           bool            // GapReason != HistoryGapNone
	GapReason     HistoryGapReason
	FirstRetained uint64          // oldest retained offset; 0 = unknown / never published
}

// Pubs returns the page publications, nil for a nil page.
func (p *HistoryPage) Pubs() []*Publication {
	if p == nil {
		return nil
	}
	return p.Publications
}

// Broker manages pub/sub message routing and optional per-channel history.
//
// Delivery error contract: a handler error means the delivery to local
// subscribers failed, not the publish itself. Neither implementation
// propagates delivery errors to Publish callers — the publication has already
// been accepted by the transport by the time the handler runs, and delivery
// failures are logged/metrics events there. Publish callers must not rely on
// the handler's return value for either implementation; a successful Publish
// only promises the message was accepted (and written to history when
// history applies).
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
	// Returns the offset assigned to this publication (0 if history is
	// disabled). The assigned offset is also written back to pub.Offset.
	// Channels with explicit empty segments ("a.", ".a", "a..b") and the
	// empty channel are rejected with topics.ErrBadTopic before any
	// publication side effect.
	Publish(ch string, pub *Publication) (uint64, error)

	// PublishTransient delivers payload to all subscribers of ch in real
	// time without writing history, so the publication never appears in
	// History. Used for events (e.g. presence join/leave) that must not
	// leak into the recovery message stream. Malformed channels are rejected
	// with topics.ErrBadTopic like Publish.
	PublishTransient(ch string, pub *Publication) error

	// PublishOccupancy fans an occupancy event on the live bus for exact
	// channel ch. It never writes Stream/history. Delivery follows Interest
	// (exact or compiled pattern): only a node interested in ch invokes its
	// occupancy handler. Handler errors do not fail the call (KD-K14).
	PublishOccupancy(ch string, evt OccupancyEvent) error

	// SetOccupancyHandler registers the live occupancy handler; it must be
	// called before Start. The publication handler never receives occupancy.
	SetOccupancyHandler(handler OccupancyHandler) error

	// SetGapHandler registers the catch-up gap handler (C6); nil disables
	// client notification. Detection itself (counters and warnings) is
	// unaffected by the handler being nil, and a handler panic must never
	// break catch-up. At most one notification is emitted per channel per
	// catch-up pass (the first detected hole wins). The handler runs on the
	// broker's catch-up path and must not block. The memory broker has no
	// catch-up concept: its SetGapHandler is a no-op.
	SetGapHandler(handler GapHandler)

	// History returns a page of publications stored for ch with offset >=
	// sinceOffset, plus gap metadata (see HistoryPage). limit <= 0 uses
	// DefaultHistoryLimit as a safety cap. An empty page is not an error;
	// error is reserved for transport/storage failures. Gap detection must
	// never report HistoryGapNone for an empty batch with sinceOffset > 0.
	// A retained range whose dense per-channel seqs (Publication.Seq) are
	// not consecutive reports HistoryGapMiddle (C4); entries without a dense
	// seq (legacy) never trigger it.
	History(ch string, sinceOffset uint64, limit int) (*HistoryPage, error)
}
