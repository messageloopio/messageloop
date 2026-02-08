package redisbroker

import (
	"context"
	"errors"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/redis/go-redis/v9"
)

// getHistory retrieves publications from the history stream.
func (b *redisBroker) getHistory(ch string, opts messageloop.HistoryOptions) ([]*messageloop.Publication, messageloop.StreamPosition, error) {
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()

	stream := b.options.StreamPrefix + ch

	// Get stream info for position
	info, err := b.client.XInfoStream(ctx, stream).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, messageloop.StreamPosition{}, err
	}

	position := messageloop.StreamPosition{
		Offset: uint64(info.Length),
		Epoch:  info.LastGeneratedID,
	}

	// If limit is 0, only return position
	if opts.Filter.Limit == 0 {
		return nil, position, nil
	}

	// Determine range
	var start, end string
	if opts.Filter.Since != nil && opts.Filter.Since.Epoch != "" {
		start = opts.Filter.Since.Epoch
	} else {
		start = "0"
	}
	end = "+"

	// Fetch messages from stream
	var messages []redis.XMessage
	if opts.Filter.Reverse {
		messages, err = b.client.XRevRange(ctx, stream, end, start).Result()
	} else {
		messages, err = b.client.XRange(ctx, stream, start, end).Result()
	}
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, position, err
	}

	// Apply limit
	if opts.Filter.Limit > 0 && len(messages) > opts.Filter.Limit {
		if opts.Filter.Reverse {
			messages = messages[:opts.Filter.Limit]
		} else {
			messages = messages[len(messages)-opts.Filter.Limit:]
		}
	}

	// Convert to publications
	publications := make([]*messageloop.Publication, 0, len(messages))
	for _, msg := range messages {
		pub, err := b.xMessageToPublication(ch, msg)
		if err != nil {
			continue
		}
		publications = append(publications, pub)
	}

	return publications, position, nil
}

// xMessageToPublication converts a Redis XMessage to a Publication.
func (b *redisBroker) xMessageToPublication(ch string, msg redis.XMessage) (*messageloop.Publication, error) {
	data, ok := msg.Values["data"].(string)
	if !ok {
		return nil, errInvalidMessageFormat
	}

	redisMsg, err := deserializeMessage([]byte(data))
	if err != nil {
		return nil, err
	}

	if redisMsg.Type != messageTypePublication {
		return nil, errInvalidMessageType
	}

	position := b.parseStreamID(msg.ID)

	return &messageloop.Publication{
		Channel:  ch,
		Offset:   position.Offset,
		Metadata: nil,
		Payload:  redisMsg.Payload,
		Time:     time.Now().UnixMilli(), // Could extract from message ID timestamp
		IsText:   redisMsg.IsText,
	}, nil
}

var (
	errInvalidMessageFormat = &redisMessageError{"invalid message format"}
	errInvalidMessageType   = &redisMessageError{"invalid message type"}
)

type redisMessageError struct {
	msg string
}

func (e *redisMessageError) Error() string {
	return e.msg
}
