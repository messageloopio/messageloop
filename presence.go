package messageloop

import (
	"context"
	"sync"
)

// PresenceInfo holds the state of a client's presence in a channel.
type PresenceInfo struct {
	ClientID    string `json:"client_id"`
	UserID      string `json:"user_id"`
	ConnectedAt int64  `json:"connected_at"`
}

// PresenceStore tracks which clients are currently present in which channels.
// It is separate from Broker so that presence and message routing can be
// implemented and replaced independently.
type PresenceStore interface {
	// Add records that client is present in ch.
	// Called on subscribe; also used to refresh TTL for long-lived connections.
	Add(ctx context.Context, ch string, info *PresenceInfo) error

	// Remove records that client has left ch.
	// Called on unsubscribe and on disconnect.
	Remove(ctx context.Context, ch, clientID string) error

	// Get returns all clients currently present in ch.
	// Returns an empty map (not an error) when no presence data exists.
	Get(ctx context.Context, ch string) (map[string]*PresenceInfo, error)
}

// memoryPresenceStore is the in-process PresenceStore implementation.
type memoryPresenceStore struct {
	mu   sync.RWMutex
	data map[string]map[string]*PresenceInfo // ch -> clientID -> info
}

// NewMemoryPresenceStore returns an in-process PresenceStore.
func NewMemoryPresenceStore() PresenceStore {
	return &memoryPresenceStore{
		data: make(map[string]map[string]*PresenceInfo),
	}
}

func (s *memoryPresenceStore) Add(_ context.Context, ch string, info *PresenceInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[ch]; !ok {
		s.data[ch] = make(map[string]*PresenceInfo)
	}
	cp := *info
	s.data[ch][info.ClientID] = &cp
	return nil
}

func (s *memoryPresenceStore) Remove(_ context.Context, ch, clientID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if clients, ok := s.data[ch]; ok {
		delete(clients, clientID)
		if len(clients) == 0 {
			delete(s.data, ch)
		}
	}
	return nil
}

func (s *memoryPresenceStore) Get(_ context.Context, ch string) (map[string]*PresenceInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	clients, ok := s.data[ch]
	if !ok {
		return map[string]*PresenceInfo{}, nil
	}
	result := make(map[string]*PresenceInfo, len(clients))
	for id, info := range clients {
		cp := *info
		result[id] = &cp
	}
	return result, nil
}

var _ PresenceStore = (*memoryPresenceStore)(nil)
