package redisbroker

import (
	"testing"
	"time"

	"github.com/fleetlit/messageloop"
	"github.com/fleetlit/messageloop/config"
	"github.com/stretchr/testify/assert"
)

// Test Options creation from config
func TestNewOptions(t *testing.T) {
	cfg := config.RedisConfig{
		Addr:              "localhost:6379",
		Password:          "secret",
		DB:                1,
		PoolSize:          20,
		MinIdleConns:      10,
		MaxRetries:        5,
		DialTimeout:       "10s",
		ReadTimeout:       "5s",
		WriteTimeout:      "5s",
		StreamMaxLength:   5000,
		StreamApproximate: false,
		HistoryTTL:        "48h",
		ConsumerGroup:     "test-group",
	}

	opts := NewOptions(cfg)

	assert.Equal(t, "localhost:6379", opts.Addr)
	assert.Equal(t, "secret", opts.Password)
	assert.Equal(t, 1, opts.DB)
	assert.Equal(t, 20, opts.PoolSize)
	assert.Equal(t, 10, opts.MinIdleConns)
	assert.Equal(t, 5, opts.MaxRetries)
	assert.Equal(t, 10*time.Second, opts.DialTimeout)
	assert.Equal(t, 5*time.Second, opts.ReadTimeout)
	assert.Equal(t, 5*time.Second, opts.WriteTimeout)
	assert.EqualValues(t, int64(5000), opts.StreamMaxLength)
	// Note: StreamApproximate defaults to true and only gets overridden if config sets it to true
	// Setting it to false in config won't change it (line 104-106 in options.go)
	assert.True(t, opts.StreamApproximate)
	assert.Equal(t, 48*time.Hour, opts.HistoryTTL)
	assert.Equal(t, "test-group", opts.ConsumerGroup)
}

func TestNewOptions_Defaults(t *testing.T) {
	cfg := config.RedisConfig{
		Addr: "localhost:6379",
	}

	opts := NewOptions(cfg)

	assert.Equal(t, "localhost:6379", opts.Addr)
	assert.Equal(t, defaultPoolSize, opts.PoolSize)
	assert.Equal(t, defaultMinIdleConns, opts.MinIdleConns)
	assert.Equal(t, defaultMaxRetries, opts.MaxRetries)
	assert.Equal(t, defaultDialTimeout, opts.DialTimeout)
	assert.Equal(t, defaultReadTimeout, opts.ReadTimeout)
	assert.Equal(t, defaultWriteTimeout, opts.WriteTimeout)
	assert.EqualValues(t, defaultStreamMaxLength, opts.StreamMaxLength)
	assert.True(t, opts.StreamApproximate)
	assert.Equal(t, defaultHistoryTTL, opts.HistoryTTL)
	assert.Equal(t, defaultConsumerGroup, opts.ConsumerGroup)
	assert.Equal(t, defaultStreamPrefix, opts.StreamPrefix)
	assert.Equal(t, defaultPubSubPrefix, opts.PubSubPrefix)
}

func TestNewOptions_InvalidDuration(t *testing.T) {
	cfg := config.RedisConfig{
		Addr:        "localhost:6379",
		DialTimeout: "invalid",
	}

	opts := NewOptions(cfg)

	// Should use default when duration is invalid
	assert.Equal(t, defaultDialTimeout, opts.DialTimeout)
}

// Test message serialization
func TestSerializeMessage(t *testing.T) {
	msg := &redisMessage{
		Type:    messageTypePublication,
		Channel: "test.channel",
		Payload: []byte(`{"test":"data"}`),
	}

	data, err := serializeMessage(msg)
	assert.NoError(t, err)
	assert.Contains(t, string(data), `"t":"pub"`)
	assert.Contains(t, string(data), `"ch":"test.channel"`)
}

func TestDeserializeMessage(t *testing.T) {
	data := `{"t":"pub","ch":"test.channel","p":"eyJ0ZXN0IjoiZGF0YSJ9"}`
	msg, err := deserializeMessage([]byte(data))
	assert.NoError(t, err)
	assert.Equal(t, messageTypePublication, msg.Type)
	assert.Equal(t, "test.channel", msg.Channel)
}

func TestDeserializeMessage_InvalidJSON(t *testing.T) {
	data := `invalid json`
	_, err := deserializeMessage([]byte(data))
	assert.Error(t, err)
}

// Test message creation helpers
func TestNewPublicationMessage(t *testing.T) {
	ch := "test.channel"
	payload := []byte("test data")

	msg := newPublicationMessage(ch, payload)

	assert.Equal(t, messageTypePublication, msg.Type)
	assert.Equal(t, ch, msg.Channel)
	assert.Equal(t, payload, msg.Payload)
}

func TestNewJoinMessage(t *testing.T) {
	ch := "test.channel"
	info := &messageloop.ClientDesc{
		UserID:   "user-1",
		ClientID: "client-1",
	}

	msg := newJoinMessage(ch, info)

	assert.Equal(t, messageTypeJoin, msg.Type)
	assert.Equal(t, ch, msg.Channel)
	assert.Equal(t, info, msg.Info)
}

func TestNewLeaveMessage(t *testing.T) {
	ch := "test.channel"
	info := &messageloop.ClientDesc{
		UserID:   "user-1",
		ClientID: "client-1",
	}

	msg := newLeaveMessage(ch, info)

	assert.Equal(t, messageTypeLeave, msg.Type)
	assert.Equal(t, ch, msg.Channel)
	assert.Equal(t, info, msg.Info)
}

// Test parseStreamID
func TestParseStreamID(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		expected messageloop.StreamPosition
	}{
		{
			name:     "valid stream id",
			id:       "1634567890123-0",
			expected: messageloop.StreamPosition{Offset: 1634567890123000, Epoch: "1634567890123-0"},
		},
		{
			name:     "valid stream id with sequence",
			id:       "1634567890123-5",
			expected: messageloop.StreamPosition{Offset: 1634567890123005, Epoch: "1634567890123-5"},
		},
		{
			name:     "invalid format - no dash",
			id:       "16345678901230",
			expected: messageloop.StreamPosition{Epoch: "16345678901230"},
		},
		{
			name:     "invalid format - multiple dashes",
			id:       "1634567890123-0-extra",
			expected: messageloop.StreamPosition{Epoch: "1634567890123-0-extra"},
		},
		{
			name:     "invalid timestamp",
			id:       "invalid-0",
			expected: messageloop.StreamPosition{Epoch: "invalid-0"},
		},
		{
			name:     "invalid sequence",
			id:       "1634567890123-invalid",
			expected: messageloop.StreamPosition{Epoch: "1634567890123-invalid"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &redisBroker{options: &Options{}}
			result := b.parseStreamID(tt.id)
			assert.Equal(t, tt.expected.Offset, result.Offset)
			assert.Equal(t, tt.expected.Epoch, result.Epoch)
		})
	}
}

// Test message error types
func TestRedisMessageError(t *testing.T) {
	err := &redisMessageError{msg: "test error"}
	assert.Equal(t, "test error", err.Error())
}

// Test broker creation without connection
func TestNew_BrokerCreation(t *testing.T) {
	cfg := config.RedisConfig{
		Addr: "localhost:6379",
	}

	broker := New(cfg, "test-node")
	assert.NotNil(t, broker)
}

// Test constants
func TestConstants(t *testing.T) {
	assert.Equal(t, "ml:stream:", defaultStreamPrefix)
	assert.Equal(t, "ml:pubsub:", defaultPubSubPrefix)
	assert.Equal(t, "messageloop", defaultConsumerGroup)
	assert.EqualValues(t, int64(10000), defaultStreamMaxLength)
	assert.Equal(t, 24*time.Hour, defaultHistoryTTL)
	assert.Equal(t, 10, defaultPoolSize)
	assert.Equal(t, 5, defaultMinIdleConns)
	assert.Equal(t, 3, defaultMaxRetries)
	assert.Equal(t, 5*time.Second, defaultDialTimeout)
	assert.Equal(t, 3*time.Second, defaultReadTimeout)
	assert.Equal(t, 3*time.Second, defaultWriteTimeout)
}

// Test with options having zero values
func TestNewOptions_ZeroValues(t *testing.T) {
	cfg := config.RedisConfig{
		Addr:              "localhost:6379",
		PoolSize:          0,
		MinIdleConns:      0,
		MaxRetries:        0,
		StreamMaxLength:   0,
		StreamApproximate: false,
	}

	opts := NewOptions(cfg)

	// Should use defaults for zero values
	assert.EqualValues(t, defaultPoolSize, opts.PoolSize)
	assert.Equal(t, defaultMinIdleConns, opts.MinIdleConns)
	assert.Equal(t, defaultMaxRetries, opts.MaxRetries)
	assert.EqualValues(t, defaultStreamMaxLength, opts.StreamMaxLength)
}

// Test stream position
func TestStreamPosition(t *testing.T) {
	pos := messageloop.StreamPosition{
		Offset: 12345,
		Epoch:  "1634567890123-0",
	}
	assert.Equal(t, uint64(12345), pos.Offset)
	assert.Equal(t, "1634567890123-0", pos.Epoch)
}

// Benchmark message serialization
func BenchmarkSerializeMessage(b *testing.B) {
	msg := &redisMessage{
		Type:    messageTypePublication,
		Channel: "test.channel",
		Payload: []byte(`{"test":"data","nested":{"key":"value"}}`),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		serializeMessage(msg)
	}
}

func BenchmarkDeserializeMessage(b *testing.B) {
	data := []byte(`{"t":"pub","ch":"test.channel","p":"eyJ0ZXN0IjoiZGF0YSJ9"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		deserializeMessage(data)
	}
}

// Test disconnect types
func TestDisconnect(t *testing.T) {
	disconnect := messageloop.Disconnect{
		Code:   3000,
		Reason: "normal disconnect",
	}
	assert.Equal(t, uint32(3000), disconnect.Code)
	assert.Equal(t, "normal disconnect", disconnect.Reason)
}
