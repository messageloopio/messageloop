// Command goclient is an interactive ChatRoom client written with the
// MessageLoop Go SDK. It connects over client gRPC by default and can switch
// to WebSocket or QUIC transports.
//
// Commands:
//
//	<text>             publish a chat message to the current room
//	/join <room>       subscribe to a room
//	/leave <room>      unsubscribe from a room
//	/roll              dice RPC via the backend
//	/stats             room stats RPC via the backend (admin API)
//	/history [n]       last n history entries RPC
//	/kick <name>       force-disconnect a user RPC (admin API)
//	/whoami            echo RPC metadata
//	/presence          query the presence snapshot of the current room
//	/poll <question>   start a survey in the current room
//	/whisper <text>    transient publish (not persisted)
//	/sys <text>        publish with ack, prints the broker offset
//	/refresh           re-validate subscriptions (SubRefresh)
//	/help              show this help
//	/quit              disconnect and exit
//
// Run it with: go run ./_examples/chatroom/cmd/goclient -name alice
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/messageloopio/messageloop/_examples/chatroom/internal/chatroom"
	messageloopgo "github.com/messageloopio/messageloop/sdks/go"
)

const (
	wsURL   = "ws://127.0.0.1:9080/ws"
	grpcURL = "127.0.0.1:9090"
	quicURL = "127.0.0.1:9443"
)

func main() {
	var (
		transport = flag.String("transport", "grpc", "transport: ws | grpc | quic")
		addr      = flag.String("addr", "", "server address (defaults per transport)")
		name      = flag.String("name", "alice", "user name (alice|bob|carol|dave|eve)")
		room      = flag.String("room", chatroom.Lobby, "initial room")
	)
	flag.Parse()

	if *addr == "" {
		switch *transport {
		case "ws":
			*addr = wsURL
		case "quic":
			*addr = quicURL
		default:
			*addr = grpcURL
		}
	}

	opts := []messageloopgo.Option{
		messageloopgo.WithClientID(fmt.Sprintf("%s-%d", *name, os.Getpid())),
		messageloopgo.WithClientType("go-demo"),
		messageloopgo.WithToken(chatroom.TokenForName(*name)),
		messageloopgo.WithVersion("chatroom/1.0.0"),
		messageloopgo.WithAutoReconnect(true),
		messageloopgo.WithReconnectBackoff(500*time.Millisecond, 10*time.Second, 2.0),
		messageloopgo.WithReconnectMaxAttempts(10),
		messageloopgo.WithRPCTimeout(10*time.Second),
	}
	if *transport == "quic" {
		opts = append(opts, messageloopgo.WithInsecureSkipVerify())
	}

	var (
		client messageloopgo.Client
		err    error
	)
	switch *transport {
	case "ws":
		client, err = messageloopgo.Dial(*addr, opts...)
	case "quic":
		client, err = messageloopgo.DialQUIC(*addr, opts...)
	default:
		client, err = messageloopgo.DialGRPC(*addr, opts...)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial failed: %v\n", err)
		os.Exit(1)
	}

	registerHandlers(client, *name)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := client.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
		os.Exit(1)
	}
	cancel()

	if err := client.Subscribe(*room); err != nil {
		fmt.Fprintf(os.Stderr, "subscribe failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("joined %s as %s\n", *room, *name)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		<-sig
		done <- struct{}{}
	}()

	go func() {
		<-done
		_ = client.Close()
		os.Exit(0)
	}()

	runShell(client, *name, *room)
}

// registerHandlers wires all SDK callbacks used by the demo.
func registerHandlers(client messageloopgo.Client, name string) {
	client.OnConnected(func(sessionID string) {
		fmt.Printf("[connected] session=%s\n", sessionID)
	})
	client.OnReconnecting(func(attempt int) {
		fmt.Printf("[reconnecting] attempt %d...\n", attempt)
	})
	client.OnReconnected(func(sessionID string) {
		fmt.Printf("[reconnected] session=%s (subscriptions resumed)\n", sessionID)
	})
	client.OnError(func(err error) {
		fmt.Printf("[error] %v\n", err)
	})
	client.OnMessage(func(msgs []*messageloopgo.Message) {
		for _, m := range msgs {
			channel := m.GetMetadata("channel")
			offset := m.GetMetadata("offset")
			var payload chatroom.ChatMessage
			if err := m.DataAs(&payload); err == nil && payload.Text != "" {
				fmt.Printf("\r[%s#%s] %s: %s\n", channel, offset, payload.User, payload.Text)
			} else {
				fmt.Printf("\r[%s#%s] %s: %s\n", channel, offset, name, m.String())
			}
		}
		fmt.Print("> ")
	})
	client.OnPresence(func(ev messageloopgo.PresenceEvent) {
		verb := "joined"
		if ev.Action == "leave" {
			verb = "left"
		}
		fmt.Printf("\r[presence] %s %s %s (user=%s)\n", ev.Channel, ev.Info.ClientID, verb, ev.Info.UserID)
		fmt.Print("> ")
	})
	client.OnPresenceSnapshot(func(snap messageloopgo.PresenceSnapshot) {
		if snap.Channel == "" {
			return // the connect-time snapshot is also delivered here; keep it simple
		}
		names := make([]string, 0, len(snap.Clients))
		for _, info := range snap.Clients {
			if info.UserID != "" {
				names = append(names, info.UserID)
			} else {
				names = append(names, info.ClientID)
			}
		}
		fmt.Printf("\r[presence] %s online (%d): %s\n", snap.Channel, len(snap.Clients), strings.Join(names, ", "))
		fmt.Print("> ")
	})
	// Survey answers from other clients are handled per-request.
	client.OnSurveyRequest(func(requestID, channel string, req *messageloopgo.Message) (*messageloopgo.Message, error) {
		var q chatroom.ChatMessage
		_ = req.DataAs(&q)
		fmt.Printf("\r[survey] %s asks: %s (auto-replying)\n", channel, q.Text)
		fmt.Print("> ")
		answer := messageloopgo.NewMessageWithData("chat.poll.answer",
			messageloopgo.NewTextData("auto answer from "+name))
		return answer, nil
	})
}

// runShell reads commands from stdin until /quit.
func runShell(client messageloopgo.Client, name, room string) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("> ")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "/quit" {
			_ = client.Close()
			return
		}
		handleCommand(client, name, &room, line)
		fmt.Print("> ")
	}
}

// handleCommand dispatches one user input line.
func handleCommand(client messageloopgo.Client, name string, room *string, line string) {
	fields := strings.Fields(line)
	cmd := fields[0]
	rest := strings.TrimSpace(strings.TrimPrefix(line, cmd))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sendText := func(channel, kind, text string, transient bool) {
		msg := messageloopgo.NewMessageWithData("chat.message", messageloopgo.NewJSONData(map[string]any{
			"user": name, "text": text, "kind": kind,
		}))
		if transient {
			if err := client.Publish(channel, msg, true); err != nil {
				fmt.Printf("[error] %v\n", err)
			}
			return
		}
		if err := client.Publish(channel, msg); err != nil {
			fmt.Printf("[error] %v\n", err)
		}
	}

	switch cmd {
	case "/join":
		if rest == "" {
			fmt.Println("usage: /join <room>")
			return
		}
		roomName := rest
		if err := client.Subscribe(roomName); err != nil {
			fmt.Printf("[error] %v\n", err)
			return
		}
		*room = roomName
		fmt.Printf("joined %s\n", roomName)

	case "/leave":
		if rest == "" {
			fmt.Println("usage: /leave <room>")
			return
		}
		if err := client.Unsubscribe(rest); err != nil {
			fmt.Printf("[error] %v\n", err)
			return
		}
		fmt.Printf("left %s\n", rest)

	case "/roll", "/stats", "/history", "/kick", "/whoami":
		req := messageloopgo.NewMessageWithData("chat.rpc", messageloopgo.NewTextData(rest))
		var resp messageloopgo.Message
		if err := client.RPC(ctx, *room, cmd, req, &resp); err != nil {
			fmt.Printf("[rpc error] %v\n", err)
			return
		}
		if resp.Data.AsText() != "" {
			fmt.Println(resp.Data.AsText())
		} else {
			fmt.Println(resp.String())
		}

	case "/presence":
		snap, err := client.Presence(ctx, *room)
		if err != nil {
			fmt.Printf("[error] %v\n", err)
			return
		}
		if len(snap.Clients) == 0 {
			fmt.Println("nobody online")
			return
		}
		fmt.Printf("online in %s (%d present):\n", snap.Channel, len(snap.Clients))
		for _, info := range snap.Clients {
			fmt.Printf("  %-12s user=%s session=%s\n", info.ClientID, info.UserID, info.SessionID)
		}

	case "/poll":
		if rest == "" {
			fmt.Println("usage: /poll <question>")
			return
		}
		msg := messageloopgo.NewMessageWithData("chat.poll",
			messageloopgo.NewJSONData(map[string]any{"user": name, "kind": "poll", "text": rest}))
		answers, err := client.Survey(ctx, *room, msg, 5*time.Second)
		if err != nil {
			fmt.Printf("[survey error] %v\n", err)
			return
		}
		fmt.Printf("poll results (%d answer(s)):\n", len(answers))
		for _, a := range answers {
			answerName := a.UserID
			if answerName == "" {
				answerName = a.SessionID
			}
			fmt.Printf("  %s: %s\n", answerName, a.Payload.String())
		}

	case "/whisper":
		if rest == "" {
			fmt.Println("usage: /whisper <text>")
			return
		}
		sendText(*room, "whisper", rest, true)
		fmt.Println("(transient, not persisted)")

	case "/sys":
		if rest == "" {
			fmt.Println("usage: /sys <text>")
			return
		}
		msg := messageloopgo.NewMessageWithData("chat.message",
			messageloopgo.NewJSONData(map[string]any{"user": name, "kind": "system", "text": rest}))
		offset, err := client.PublishWithAck(ctx, *room, msg)
		if err != nil {
			fmt.Printf("[error] %v\n", err)
			return
		}
		fmt.Printf("published at offset %d\n", offset)

	case "/refresh":
		if err := client.SubRefresh(ctx, *room); err != nil {
			fmt.Printf("[error] %v\n", err)
			return
		}
		fmt.Println("subscriptions re-validated")

	case "/help":
		printHelp()

	default:
		if strings.HasPrefix(cmd, "/") {
			fmt.Println("unknown command, try /help")
			return
		}
		sendText(*room, "chat", line, false)
	}
}

func printHelp() {
	fmt.Println(`commands:
  <text>             publish a chat message to the current room
  /join <room>       subscribe to a room
  /leave <room>      unsubscribe from a room
  /roll              dice RPC via the backend
  /stats             room stats RPC via the backend (admin API)
  /history [n]       last n history entries RPC
  /kick <name>       force-disconnect a user RPC (admin API)
  /whoami            echo RPC metadata
  /presence          query the presence snapshot of the current room
  /poll <question>   start a survey in the current room
  /whisper <text>    transient publish (not persisted)
  /sys <text>        publish with ack, prints the broker offset
  /refresh           re-validate subscriptions (SubRefresh)
  /help              show this help
  /quit              disconnect and exit`)
}
