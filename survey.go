package messageloop

import (
	"context"
	"sync"
	"time"
)

// SurveyResult represents a response from a client to a survey request.
type SurveyResult struct {
	SessionID     string
	NodeID        string
	IncarnationID string
	Payload       []byte
	Error         error
}

// Survey manages the lifecycle of a survey request and response collection.
type Survey struct {
	id         string
	channel    string
	payload    []byte
	timeout    time.Duration
	responses  map[string]*SurveyResult
	responseCh chan *SurveyResult
	done       chan struct{}
	closeOnce  sync.Once
	mu         sync.Mutex
	// expected holds the session IDs the survey request was sent to. Only
	// these sessions are allowed to respond; responses from other sessions
	// are treated as forged and rejected by the node.
	expected map[string]struct{}
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
		expected:   make(map[string]struct{}),
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

// AddExpectedSession registers a session that the survey request was sent to
// and is therefore allowed to respond.
func (s *Survey) AddExpectedSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expected[sessionID] = struct{}{}
}

// IsExpectedSession reports whether the given session may respond to this
// survey, i.e. whether it was one of the subscribers the request went to.
func (s *Survey) IsExpectedSession(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.expected[sessionID]
	return ok
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

	// Also send to channel for Wait() method. The map write above already
	// captured the response, so a full or closed channel never loses it:
	// Wait() collects the map once done is signaled. No warning is needed
	// when the channel is full.
	select {
	case s.responseCh <- result:
	case <-s.done:
	default:
	}
}

// defaultSurveyWaitTimeout is the fallback wait duration when the survey
// timeout is <= 0, which would otherwise make Wait expire immediately.
// Variable for testability.
var defaultSurveyWaitTimeout = 5 * time.Second

// Wait waits for responses until timeout or context cancellation.
// Returns collected results.
func (s *Survey) Wait(ctx context.Context) []*SurveyResult {
	// A non-positive timeout means "expire immediately", which turns every
	// survey into a no-op; fall back to the default wait instead.
	timeout := s.timeout
	if timeout <= 0 {
		timeout = defaultSurveyWaitTimeout
	}

	// Create timeout context if not already timed
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	go func() {
		select {
		case <-timeoutCtx.Done():
			s.closeDone()
		case <-ctx.Done():
			s.closeDone()
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

// closeDone safely closes the done channel exactly once.
func (s *Survey) closeDone() {
	s.closeOnce.Do(func() { close(s.done) })
}

// Close cleans up survey resources.
func (s *Survey) Close() {
	s.closeDone()
	// Drain the response channel so late senders never block: a single
	// non-blocking receive could leave items behind.
	for {
		select {
		case <-s.responseCh:
		default:
			return
		}
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
