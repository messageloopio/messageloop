package messageloopgo

import (
	"time"
)

// EncodingType represents the message encoding type.
type EncodingType int

const (
	// EncodingJSON uses JSON encoding (ProtoJSON for protobuf messages)
	EncodingJSON EncodingType = iota
	// EncodingProtobuf uses binary protobuf encoding
	EncodingProtobuf
)

// String returns the string representation of the encoding type.
func (e EncodingType) String() string {
	switch e {
	case EncodingJSON:
		return "json"
	case EncodingProtobuf:
		return "proto"
	default:
		return "json"
	}
}

// Subprotocol returns the WebSocket subprotocol for the encoding.
func (e EncodingType) Subprotocol() string {
	switch e {
	case EncodingJSON:
		return "messageloop+json"
	case EncodingProtobuf:
		return "messageloop+proto"
	default:
		return "messageloop+json"
	}
}

// Options contains client configuration options.
type Options struct {
	// Encoding is the message encoding type (JSON or Protobuf)
	Encoding EncodingType
	// DialTimeout is the timeout for establishing a connection
	DialTimeout time.Duration
	// ClientID is the client identifier
	ClientID string
	// ClientType is the type of client (e.g., "mobile", "web", "server")
	ClientType string
	// Token is the authentication token
	Token string
	// Version is the client version
	Version string
	// AutoSubscribe are channels to automatically subscribe to on connect
	AutoSubscribe []string
	// PingInterval is the interval between ping messages
	PingInterval time.Duration
	// PingTimeout is the timeout for waiting for a pong response
	PingTimeout time.Duration
}

// Option is a function that modifies Options.
type Option func(*Options)

// defaultOptions returns the default options.
func defaultOptions() *Options {
	return &Options{
		Encoding:      EncodingJSON,
		DialTimeout:   10 * time.Second,
		ClientID:      "",
		ClientType:    "sdk",
		Token:         "",
		Version:       "1.0.0",
		AutoSubscribe: nil,
		PingInterval:  30 * time.Second,
		PingTimeout:   10 * time.Second,
	}
}

// WithEncoding sets the message encoding type.
func WithEncoding(enc EncodingType) Option {
	return func(o *Options) {
		o.Encoding = enc
	}
}

// WithDialTimeout sets the connection timeout.
func WithDialTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.DialTimeout = timeout
	}
}

// WithClientID sets the client identifier.
func WithClientID(id string) Option {
	return func(o *Options) {
		o.ClientID = id
	}
}

// WithClientType sets the client type.
func WithClientType(clientType string) Option {
	return func(o *Options) {
		o.ClientType = clientType
	}
}

// WithToken sets the authentication token.
func WithToken(token string) Option {
	return func(o *Options) {
		o.Token = token
	}
}

// WithVersion sets the client version.
func WithVersion(version string) Option {
	return func(o *Options) {
		o.Version = version
	}
}

// WithAutoSubscribe sets channels to automatically subscribe to on connect.
func WithAutoSubscribe(channels ...string) Option {
	return func(o *Options) {
		o.AutoSubscribe = channels
	}
}

// WithPingInterval sets the interval between ping messages.
func WithPingInterval(interval time.Duration) Option {
	return func(o *Options) {
		o.PingInterval = interval
	}
}

// WithPingTimeout sets the timeout for waiting for a pong response.
func WithPingTimeout(timeout time.Duration) Option {
	return func(o *Options) {
		o.PingTimeout = timeout
	}
}
