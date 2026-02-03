package messageloop

import (
	"context"
	"sync"
	"time"
)

// SurveyResult represents a response from a client to a survey request.
type SurveyResult struct {
	SessionID string
	Payload   []byte
	Error     error
}

// Survey manages the lifecycle of a survey request and response collection.
type Survey struct {
	id          string
	channel     string
	payload     []byte
	timeout     time.Duration
	responses   map[string]*SurveyResult
	responseCh  chan *SurveyResult
	done        chan struct{}
	mu          sync.Mutex
}

// NewSurvey creates a new Survey instance.
func NewSurvey(id, channel string, payload []byte, timeout time.Duration) *Survey {
	return &Survey{
		id:         id,
		channel:    channel,
		payload:    payload,
		timeout:    timeout,
		responses:  make(map[string]*SurveyResult),
		responseCh: make(chan *SurveyResult, 100),
		done:       make(chan struct{}),
	}
}

// ID returns the survey request ID.
func (s *Survey) ID() string {
	return s.id
}

// Channel returns the target channel for this survey.
func (s *Survey) Channel() string {
	return s.channel
}

// Payload returns the survey request payload.
func (s *Survey) Payload() []byte {
	return s.payload
}

// Timeout returns the survey timeout duration.
func (s *Survey) Timeout() time.Duration {
	return s.timeout
}

// AddResponse adds a client response to the survey.
func (s *Survey) AddResponse(sessionID string, payload []byte, err error) {
	result := &SurveyResult{
		SessionID: sessionID,
		Payload:   payload,
		Error:     err,
	}

	// Add to the map for deduplication and Results() access
	s.mu.Lock()
	s.responses[sessionID] = result
	s.mu.Unlock()

	// Also send to channel for Wait() method
	select {
	case s.responseCh <- result:
	case <-s.done:
		// Survey is closed, don't block
	default:
		// Channel full, try non-blocking
		select {
		case s.responseCh <- result:
		default:
		}
	}
}

// Wait waits for responses until timeout or context cancellation.
// Returns collected results.
func (s *Survey) Wait(ctx context.Context) []*SurveyResult {
	// Create timeout context if not already timed
	timeoutCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	go func() {
		select {
		case <-timeoutCtx.Done():
			close(s.done)
		case <-ctx.Done():
			close(s.done)
		}
	}()

	var results []*SurveyResult
	for {
		select {
		case result, ok := <-s.responseCh:
			if !ok {
				return results
			}
			s.mu.Lock()
			// Deduplicate by session ID - later responses overwrite earlier ones
			s.responses[result.SessionID] = result
			s.mu.Unlock()

		case <-s.done:
			// Collect all responses
			s.mu.Lock()
			for _, r := range s.responses {
				results = append(results, r)
			}
			s.mu.Unlock()
			return results
		}
	}
}

// Close cleans up survey resources.
func (s *Survey) Close() {
	select {
	case <-s.done:
		// Already closed
	default:
		close(s.done)
	}
	// Drain response channel
	select {
	case <-s.responseCh:
	default:
	}
}

// Results returns a copy of the current collected results.
func (s *Survey) Results() []*SurveyResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	results := make([]*SurveyResult, 0, len(s.responses))
	for _, r := range s.responses {
		results = append(results, r)
	}
	return results
}
