package messageloop

import (
	"context"
	"sync"
	"testing"
	"time"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

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
				Connect: &clientpb.Connect{ClientId: "client-" + string(rune('0'+i))},
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

	// Process survey requests from transports and send responses
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
						// Simulate client processing the survey request
						inboundMsg := &clientpb.InboundMessage{
							Id: sr.RequestId,
							Envelope: &clientpb.InboundMessage_SurveyRequest{
								SurveyRequest: sr,
							},
						}
						_ = clients[i].HandleMessage(ctx, inboundMsg)
					}
				}
			}
		}
	}

	// Give handleSurvey time to process
	time.Sleep(100 * time.Millisecond)

	// Clear transport messages from survey responses
	for i := 0; i < numClients; i++ {
		transports[i].messages = nil
	}

	// Send survey responses back
	for i := 0; i < numClients; i++ {
		requestID := clients[i].LastSurveyRequestID()
		respData, _ := structpb.NewStruct(map[string]interface{}{
			"message": "response from client " + string(rune('0'+i)),
		})
		responseMsg := &clientpb.InboundMessage{
			Id: "msg-survey-resp-" + string(rune('0'+i)),
			Envelope: &clientpb.InboundMessage_SurveyReply{
				SurveyReply: &clientpb.SurveyReply{
					RequestId: requestID,
					Payload: &sharedpb.Payload{
						Data: &sharedpb.Payload_Json{
							Json: respData,
						},
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
				Connect: &clientpb.Connect{ClientId: "client-" + string(rune('0'+i))},
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
						// Now simulate the client receiving and processing the survey request
						// This calls handleSurvey which stores the request ID
						inboundMsg := &clientpb.InboundMessage{
							Id: sr.RequestId,
							Envelope: &clientpb.InboundMessage_SurveyRequest{
								SurveyRequest: sr,
							},
						}
						_ = clients[i].HandleMessage(ctx, inboundMsg)
					}
				}
			}
		}
	}

	// Give handleSurvey time to process and send responses
	time.Sleep(100 * time.Millisecond)

	// Clear transport messages from survey responses (they were sent by clients)
	for i := 0; i < numClients; i++ {
		transports[i].messages = nil
	}

	// Now send the survey responses back to the server
	for i := 0; i < numClients; i++ {
		requestID := clients[i].LastSurveyRequestID()
		t.Logf("Client %d: sending response with request ID: %s", i, requestID)

		respData, _ := structpb.NewStruct(map[string]interface{}{
			"message": "response from client " + string(rune('0'+i)),
		})
		responseMsg := &clientpb.InboundMessage{
			Id: "msg-survey-resp-" + string(rune('0'+i)),
			Envelope: &clientpb.InboundMessage_SurveyReply{
				SurveyReply: &clientpb.SurveyReply{
					RequestId: requestID,
					Payload: &sharedpb.Payload{
						Data: &sharedpb.Payload_Json{
							Json: respData,
						},
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
					Connect: &clientpb.Connect{ClientId: "client-" + string(rune('0'+i))},
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

	// Process survey requests and send responses
	for i := 0; i < numClients; i++ {
		if transports[i].getMessageCount() > 0 {
			// Parse the received message
			data := transports[i].getLastMessage()
			if len(data) > 0 {
				var msg clientpb.OutboundMessage
				var m JSONMarshaler
				if err := m.Unmarshal(data, &msg); err == nil {
					if sr := msg.GetSurveyRequest(); sr != nil {
						// Simulate client processing
						inboundMsg := &clientpb.InboundMessage{
							Id: sr.RequestId,
							Envelope: &clientpb.InboundMessage_SurveyRequest{
								SurveyRequest: sr,
							},
						}
						_ = clients[i].HandleMessage(ctx, inboundMsg)
					}
				}
			}
		}
	}

	// Give handleSurvey time to process
	time.Sleep(100 * time.Millisecond)

	// Clear transport messages
	for i := 0; i < numClients; i++ {
		transports[i].messages = nil
	}

	// Send responses
	for i := 0; i < numClients; i++ {
		requestID := clients[i].LastSurveyRequestID()
		respData, _ := structpb.NewStruct(map[string]interface{}{
			"message": "response from client " + string(rune('0'+i)),
		})
		responseMsg := &clientpb.InboundMessage{
			Id: "msg-survey-resp-" + string(rune('0'+i)),
			Envelope: &clientpb.InboundMessage_SurveyReply{
				SurveyReply: &clientpb.SurveyReply{
					RequestId: requestID,
					Payload: &sharedpb.Payload{
						Data: &sharedpb.Payload_Json{
							Json: respData,
						},
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
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
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
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "attacker"}},
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

		_ = node.AddClient(clients[i])
		err = node.AddSubscription(ctx, "test-channel", Subscriber{Client: clients[i], Ephemeral: false})
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
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "blocked-client"}},
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
