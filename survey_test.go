package messageloop

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/messageloopio/messageloop/config"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// userPrincipal is a local copy of the helper that moved with the authorizer
// tests to internal/authz in PR-KA-D12.
func userPrincipal(userID string) Principal {
	return Principal{Kind: PrincipalUser, UserID: userID}
}

func TestSurvey_NewSurvey(t *testing.T) {
	id := "test-survey-123"
	channel := "test-channel"
	payload := []byte("test payload")
	timeout := 5 * time.Second

	survey := NewSurvey(id, channel, payload, timeout)

	if survey.ID() != id {
		t.Errorf("expected ID %s, got %s", id, survey.ID())
	}
	if survey.Channel() != channel {
		t.Errorf("expected channel %s, got %s", channel, survey.Channel())
	}
	if string(survey.Payload()) != string(payload) {
		t.Errorf("expected payload %s, got %s", payload, survey.Payload())
	}
	if survey.Timeout() != timeout {
		t.Errorf("expected timeout %v, got %v", timeout, survey.Timeout())
	}
}

func TestSurvey_AddResponse(t *testing.T) {
	survey := NewSurvey("test-id", "test-channel", []byte("payload"), 5*time.Second)

	// Add a response
	survey.AddResponse("session-1", []byte("response-1"), nil)

	// Check that the response was added
	results := survey.Results()
	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}
	if results[0].SessionID != "session-1" {
		t.Errorf("expected session-1, got %s", results[0].SessionID)
	}
}

func TestSurvey_AddError(t *testing.T) {
	survey := NewSurvey("test-id", "test-channel", []byte("payload"), 5*time.Second)

	expectedErr := context.DeadlineExceeded
	survey.AddResponse("session-1", nil, expectedErr)

	results := survey.Results()
	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}
	if results[0].Error != expectedErr {
		t.Errorf("expected error %v, got %v", expectedErr, results[0].Error)
	}
}

func TestSurvey_Deduplication(t *testing.T) {
	survey := NewSurvey("test-id", "test-channel", []byte("payload"), 5*time.Second)

	// Add multiple responses from the same session
	survey.AddResponse("session-1", []byte("response-1"), nil)
	survey.AddResponse("session-1", []byte("response-2"), nil)
	survey.AddResponse("session-1", []byte("response-3"), nil)

	// Should only have one result due to deduplication
	results := survey.Results()
	if len(results) != 1 {
		t.Errorf("expected 1 result (deduplicated), got %d", len(results))
	}
}

func TestSurvey_Wait_Timeout(t *testing.T) {
	survey := NewSurvey("test-id", "test-channel", []byte("payload"), 100*time.Millisecond)

	// Add a response before waiting
	survey.AddResponse("session-1", []byte("response-1"), nil)

	// Wait with a timeout context
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	results := survey.Wait(ctx)

	// Should have 1 result
	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}

	survey.Close()
}

func TestSurvey_Wait_ContextCancellation(t *testing.T) {
	survey := NewSurvey("test-id", "test-channel", []byte("payload"), 5*time.Second)

	// Add a response before waiting
	survey.AddResponse("session-1", []byte("response-1"), nil)

	// Wait with a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	results := survey.Wait(ctx)

	// Should have 1 result (already added)
	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}

	survey.Close()
}

func TestSurvey_MultipleResponses(t *testing.T) {
	survey := NewSurvey("test-id", "test-channel", []byte("payload"), 5*time.Second)

	// Add multiple responses
	survey.AddResponse("session-1", []byte("response-1"), nil)
	survey.AddResponse("session-2", []byte("response-2"), nil)
	survey.AddResponse("session-3", []byte("response-3"), nil)

	ctx := context.Background()
	results := survey.Wait(ctx)

	if len(results) != 3 {
		t.Errorf("expected 3 results, got %d", len(results))
	}

	survey.Close()
}

func TestSurvey_Close(t *testing.T) {
	survey := NewSurvey("test-id", "test-channel", []byte("payload"), 5*time.Second)

	// Close should not panic
	survey.Close()
	survey.Close() // Calling Close multiple times should be safe
}

func TestSurvey_EmptyChannel(t *testing.T) {
	// This tests the Node.Survey behavior with no subscribers
	survey := NewSurvey("test-id", "empty-channel", []byte("payload"), 5*time.Second)

	// Wait without adding any responses
	ctx := context.Background()
	results := survey.Wait(ctx)

	// Should have 0 results
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}

	survey.Close()
}

// End-to-end Survey tests

func TestNode_Survey_Basic(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())
	ctx := context.Background()

	const numClients = 3
	transports := make([]*capturingTransport, numClients)
	clients := make([]*Client, numClients)

	// Create clients and subscribe them to the channel
	for i := 0; i < numClients; i++ {
		transports[i] = &capturingTransport{}
		var err error
		clients[i], _, err = NewClient(ctx, node, transports[i], JSONMarshaler{})
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		// Authenticate
		connectMsg := &clientpb.InboundMessage{
			Id: "msg-connect-" + string(rune('0'+i)),
			Envelope: &clientpb.InboundMessage_Connect{
				Connect: &clientpb.Connect{Version: testProtocolVersion, ClientId: "client-" + string(rune('0'+i))},
			},
		}
		err = clients[i].HandleMessage(ctx, connectMsg)
		if err != nil {
			t.Fatalf("HandleMessage() Connect error = %v", err)
		}

		// Clear transport messages from connect
		transports[i].messages = nil

		// Subscribe to channel (use unique channel for this test)
		subMsg := &clientpb.InboundMessage{
			Id: "msg-sub-" + string(rune('0'+i)),
			Envelope: &clientpb.InboundMessage_Subscribe{
				Subscribe: &clientpb.Subscribe{
					Subscriptions: []*clientpb.Subscription{
						{Channel: "survey-channel-basic"},
					},
				},
			},
		}
		err = clients[i].HandleMessage(ctx, subMsg)
		if err != nil {
			t.Fatalf("HandleMessage() Subscribe error = %v", err)
		}

		// Clear transport messages from subscribe
		transports[i].messages = nil
	}

	// Verify all clients are subscribed
	subCount := node.Hub().NumSubscribers("survey-channel-basic")
	if subCount != numClients {
		t.Errorf("Expected %d subscribers, got %d", numClients, subCount)
	}

	// Start survey in a goroutine so we can process client responses
	surveyPayload := []byte("survey request payload")
	var surveyResults []*SurveyResult
	var surveyErr error
	var surveyWg sync.WaitGroup
	surveyWg.Add(1)
	go func() {
		defer surveyWg.Done()
		surveyResults, surveyErr = node.Survey(ctx, "survey-channel-basic", surveyPayload, 5*time.Second)
	}()

	// Give survey requests time to be sent and processed
	t.Log("Waiting for survey requests...")
	time.Sleep(500 * time.Millisecond)
	t.Log("After waiting, checking messages...")

	// Debug: check if survey has registered
	subscribers := node.Hub().GetSubscribers("survey-channel-basic")
	t.Logf("Subscribers after survey started: %d", len(subscribers))

	// Process survey requests from transports: read the outbound
	// SurveyRequest.request_id (never feed the inbound request back — the
	// echo is gone since PR-07).
	requestIDs := make([]string, numClients)
	for i := 0; i < numClients; i++ {
		msgCount := transports[i].getMessageCount()
		if msgCount > 0 {
			// Parse the received message to get the request ID
			data := transports[i].getLastMessage()
			if len(data) > 0 {
				var msg clientpb.OutboundMessage
				var m JSONMarshaler
				if err := m.Unmarshal(data, &msg); err == nil {
					if sr := msg.GetSurveyRequest(); sr != nil {
						requestIDs[i] = sr.RequestId
					}
				}
			}
		}
	}

	// Send survey responses back
	for i := 0; i < numClients; i++ {
		responseMsg := &clientpb.InboundMessage{
			Id: "msg-survey-resp-" + string(rune('0'+i)),
			Envelope: &clientpb.InboundMessage_SurveyReply{
				SurveyReply: &clientpb.SurveyReply{
					RequestId: requestIDs[i],
					Payload: &sharedpb.Payload{
						Data: &sharedpb.Payload_Binary{Binary: []byte("response from client " + string(rune('0'+i)))},
					},
				},
			},
		}
		_ = clients[i].HandleMessage(ctx, responseMsg)
	}

	// Wait for survey to complete
	surveyWg.Wait()

	// Check survey results
	if surveyErr != nil {
		t.Fatalf("Survey() error = %v", surveyErr)
	}

	// Results should include all clients
	if len(surveyResults) != numClients {
		t.Errorf("Expected %d results, got %d", numClients, len(surveyResults))
	}
}

func TestNode_Survey_AllClientsRespond(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())
	ctx := context.Background()

	const numClients = 3
	transports := make([]*capturingTransport, numClients)
	clients := make([]*Client, numClients)

	// Create clients and subscribe them to the channel
	for i := 0; i < numClients; i++ {
		transports[i] = &capturingTransport{}
		var err error
		clients[i], _, err = NewClient(ctx, node, transports[i], JSONMarshaler{})
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		// Authenticate
		connectMsg := &clientpb.InboundMessage{
			Id: "msg-connect-" + string(rune('0'+i)),
			Envelope: &clientpb.InboundMessage_Connect{
				Connect: &clientpb.Connect{Version: testProtocolVersion, ClientId: "client-" + string(rune('0'+i))},
			},
		}
		err = clients[i].HandleMessage(ctx, connectMsg)
		if err != nil {
			t.Fatalf("HandleMessage() Connect error = %v", err)
		}

		// Clear transport messages from connect
		transports[i].messages = nil

		// Subscribe to channel (use unique channel for this test)
		subMsg := &clientpb.InboundMessage{
			Id: "msg-sub-" + string(rune('0'+i)),
			Envelope: &clientpb.InboundMessage_Subscribe{
				Subscribe: &clientpb.Subscribe{
					Subscriptions: []*clientpb.Subscription{
						{Channel: "survey-channel-respond"},
					},
				},
			},
		}
		err = clients[i].HandleMessage(ctx, subMsg)
		if err != nil {
			t.Fatalf("HandleMessage() Subscribe error = %v", err)
		}

		// Clear transport messages from subscribe
		transports[i].messages = nil
	}

	// Debug: Check subscribers before survey
	subscribers := node.Hub().GetSubscribers("survey-channel-respond")
	t.Logf("Subscribers before survey: %d", len(subscribers))

	// Call Survey with a longer timeout to allow all responses
	surveyPayload := []byte("survey request payload")

	// Start survey in goroutine to allow clients to respond
	var surveyResults []*SurveyResult
	var surveyErr error
	var surveyWg sync.WaitGroup
	surveyWg.Add(1)
	go func() {
		defer surveyWg.Done()
		surveyResults, surveyErr = node.Survey(ctx, "survey-channel-respond", surveyPayload, 2*time.Second)
	}()

	// Give survey requests time to be sent
	time.Sleep(500 * time.Millisecond)

	// Debug: Check message counts
	requestIDs := make([]string, numClients)
	for i := 0; i < numClients; i++ {
		msgCount := transports[i].getMessageCount()
		t.Logf("Client %d: %d messages received", i, msgCount)
		if msgCount > 0 {
			// Parse the received message to get the request ID
			// The message was sent with JSONMarshaler
			data := transports[i].getLastMessage()
			if len(data) > 0 {
				// Unmarshal to get the survey request
				var msg clientpb.OutboundMessage
				var m JSONMarshaler
				if err := m.Unmarshal(data, &msg); err == nil {
					if sr := msg.GetSurveyRequest(); sr != nil {
						t.Logf("Client %d: parsed request ID: %s", i, sr.RequestId)
						requestIDs[i] = sr.RequestId
					}
				}
			}
		}
	}

	// Now send the survey responses back to the server
	for i := 0; i < numClients; i++ {
		t.Logf("Client %d: sending response with request ID: %s", i, requestIDs[i])

		responseMsg := &clientpb.InboundMessage{
			Id: "msg-survey-resp-" + string(rune('0'+i)),
			Envelope: &clientpb.InboundMessage_SurveyReply{
				SurveyReply: &clientpb.SurveyReply{
					RequestId: requestIDs[i],
					Payload: &sharedpb.Payload{
						Data: &sharedpb.Payload_Binary{Binary: []byte("response from client " + string(rune('0'+i)))},
					},
				},
			},
		}
		_ = clients[i].HandleMessage(ctx, responseMsg)
	}

	surveyWg.Wait()

	if surveyErr != nil {
		t.Fatalf("Survey() error = %v", surveyErr)
	}

	// Verify we got responses from all clients
	if len(surveyResults) != numClients {
		t.Errorf("Expected %d results, got %d", numClients, len(surveyResults))
	}

	// Verify each client session is in the results
	sessionIDs := make(map[string]bool)
	for _, r := range surveyResults {
		sessionIDs[r.SessionID] = true
	}
	for i := 0; i < numClients; i++ {
		if !sessionIDs[clients[i].SessionID()] {
			t.Errorf("Missing response from client %d (session %s)", i, clients[i].SessionID())
		}
	}
}

func TestNode_Survey_NoSubscribers(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())
	ctx := context.Background()

	// Survey on a channel with no subscribers
	results, err := node.Survey(ctx, "empty-channel", []byte("payload"), 5*time.Second)
	if err != nil {
		t.Fatalf("Survey() error = %v", err)
	}

	// Should return empty results, not error
	if len(results) != 0 {
		t.Errorf("Expected 0 results for empty channel, got %d", len(results))
	}
}

func TestNode_Survey_ConcurrentClients(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())
	ctx := context.Background()

	const numClients = 10
	var wg sync.WaitGroup
	transports := make([]*capturingTransport, numClients)
	clients := make([]*Client, numClients)
	errCh := make(chan error, numClients)

	// Create clients concurrently
	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			transports[i] = &capturingTransport{}
			var err error
			clients[i], _, err = NewClient(ctx, node, transports[i], JSONMarshaler{})
			if err != nil {
				errCh <- err
				return
			}

			// Authenticate
			connectMsg := &clientpb.InboundMessage{
				Id: "msg-connect-" + string(rune('0'+i)),
				Envelope: &clientpb.InboundMessage_Connect{
					Connect: &clientpb.Connect{Version: testProtocolVersion, ClientId: "client-" + string(rune('0'+i))},
				},
			}
			err = clients[i].HandleMessage(ctx, connectMsg)
			if err != nil {
				errCh <- err
				return
			}

			// Clear transport messages from connect. Concurrent subscribe
			// fan-out may write presence events to this transport, so the
			// reset must take the transport lock (PR-04a).
			transports[i].resetMessages()

			// Subscribe to channel (use unique channel for this test)
			subMsg := &clientpb.InboundMessage{
				Id: "msg-sub-" + string(rune('0'+i)),
				Envelope: &clientpb.InboundMessage_Subscribe{
					Subscribe: &clientpb.Subscribe{
						Subscriptions: []*clientpb.Subscription{
							{Channel: "survey-channel-concurrent"},
						},
					},
				},
			}
			err = clients[i].HandleMessage(ctx, subMsg)
			if err != nil {
				errCh <- err
				return
			}

			// Clear transport messages from subscribe
			transports[i].resetMessages()
		}(i)
	}
	wg.Wait()

	// Check for any errors
	close(errCh)
	for err := range errCh {
		t.Fatalf("Client setup error = %v", err)
	}

	// Call Survey
	surveyPayload := []byte("concurrent survey test")

	// Start survey in goroutine
	var surveyResults []*SurveyResult
	var surveyErr error
	var surveyWg sync.WaitGroup
	surveyWg.Add(1)
	go func() {
		defer surveyWg.Done()
		surveyResults, surveyErr = node.Survey(ctx, "survey-channel-concurrent", surveyPayload, 5*time.Second)
	}()

	// Give survey requests time to be sent
	time.Sleep(500 * time.Millisecond)

	// Read the outbound SurveyRequest ids, then respond to each survey.
	requestIDs := make([]string, numClients)
	for i := 0; i < numClients; i++ {
		if transports[i].getMessageCount() > 0 {
			// Parse the received message
			data := transports[i].getLastMessage()
			if len(data) > 0 {
				var msg clientpb.OutboundMessage
				var m JSONMarshaler
				if err := m.Unmarshal(data, &msg); err == nil {
					if sr := msg.GetSurveyRequest(); sr != nil {
						requestIDs[i] = sr.RequestId
					}
				}
			}
		}
	}

	// Send responses
	for i := 0; i < numClients; i++ {
		responseMsg := &clientpb.InboundMessage{
			Id: "msg-survey-resp-" + string(rune('0'+i)),
			Envelope: &clientpb.InboundMessage_SurveyReply{
				SurveyReply: &clientpb.SurveyReply{
					RequestId: requestIDs[i],
					Payload: &sharedpb.Payload{
						Data: &sharedpb.Payload_Binary{Binary: []byte("response from client " + string(rune('0'+i)))},
					},
				},
			},
		}
		_ = clients[i].HandleMessage(ctx, responseMsg)
	}

	surveyWg.Wait()

	if surveyErr != nil {
		t.Fatalf("Survey() error = %v", surveyErr)
	}

	// Results should match client count
	if len(surveyResults) != numClients {
		t.Errorf("Expected %d results, got %d", numClients, len(surveyResults))
	}
}

// --- P1-8: survey responses from non-subscribers must be dropped ---

func TestNode_AddSurveyResponse_ForgedSessionDropped(t *testing.T) {
	node := NewNode(nil)
	ctx := context.Background()

	survey := NewSurvey("survey-forge-1", "forge.ch", []byte("payload"), 5*time.Second)
	survey.AddExpectedSession("real-session")
	require.True(t, node.registerSurvey(ctx, survey))
	defer node.unregisterSurvey(survey.ID())

	// A session that is not a subscriber of the survey channel tries to inject
	// a response — it must be dropped.
	node.AddSurveyResponse(ctx, "attacker-session", survey.ID(), []byte("forged"), nil)
	assert.Empty(t, survey.Results(), "forged response must be dropped")

	// The subscribed session's response is collected normally.
	node.AddSurveyResponse(ctx, "real-session", survey.ID(), []byte("real"), nil)
	results := survey.Results()
	require.Len(t, results, 1)
	assert.Equal(t, "real-session", results[0].SessionID)
	assert.Equal(t, "real", string(results[0].Payload))
}

func TestNode_Survey_ForgedResponseFromNonSubscriberDropped(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())
	ctx := context.Background()

	// Legitimate subscriber.
	transport := &capturingTransport{}
	subscriber, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	connectMsg := &clientpb.InboundMessage{
		Id:       "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{Version: testProtocolVersion, ClientId: "client-1"}},
	}
	require.NoError(t, subscriber.HandleMessage(ctx, connectMsg))
	transport.messages = nil
	subMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{Subscriptions: []*clientpb.Subscription{{Channel: "survey-forge-channel"}}},
		},
	}
	require.NoError(t, subscriber.HandleMessage(ctx, subMsg))
	transport.messages = nil

	// Attacker: connected but not subscribed to the channel.
	attackerTransport := &capturingTransport{}
	attacker, _, err := NewClient(ctx, node, attackerTransport, JSONMarshaler{})
	require.NoError(t, err)
	attackerConnect := &clientpb.InboundMessage{
		Id:       "msg-a1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{Version: testProtocolVersion, ClientId: "attacker"}},
	}
	require.NoError(t, attacker.HandleMessage(ctx, attackerConnect))

	var results []*SurveyResult
	var surveyErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		results, surveyErr = node.Survey(ctx, "survey-forge-channel", []byte("ping"), 800*time.Millisecond)
	}()

	time.Sleep(200 * time.Millisecond)

	// Read the survey request ID from the subscriber's transport.
	var req clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getLastMessage(), &req))
	surveyReq := req.GetSurveyRequest()
	require.NotNil(t, surveyReq)

	// Legitimate response from the subscriber.
	realMsg := &clientpb.InboundMessage{
		Id: "msg-r1",
		Envelope: &clientpb.InboundMessage_SurveyReply{
			SurveyReply: &clientpb.SurveyReply{
				RequestId: surveyReq.RequestId,
				Payload:   &sharedpb.Payload{Data: &sharedpb.Payload_Binary{Binary: []byte("real")}},
			},
		},
	}
	require.NoError(t, subscriber.HandleMessage(ctx, realMsg))

	// Forged response from the non-subscriber.
	forgeMsg := &clientpb.InboundMessage{
		Id: "msg-f1",
		Envelope: &clientpb.InboundMessage_SurveyReply{
			SurveyReply: &clientpb.SurveyReply{
				RequestId: surveyReq.RequestId,
				Payload:   &sharedpb.Payload{Data: &sharedpb.Payload_Binary{Binary: []byte("forged")}},
			},
		},
	}
	require.NoError(t, attacker.HandleMessage(ctx, forgeMsg))

	wg.Wait()
	require.NoError(t, surveyErr)
	require.Len(t, results, 1, "forged response must be dropped")
	assert.Equal(t, subscriber.SessionID(), results[0].SessionID)
	assert.Equal(t, "real", string(results[0].Payload))
}

func TestHub_GetSubscribers(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run(context.Background())
	ctx := context.Background()

	const numClients = 3
	clients := make([]*Client, numClients)

	// Create clients and subscribe them
	for i := 0; i < numClients; i++ {
		transport := &capturingTransport{}
		var err error
		clients[i], _, err = NewClient(ctx, node, transport, JSONMarshaler{})
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		clients[i].mu.Lock()
		clients[i].authenticated = true
		clients[i].mu.Unlock()
		require.NoError(t, clients[i].Attach(clients[i].attachment))

		_ = node.AddClient(clients[i])
		err = node.AddSubscription(ctx, "test-channel", Subscriber{Session: clients[i], Ephemeral: false})
		if err != nil {
			t.Fatalf("addSubscription() error = %v", err)
		}
	}

	// Get subscribers
	subscribers := node.Hub().GetSubscribers("test-channel")
	if len(subscribers) != numClients {
		t.Errorf("Expected %d subscribers, got %d", numClients, len(subscribers))
	}

	// Verify we got the correct clients
	sessionIDs := make(map[string]bool)
	for _, client := range clients {
		sessionIDs[client.SessionID()] = true
	}

	for _, sub := range subscribers {
		if !sessionIDs[sub.SessionID()] {
			t.Errorf("Got unexpected subscriber: %s", sub.SessionID())
		}
	}
}

func TestHub_GetSubscribers_EmptyChannel(t *testing.T) {
	node := NewNode(nil)

	subscribers := node.Hub().GetSubscribers("empty-channel")
	if subscribers != nil {
		t.Errorf("Expected nil for empty channel, got %d subscribers", len(subscribers))
	}
}

// --- P2-3: survey robustness — send timeout, wait fallback, registry cap ---

// blockingTransport blocks every write while block is true (until release is
// closed), simulating a slow consumer whose send buffer is full.
type blockingTransport struct {
	mu      sync.Mutex
	block   bool
	release chan struct{}
}

func (t *blockingTransport) Write([]byte) error {
	t.mu.Lock()
	block := t.block
	t.mu.Unlock()
	if block {
		<-t.release
	}
	return nil
}

func (t *blockingTransport) WriteMany(...[]byte) error {
	t.mu.Lock()
	block := t.block
	t.mu.Unlock()
	if block {
		<-t.release
	}
	return nil
}

func (t *blockingTransport) Close(Disconnect) error { return nil }
func (t *blockingTransport) RemoteAddr() string     { return "127.0.0.1:12345" }

func (t *blockingTransport) setBlock(block bool) {
	t.mu.Lock()
	t.block = block
	t.mu.Unlock()
}

// TestNode_Survey_BlockedWriteTimesOutInsteadOfHanging verifies P2-3 fix 1:
// a subscriber whose transport blocks writes produces an error response and
// the survey returns instead of hanging forever.
func TestNode_Survey_BlockedWriteTimesOutInsteadOfHanging(t *testing.T) {
	originalSendTimeout := surveySendTimeout
	surveySendTimeout = 300 * time.Millisecond
	t.Cleanup(func() { surveySendTimeout = originalSendTimeout })

	node := NewNode(nil)
	require.NoError(t, node.Run(context.Background()))
	ctx := context.Background()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	transport := &blockingTransport{release: release}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	connectMsg := &clientpb.InboundMessage{
		Id:       "connect-blocked",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{Version: testProtocolVersion, ClientId: "blocked-client"}},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))
	subMsg := &clientpb.InboundMessage{
		Id: "subscribe-blocked",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{Subscriptions: []*clientpb.Subscription{{Channel: "survey-blocked.ch"}}},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, subMsg))

	// Now block the transport: the survey request write must time out and
	// be recorded as a failure instead of hanging the survey.
	transport.setBlock(true)

	start := time.Now()
	results, err := node.Survey(ctx, "survey-blocked.ch", []byte("ping"), time.Second)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Less(t, elapsed, 10*time.Second, "survey must return on send timeout, not hang")
	require.Len(t, results, 1, "the blocked subscriber must yield one (error) result")
	assert.Equal(t, client.SessionID(), results[0].SessionID)
	assert.Error(t, results[0].Error, "send failure must be recorded in the response")

	// Unblock writes so the abandoned send goroutine can finish.
	transport.setBlock(false)
}

// TestSurvey_Wait_ZeroTimeoutFallsBackToDefault verifies P2-3 fix 3: a
// timeout <= 0 must not make Wait expire immediately.
func TestSurvey_Wait_ZeroTimeoutFallsBackToDefault(t *testing.T) {
	originalDefault := defaultSurveyWaitTimeout
	defaultSurveyWaitTimeout = 100 * time.Millisecond
	t.Cleanup(func() { defaultSurveyWaitTimeout = originalDefault })

	survey := NewSurvey("wait-zero", "ch", []byte("payload"), 0)
	start := time.Now()
	results := survey.Wait(context.Background())
	elapsed := time.Since(start)

	require.Empty(t, results)
	require.GreaterOrEqual(t, elapsed, 50*time.Millisecond,
		"Wait must not expire immediately when the survey timeout is <= 0")
}

// TestNode_RegisterSurveyRegistryLimit verifies P2-3 fix 4: registration
// beyond the cap is rejected.
func TestNode_RegisterSurveyRegistryLimit(t *testing.T) {
	originalLimit := maxActiveSurveys
	maxActiveSurveys = 2
	t.Cleanup(func() { maxActiveSurveys = originalLimit })

	node := NewNode(nil)
	ctx := context.Background()

	require.True(t, node.registerSurvey(ctx, NewSurvey("reg-1", "ch", nil, time.Second)))
	require.True(t, node.registerSurvey(ctx, NewSurvey("reg-2", "ch", nil, time.Second)))
	require.False(t, node.registerSurvey(ctx, NewSurvey("reg-3", "ch", nil, time.Second)),
		"registration beyond the cap must be rejected")
}

// --- PR-07: client-initiated Survey ---

// decodeOutboundMessages decodes every outbound message captured by the
// transport (JSON marshaler, same as the tests above).
func decodeOutboundMessages(t *testing.T, transport *capturingTransport) []*clientpb.OutboundMessage {
	t.Helper()
	transport.mu.Lock()
	raw := append([][]byte(nil), transport.messages...)
	transport.mu.Unlock()
	var messages []*clientpb.OutboundMessage
	for _, data := range raw {
		var msg clientpb.OutboundMessage
		if err := (JSONMarshaler{}).Unmarshal(data, &msg); err == nil {
			messages = append(messages, &msg)
		}
	}
	return messages
}

func waitForOutbound(t *testing.T, transport *capturingTransport, match func(*clientpb.OutboundMessage) bool) *clientpb.OutboundMessage {
	t.Helper()
	var found *clientpb.OutboundMessage
	require.Eventually(t, func() bool {
		for _, msg := range decodeOutboundMessages(t, transport) {
			if match(msg) {
				found = msg
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)
	return found
}

func countOutbound(t *testing.T, transport *capturingTransport, match func(*clientpb.OutboundMessage) bool) int {
	count := 0
	for _, msg := range decodeOutboundMessages(t, transport) {
		if match(msg) {
			count++
		}
	}
	return count
}

func waitForSurveyRequest(t *testing.T, transport *capturingTransport) *clientpb.SurveyRequest {
	t.Helper()
	msg := waitForOutbound(t, transport, func(msg *clientpb.OutboundMessage) bool {
		return msg.GetSurveyRequest() != nil
	})
	return msg.GetSurveyRequest()
}

func waitForSurveyResult(t *testing.T, transport *capturingTransport) *clientpb.SurveyResult {
	t.Helper()
	msg := waitForOutbound(t, transport, func(msg *clientpb.OutboundMessage) bool {
		return msg.GetSurveyResult() != nil
	})
	return msg.GetSurveyResult()
}

func waitForError(t *testing.T, transport *capturingTransport, code string) *sharedpb.Error {
	t.Helper()
	msg := waitForOutbound(t, transport, func(msg *clientpb.OutboundMessage) bool {
		return msg.GetError() != nil && msg.GetError().GetCode() == code
	})
	return msg.GetError()
}

func surveyRequestMessage(channel string, timeoutMs int32) *clientpb.InboundMessage {
	return &clientpb.InboundMessage{
		Id: "survey-" + channel,
		Envelope: &clientpb.InboundMessage_SurveyRequest{
			SurveyRequest: &clientpb.SurveyRequest{
				Channel:   channel,
				Payload:   &sharedpb.Payload{Data: &sharedpb.Payload_Binary{Binary: []byte("ping")}},
				TimeoutMs: timeoutMs,
			},
		},
	}
}

func replyWith(t *testing.T, ctx context.Context, client *Client, requestID string, payload []byte) {
	t.Helper()
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "reply-" + requestID,
		Envelope: &clientpb.InboundMessage_SurveyReply{
			SurveyReply: &clientpb.SurveyReply{
				RequestId: requestID,
				Payload:   &sharedpb.Payload{Data: &sharedpb.Payload_Binary{Binary: payload}},
			},
		},
	}))
}

func answerPayload(answer *clientpb.SurveyAnswer) []byte {
	if answer.GetPayload() == nil {
		return nil
	}
	return answer.GetPayload().GetBinary()
}

// newSurveyTestClient connects a fresh client and subscribes it to channel,
// then clears the transport so tests only see survey traffic.
func newSurveyTestClient(t *testing.T, node *Node, channel string) (*Client, *capturingTransport) {
	t.Helper()
	ctx := context.Background()
	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "connect-" + uuid.NewString(),
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{Version: testProtocolVersion, ClientId: "client-" + uuid.NewString()},
		},
	}))
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "sub-" + uuid.NewString(),
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{Subscriptions: []*clientpb.Subscription{{Channel: channel}}},
		},
	}))
	transport.resetMessages()
	return client, transport
}

// newSurveyClient wires a client with deterministic session/user ids before
// AddClient (so the hub session map matches) and subscribes it to channel.
func newSurveyClient(t *testing.T, node *Node, sessionID, userID, channel string) (*Client, *capturingTransport) {
	t.Helper()
	ctx := context.Background()
	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs(sessionID, userID, userID)
	require.NoError(t, node.AddClient(client))
	require.NoError(t, node.AddSubscription(ctx, channel, NewSubscriber(client, false)))
	transport.resetMessages()
	return client, transport
}

// TestClientSurvey_RoundTrip: policy + ACL on, A/B subscribe an exact
// channel. A surveys; B (and A) answer according to the outbound
// SurveyRequest.request_id; A receives the aggregated SurveyResult.
func TestClientSurvey_RoundTrip(t *testing.T) {
	node := NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Default: config.ChannelPolicySpec{Survey: policyBoolPtr(true)},
			Rules: []config.AuthorizerRule{
				{Pattern: "survey.**", AllowSurvey: []string{"*"}},
			},
		},
	})
	require.NoError(t, node.Run(context.Background()))
	ctx := context.Background()

	// Deterministic session/user ids are set before AddClient so the hub
	// session map matches and the answer metadata user_id entry (the proto
	// has no user_id field) is observable.
	clientA, transportA := newSurveyClient(t, node, "sess-survey-a", "user-survey-a", "survey.room.1")
	clientB, transportB := newSurveyClient(t, node, "sess-survey-b", "user-survey-b", "survey.room.1")

	requestID := "srv-1"
	require.NoError(t, clientA.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "survey-initiate",
		Envelope: &clientpb.InboundMessage_SurveyRequest{
			SurveyRequest: &clientpb.SurveyRequest{
				RequestId: requestID,
				Channel:   "survey.room.1",
				Payload:   &sharedpb.Payload{Data: &sharedpb.Payload_Binary{Binary: []byte("ping")}},
				TimeoutMs: 300,
			},
		},
	}))

	// Both subscribers receive the outbound SurveyRequest (channel filled,
	// server-generated request id); they reply to it.
	reqA := waitForSurveyRequest(t, transportA)
	reqB := waitForSurveyRequest(t, transportB)
	require.Equal(t, "survey.room.1", reqA.GetChannel())
	require.Equal(t, "survey.room.1", reqB.GetChannel())
	require.Equal(t, reqA.GetRequestId(), reqB.GetRequestId())
	require.NotEmpty(t, reqA.GetRequestId())
	replyWith(t, ctx, clientB, reqB.GetRequestId(), []byte("pong"))
	replyWith(t, ctx, clientA, reqA.GetRequestId(), []byte("self"))

	// A receives the async SurveyResult echoing its own request id.
	result := waitForSurveyResult(t, transportA)
	require.Equal(t, requestID, result.GetRequestId())
	require.Equal(t, "survey.room.1", result.GetChannel())
	answers := make(map[string]*clientpb.SurveyAnswer, len(result.GetAnswers()))
	for _, answer := range result.GetAnswers() {
		answers[answer.GetSessionId()] = answer
	}
	require.Contains(t, answers, clientB.SessionID(), "B's answer must be in the result")
	require.Equal(t, []byte("pong"), answerPayload(answers[clientB.SessionID()]))
	require.Contains(t, answers, clientA.SessionID(), "the initiator's own answer may be included")
	require.Equal(t, []byte("self"), answerPayload(answers[clientA.SessionID()]))
	require.Equal(t, "user-survey-b", answers[clientB.SessionID()].GetMetadata().GetEntries()["user_id"],
		"user_id must be carried in answer metadata (no proto field)")
}

// TestClientSurvey_DefaultDisabled: default config (survey=false) rejects
// with SURVEY_DISABLED and delivers zero SurveyRequests.
func TestClientSurvey_DefaultDisabled(t *testing.T) {
	node := NewNode(nil)
	require.NoError(t, node.Run(context.Background()))
	ctx := context.Background()

	clientA, transportA := newSurveyTestClient(t, node, "plain.ch")
	require.NoError(t, clientA.HandleMessage(ctx, surveyRequestMessage("plain.ch", 300)))

	require.NotNil(t, waitForError(t, transportA, "SURVEY_DISABLED"))
	time.Sleep(200 * time.Millisecond)
	require.Zero(t, countOutbound(t, transportA, func(msg *clientpb.OutboundMessage) bool {
		return msg.GetSurveyRequest() != nil
	}), "no SurveyRequest may be delivered when survey is disabled")
}

// TestClientSurvey_NotCovered: a session may only survey channels it
// subscribes to (exact or wildcard); otherwise PERMISSION_DENIED with zero
// delivery.
func TestClientSurvey_NotCovered(t *testing.T) {
	node := NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Default: config.ChannelPolicySpec{Survey: policyBoolPtr(true)},
			Rules: []config.AuthorizerRule{
				{Pattern: "csurvey.**", AllowSurvey: []string{"*"}},
			},
		},
	})
	require.NoError(t, node.Run(context.Background()))
	ctx := context.Background()

	clientA, transportA := newSurveyTestClient(t, node, "a.ch")
	require.NoError(t, clientA.HandleMessage(ctx, surveyRequestMessage("b.ch", 300)))

	require.NotNil(t, waitForError(t, transportA, "PERMISSION_DENIED"))
	time.Sleep(200 * time.Millisecond)
	require.Zero(t, countOutbound(t, transportA, func(msg *clientpb.OutboundMessage) bool {
		return msg.GetSurveyRequest() != nil
	}), "an uncovered channel must produce zero outbound SurveyRequests")
}

// TestClientSurvey_TooManyLocal: a single node with > MaxSurveySubscribers
// subscribers rejects synchronously with SURVEY_TOO_MANY_SUBSCRIBERS and
// zero outbound SurveyRequests.
// surveyOpenServer returns a server config with client survey enabled and
// allow_survey opened on prefix (PR-KA-A4: survey is default-deny, an
// allow_survey rule is required to open it).
func surveyOpenServer(prefix string) *config.Server {
	return &config.Server{
		Authorizer: config.AuthorizerConfig{
			Default: config.ChannelPolicySpec{Survey: policyBoolPtr(true)},
			Rules: []config.AuthorizerRule{
				{Pattern: prefix + ".**", AllowSurvey: []string{"*"}},
			},
		},
	}
}

// surveyOpenNode returns a running node built from surveyOpenServer.
func surveyOpenNode(t *testing.T, prefix string) *Node {
	t.Helper()
	node := NewNode(surveyOpenServer(prefix))
	require.NoError(t, node.Run(context.Background()))
	return node
}

func TestClientSurvey_TooManyLocal(t *testing.T) {
	node := surveyOpenNode(t, "big")
	require.NoError(t, node.Run(context.Background()))
	ctx := context.Background()

	clientA, transportA := newSurveyTestClient(t, node, "big.ch")
	transports := []*capturingTransport{transportA}
	for i := 0; i < 256; i++ {
		transport := &capturingTransport{}
		client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
		require.NoError(t, err)
		require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
			Id: "connect-" + strconv.Itoa(i),
			Envelope: &clientpb.InboundMessage_Connect{
				Connect: &clientpb.Connect{Version: testProtocolVersion, ClientId: "c-" + strconv.Itoa(i)},
			},
		}))
		require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
			Id: "sub-" + strconv.Itoa(i),
			Envelope: &clientpb.InboundMessage_Subscribe{
				Subscribe: &clientpb.Subscribe{
					Subscriptions: []*clientpb.Subscription{{Channel: "big.ch", Ephemeral: true}},
				},
			},
		}))
		transports = append(transports, transport)
	}
	require.Equal(t, 257, node.Hub().NumSubscribers("big.ch"))

	require.NoError(t, clientA.HandleMessage(ctx, surveyRequestMessage("big.ch", 300)))
	require.NotNil(t, waitForError(t, transportA, "SURVEY_TOO_MANY_SUBSCRIBERS"))
	time.Sleep(200 * time.Millisecond)
	for i, transport := range transports {
		require.Zero(t, countOutbound(t, transport, func(msg *clientpb.OutboundMessage) bool {
			return msg.GetSurveyRequest() != nil
		}), "no outbound SurveyRequest expected (subscriber %d)", i)
	}
}

// recordingSurveyCommandBus wraps the shared fake bus so broadcasts (which
// the fake does not record) are observable for the count_only preflight.
type recordingSurveyCommandBus struct {
	*fakeClusterCommandBus
	broadcasts []*ClusterCommand
}

func (r *recordingSurveyCommandBus) BroadcastCommand(ctx context.Context, cmd *ClusterCommand) ([]*ClusterCommandResult, error) {
	r.broadcasts = append(r.broadcasts, cmd)
	return r.broadcastResults, r.broadcastErr
}

// TestClientSurvey_CountOnlyCluster: with a cluster, the client survey
// preflight broadcasts a count_only ClusterCommandSurvey (exclude_self) and
// never runs localSurvey; a remote count over the cap rejects with zero
// outbound SurveyRequests.
func TestClientSurvey_CountOnlyCluster(t *testing.T) {
	bus := &recordingSurveyCommandBus{
		fakeClusterCommandBus: &fakeClusterCommandBus{broadcastResults: []*ClusterCommandResult{
			{
				NodeID:        "node-b",
				IncarnationID: "inc-b",
				Status:        ClusterCommandStatusSucceeded,
				Metadata:      map[string]string{"count": "300"},
			},
		}},
	}

	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", Backend: "memory"}, ClusterDependencies{
		CommandBus: bus,
		QueryStore: fakeQueryStore{},
	})
	require.NoError(t, err)

	node := surveyOpenNode(t, "count")
	node.SetCluster(runtime)
	ctx := context.Background()

	transport := &capturingTransport{}
	clientA, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	clientA.ForceTestIDs("sess-survey-a", "user-survey-a", "client-survey-a")
	require.NoError(t, node.AddClient(clientA))
	require.NoError(t, node.AddSubscription(ctx, "count.ch", NewSubscriber(clientA, false)))
	transport.resetMessages()

	require.NoError(t, clientA.HandleMessage(ctx, surveyRequestMessage("count.ch", 300)))
	require.NotNil(t, waitForError(t, transport, "SURVEY_TOO_MANY_SUBSCRIBERS"))

	require.Len(t, bus.broadcasts, 1, "exactly one preflight broadcast expected")
	cmd := bus.broadcasts[0]
	require.Equal(t, ClusterCommandSurvey, cmd.Type)
	require.Equal(t, "count.ch", cmd.Channel)
	require.Equal(t, "true", cmd.Metadata["count_only"])
	require.Equal(t, "true", cmd.Metadata["exclude_self"])

	node.surveyMu.RLock()
	require.Empty(t, node.surveys, "count_only must not register a survey (no localSurvey)")
	node.surveyMu.RUnlock()
	time.Sleep(200 * time.Millisecond)
	require.Zero(t, countOutbound(t, transport, func(msg *clientpb.OutboundMessage) bool {
		return msg.GetSurveyRequest() != nil
	}), "count_only path must not send any SurveyRequest")
}

// TestClientSurvey_WildcardCoverExact: a wildcard subscription covers exact
// channels for survey initiation; surveying a wildcard pattern is a
// BAD_REQUEST.
func TestClientSurvey_WildcardCoverExact(t *testing.T) {
	node := surveyOpenNode(t, "game")
	require.NoError(t, node.Run(context.Background()))
	ctx := context.Background()

	transport := &capturingTransport{}
	clientA, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, clientA.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{Version: testProtocolVersion, ClientId: "wildcard-client"},
		},
	}))
	require.NoError(t, clientA.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "sub-wildcard",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "game.**"}},
			},
		},
	}))
	transport.resetMessages()

	// game.** covers game.room.1, so surveying the exact channel is allowed.
	require.NoError(t, clientA.HandleMessage(ctx, surveyRequestMessage("game.room.1", 300)))
	req := waitForSurveyRequest(t, transport)
	require.Equal(t, "game.room.1", req.GetChannel())

	// Surveying a pattern itself is rejected.
	require.NoError(t, clientA.HandleMessage(ctx, surveyRequestMessage("game.**", 300)))
	require.NotNil(t, waitForError(t, transport, "BAD_REQUEST"))
	time.Sleep(200 * time.Millisecond)
	require.Zero(t, countOutbound(t, transport, func(msg *clientpb.OutboundMessage) bool {
		return msg.GetSurveyRequest() != nil && msg.GetSurveyRequest().GetChannel() == "game.**"
	}), "no SurveyRequest may be issued for a wildcard pattern")
}

// TestClientSurvey_NoDeadlockSelfAnswer: the initiator is also a subscriber.
// While the worker is collecting, the read loop must still process the
// initiator's own SurveyReply and a Ping; the SurveyResult arrives
// asynchronously (KD-15).
func TestClientSurvey_NoDeadlockSelfAnswer(t *testing.T) {
	node := surveyOpenNode(t, "self")
	require.NoError(t, node.Run(context.Background()))
	ctx := context.Background()

	clientA, transportA := newSurveyTestClient(t, node, "self.ch")
	require.NoError(t, clientA.HandleMessage(ctx, surveyRequestMessage("self.ch", 500)))

	// The initiator receives its own SurveyRequest.
	req := waitForSurveyRequest(t, transportA)

	// While the worker is still running (the 500ms wait has not elapsed),
	// the read loop must stay responsive: reply and ping handled promptly.
	start := time.Now()
	replyWith(t, ctx, clientA, req.GetRequestId(), []byte("self"))
	require.NoError(t, clientA.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "ping-during-survey",
		Envelope: &clientpb.InboundMessage_Ping{Ping: &clientpb.Ping{}},
	}))
	require.Less(t, time.Since(start), time.Second,
		"the read loop must not block while the survey worker runs")
	require.NotNil(t, waitForOutbound(t, transportA, func(msg *clientpb.OutboundMessage) bool {
		return msg.GetPong() != nil
	}))

	// The SurveyResult arrives asynchronously with the self answer.
	result := waitForSurveyResult(t, transportA)
	require.Len(t, result.GetAnswers(), 1)
	require.Equal(t, clientA.SessionID(), result.GetAnswers()[0].GetSessionId())
	require.Equal(t, []byte("self"), answerPayload(result.GetAnswers()[0]))
}

// TestClientSurvey_AnswerTooLarge: an 8KiB answer is truncated to a
// SURVEY_ANSWER_TOO_LARGE error with no payload.
func TestClientSurvey_AnswerTooLarge(t *testing.T) {
	node := surveyOpenNode(t, "large")
	require.NoError(t, node.Run(context.Background()))
	ctx := context.Background()

	clientA, transportA := newSurveyTestClient(t, node, "large.ch")
	clientB, transportB := newSurveyTestClient(t, node, "large.ch")

	require.NoError(t, clientA.HandleMessage(ctx, surveyRequestMessage("large.ch", 300)))
	reqB := waitForSurveyRequest(t, transportB)
	replyWith(t, ctx, clientB, reqB.GetRequestId(), make([]byte, 8*1024))

	result := waitForSurveyResult(t, transportA)
	var bAnswer *clientpb.SurveyAnswer
	for _, answer := range result.GetAnswers() {
		if answer.GetSessionId() == clientB.SessionID() {
			bAnswer = answer
		}
	}
	require.NotNil(t, bAnswer)
	require.NotNil(t, bAnswer.GetError())
	require.Equal(t, "SURVEY_ANSWER_TOO_LARGE", bAnswer.GetError().GetCode())
	require.Nil(t, bAnswer.GetPayload(), "an oversized answer must carry no payload")
}

// TestClientSurvey_EchoGone: an inbound SurveyRequest never produces a
// SurveyReply echo anymore.
func TestClientSurvey_EchoGone(t *testing.T) {
	node := NewNode(nil)
	require.NoError(t, node.Run(context.Background()))
	ctx := context.Background()

	clientA, transportA := newSurveyTestClient(t, node, "echo.ch")
	require.NoError(t, clientA.HandleMessage(ctx, surveyRequestMessage("echo.ch", 300)))

	require.NotNil(t, waitForError(t, transportA, "SURVEY_DISABLED"))
	time.Sleep(200 * time.Millisecond)
	require.Zero(t, countOutbound(t, transportA, func(msg *clientpb.OutboundMessage) bool {
		return msg.GetSurveyReply() != nil
	}), "inbound SurveyRequest must no longer echo a SurveyReply")
}

// TestAuthorizer_SurveyGatePinning pins the survey gates through Decide:
// CanSurvey defaults to deny — no rules and rules that only list
// allow_subscribe never open survey; an explicit allow_survey list opens it;
// deny_all short-circuits.
func TestAuthorizer_SurveyGatePinning(t *testing.T) {
	u := userPrincipal("user-1")
	empty, err := NewAuthorizer(config.AuthorizerConfig{})
	require.NoError(t, err)
	require.True(t, empty.Decide(u, ActionSubscribePattern, "any.ch").Allow)
	require.False(t, empty.Decide(u, ActionSurvey, "any.ch").Allow, "no rules must default to deny")

	subOnly, err := NewAuthorizer(config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{{Pattern: "chat.**", AllowSubscribe: []string{"*"}}},
	})
	require.NoError(t, err)
	require.True(t, subOnly.Decide(u, ActionSubscribePattern, "chat.room").Allow)
	require.False(t, subOnly.Decide(u, ActionSurvey, "chat.room").Allow,
		"a rule that only lists allow_subscribe must not open survey")

	open, err := NewAuthorizer(config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "chat.**", AllowSurvey: []string{"*"}, ChannelPolicySpec: config.ChannelPolicySpec{Survey: policyBoolPtr(true)}},
		},
	})
	require.NoError(t, err)
	require.True(t, open.Decide(u, ActionSurvey, "chat.room").Allow)

	gated, err := NewAuthorizer(config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "chat.**", AllowSurvey: []string{"alice"}, ChannelPolicySpec: config.ChannelPolicySpec{Survey: policyBoolPtr(true)}},
		},
	})
	require.NoError(t, err)
	require.True(t, gated.Decide(userPrincipal("alice"), ActionSurvey, "chat.room").Allow)
	require.False(t, gated.Decide(u, ActionSurvey, "chat.room").Allow)

	denied, err := NewAuthorizer(config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{
				Pattern:           "chat.**",
				DenyAll:           true,
				AllowSurvey:       []string{"*"},
				ChannelPolicySpec: config.ChannelPolicySpec{Survey: policyBoolPtr(true)},
			},
		},
	})
	require.NoError(t, err)
	require.False(t, denied.Decide(u, ActionSurvey, "chat.room").Allow,
		"deny_all must deny survey even with allow_survey")
}
