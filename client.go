package messageloop

import (
	"context"
	"sync"
)

type Client struct {
	mu        sync.RWMutex
	connectMu sync.Mutex // allows syncing connect with disconnect.
	ctx       context.Context
	transport Transport
	uid       string
	session   string
	user      string
	info      []byte
	status    status
}

type status uint8

const (
	statusConnecting status = 1
	statusConnected  status = 2
	statusClosed     status = 3
)
