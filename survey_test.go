package messageloop

import (
	"context"
	"sync"
	"testing"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/v1"
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
	_ = node.Run()
	ctx := context.Background()

	const numClients = 3
	transports := make([]*capturingTransport, numClients)
	clients := make([]*ClientSession, numClients)

	// Create clients and subscribe them to the channel
	for i := 0; i < numClients; i++ {
		transports[i] = &capturingTransport{}
		var err error
		clients[i], _, err = NewClientSession(ctx, node, transports[i], JSONMarshaler{})
		if err != nil {
			t.Fatalf("NewClientSession() error = %v", err)
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
			Id:      "msg-sub-" + string(rune('0'+i)),
			Channel: "survey-channel-basic",
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
							Id:      sr.RequestId,
							Channel: "survey-channel-basic",
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
		responseMsg := &clientpb.InboundMessage{
			Id:      "msg-survey-resp-" + string(rune('0'+i)),
			Channel: "survey-channel-basic",
			Envelope: &clientpb.InboundMessage_SurveyResponse{
				SurveyResponse: &clientpb.SurveyResponse{
					RequestId: requestID,
					Payload: &cloudevents.CloudEvent{
						Id:          "response-" + string(rune('0'+i)),
						Source:      "survey-channel-basic",
						SpecVersion: "1.0",
						Type:        "com.messageloop.survey.response",
						Data: &cloudevents.CloudEvent_BinaryData{
							BinaryData: []byte("response from client " + string(rune('0'+i))),
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
	_ = node.Run()
	ctx := context.Background()

	const numClients = 3
	transports := make([]*capturingTransport, numClients)
	clients := make([]*ClientSession, numClients)

	// Create clients and subscribe them to the channel
	for i := 0; i < numClients; i++ {
		transports[i] = &capturingTransport{}
		var err error
		clients[i], _, err = NewClientSession(ctx, node, transports[i], JSONMarshaler{})
		if err != nil {
			t.Fatalf("NewClientSession() error = %v", err)
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
			Id:      "msg-sub-" + string(rune('0'+i)),
			Channel: "survey-channel-respond",
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
							Id:      sr.RequestId,
							Channel: "survey-channel-respond",
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

		responseMsg := &clientpb.InboundMessage{
			Id:      "msg-survey-resp-" + string(rune('0'+i)),
			Channel: "survey-channel-respond",
			Envelope: &clientpb.InboundMessage_SurveyResponse{
				SurveyResponse: &clientpb.SurveyResponse{
					RequestId: requestID,
					Payload: &cloudevents.CloudEvent{
						Id:          "response-" + string(rune('0'+i)),
						Source:      "survey-channel-respond",
						SpecVersion: "1.0",
						Type:        "com.messageloop.survey.response",
						Data: &cloudevents.CloudEvent_BinaryData{
							BinaryData: []byte("response from client " + string(rune('0'+i))),
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
	_ = node.Run()
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
	_ = node.Run()
	ctx := context.Background()

	const numClients = 10
	var wg sync.WaitGroup
	transports := make([]*capturingTransport, numClients)
	clients := make([]*ClientSession, numClients)
	errCh := make(chan error, numClients)

	// Create clients concurrently
	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			transports[i] = &capturingTransport{}
			var err error
			clients[i], _, err = NewClientSession(ctx, node, transports[i], JSONMarshaler{})
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

			// Clear transport messages from connect
			transports[i].messages = nil

			// Subscribe to channel (use unique channel for this test)
			subMsg := &clientpb.InboundMessage{
				Id:      "msg-sub-" + string(rune('0'+i)),
				Channel: "survey-channel-concurrent",
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
			transports[i].messages = nil
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
							Id:      sr.RequestId,
							Channel: "survey-channel-concurrent",
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
		responseMsg := &clientpb.InboundMessage{
			Id:      "msg-survey-resp-" + string(rune('0'+i)),
			Channel: "survey-channel-concurrent",
			Envelope: &clientpb.InboundMessage_SurveyResponse{
				SurveyResponse: &clientpb.SurveyResponse{
					RequestId: requestID,
					Payload: &cloudevents.CloudEvent{
						Id:          "response-" + string(rune('0'+i)),
						Source:      "survey-channel-concurrent",
						SpecVersion: "1.0",
						Type:        "com.messageloop.survey.response",
						Data: &cloudevents.CloudEvent_BinaryData{
							BinaryData: []byte("response from client " + string(rune('0'+i))),
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

func TestHub_GetSubscribers(t *testing.T) {
	node := NewNode(nil)
	_ = node.Run()
	ctx := context.Background()

	const numClients = 3
	clients := make([]*ClientSession, numClients)

	// Create clients and subscribe them
	for i := 0; i < numClients; i++ {
		transport := &capturingTransport{}
		var err error
		clients[i], _, err = NewClientSession(ctx, node, transport, JSONMarshaler{})
		if err != nil {
			t.Fatalf("NewClientSession() error = %v", err)
		}

		clients[i].mu.Lock()
		clients[i].authenticated = true
		clients[i].mu.Unlock()

		node.addClient(clients[i])
		err = node.addSubscription(ctx, "test-channel", subscriber{client: clients[i], ephemeral: false})
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
