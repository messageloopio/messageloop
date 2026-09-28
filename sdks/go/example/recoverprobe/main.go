// Command recoverprobe is the cutover acceptance probe for the message
// recovery contract across a stack migration (docker/fleetly/cutover-runbook.md
// §4, DT-8): it records a broker position on the pre-cutover stack and, after
// the cutover, asserts that a client reconnect carrying that (epoch, offset)
// cursor is trusted and the live path still works end to end.
//
// Two phases, two invocations:
//
//	ML_PROBE_URL=wss://ws.example.com/ws \
//	ML_PROBE_TOKEN=<torchwood-access-token> \
//	go run ./example/recoverprobe record
//
// prints ML_PROBE_OFFSET=<n> (the broker-assigned offset). The broker epoch
// comes from the deployment side:
//
//	docker exec <redis> redis-cli GET ml2:broker:epoch
//
// After the cutover, run the verify phase against the new stack with both
// values:
//
//	ML_PROBE_URL=wss://ws.example.com/ws \
//	ML_PROBE_TOKEN=<torchwood-access-token> \
//	ML_PROBE_EPOCH=<epoch recorded before the cutover> \
//	ML_PROBE_OFFSET=<offset printed by record> \
//	go run ./example/recoverprobe verify
//
// Exit codes: 0 = the recorded cursor was honored (the server did not replay
// history at or below the recorded offset) and a fresh publish was delivered
// live; non-zero = the probe's invariant is broken (details on stderr).
//
// What it proves and what it does not: a fresh (non-resume) subscription
// treats the cursor offset as a continuation floor — it never validates the
// carried epoch server-side — so this probe proves "the offset resume point
// is honored and the live path works" (携偏移重连被信任、无重复回放、新边缘
// 端到端可达). It cannot tell a restored Redis from an empty-but-fresh one:
// a reset instance simply has no history to replay. The decisive
// "回灌成功" check stays the Redis-side comparison in the runbook §4.1
// (ml2:broker:epoch equality + sample XLEN before/after).
//
// Environment:
//
//	ML_PROBE_URL       server URL: ws://wss:// URL for WebSocket (default
//	                   transport), host:port for gRPC
//	ML_PROBE_TOKEN     optional Torchwood access token (require_auth deployments)
//	ML_PROBE_CHANNEL   probe channel, namespaced: "<namespace>:cutover.probe"
//	                   (namespace = Torchwood project id; default "cutover.probe"
//	                   works only for a static-namespace deployment)
//	ML_PROBE_TRANSPORT "ws" (default) or "grpc"
//	ML_PROBE_TLS       "true" dials gRPC with system-root TLS (platform edge
//	                   terminates TLS and forwards h2c; requires ML_PROBE_TRANSPORT=grpc)
//	ML_PROBE_EPOCH     verify: broker epoch captured before the cutover
//	ML_PROBE_OFFSET    verify: offset printed by the record phase
//	ML_PROBE_SETTLE    verify: settle window before the live marker publish
//	                   (default 5s; raise it on a slow link with a long replay)
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	messageloopgo "github.com/messageloopio/messageloop/sdks/go"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("recoverprobe: ")
	if len(os.Args) != 2 || (os.Args[1] != "record" && os.Args[1] != "verify") {
		log.Fatalf("usage: recoverprobe <record|verify> (see the file header for ML_PROBE_* variables)")
	}
	var err error
	switch os.Args[1] {
	case "record":
		err = runRecord()
	case "verify":
		err = runVerify()
	}
	if err != nil {
		log.Fatalf("FAIL: %v", err)
	}
}

// dial builds the probe client for the configured transport.
func dial() (messageloopgo.Client, error) {
	url := os.Getenv("ML_PROBE_URL")
	if url == "" {
		return nil, fmt.Errorf("ML_PROBE_URL is required (ws:// or wss:// WebSocket URL, or host:port for gRPC)")
	}
	opts := []messageloopgo.Option{messageloopgo.WithClientID("recover-probe")}
	if token := os.Getenv("ML_PROBE_TOKEN"); token != "" {
		opts = append(opts, messageloopgo.WithToken(token))
	}
	if strings.EqualFold(envOr("ML_PROBE_TRANSPORT", "ws"), "grpc") {
		if strings.EqualFold(os.Getenv("ML_PROBE_TLS"), "true") {
			opts = append(opts, messageloopgo.WithTLS())
		}
		return messageloopgo.DialGRPC(url, opts...)
	}
	return messageloopgo.Dial(url, opts...)
}

// runRecord publishes one message and prints the broker-assigned offset, the
// cursor component a post-cutover client would carry.
func runRecord() error {
	channel := envOr("ML_PROBE_CHANNEL", "cutover.probe")
	client, err := dial()
	if err != nil {
		return err
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	offset, err := client.PublishWithAck(ctx, channel,
		messageloopgo.NewMessageWithData("cutover.probe.record", messageloopgo.NewTextData("record")))
	if err != nil {
		return fmt.Errorf("publish to %s: %w", channel, err)
	}
	fmt.Printf("ML_PROBE_OFFSET=%d\n", offset)
	fmt.Printf("record ok: channel=%s offset=%d\n", channel, offset)
	fmt.Printf("next: read the broker epoch before the cutover: docker exec <redis> redis-cli GET ml2:broker:epoch\n")
	return nil
}

// delivery is one observed message with its wire offset (when carried).
type delivery struct {
	offset uint64
	set    bool
	text   string
}

// runVerify subscribes with the recorded cursor and asserts the two contract
// clauses: nothing at or below the cursor is replayed (the offset is trusted),
// and a fresh publish is delivered live through the new edge.
func runVerify() error {
	channel := envOr("ML_PROBE_CHANNEL", "cutover.probe")
	epoch := os.Getenv("ML_PROBE_EPOCH")
	if epoch == "" {
		return fmt.Errorf("ML_PROBE_EPOCH is required (docker exec <redis> redis-cli GET ml2:broker:epoch, captured before the cutover)")
	}
	offset, err := strconv.ParseUint(os.Getenv("ML_PROBE_OFFSET"), 10, 64)
	if err != nil {
		return fmt.Errorf("ML_PROBE_OFFSET %q is not a valid uint64 offset (use the value printed by the record phase)", os.Getenv("ML_PROBE_OFFSET"))
	}

	client, err := dial()
	if err != nil {
		return err
	}
	defer client.Close()

	deliveries := make(chan delivery, 256)
	client.OnMessage(func(messages []*messageloopgo.Message) {
		for _, m := range messages {
			d := delivery{text: m.Data.AsText()}
			if raw, ok := m.Metadata["offset"]; ok {
				if n, err := strconv.ParseUint(raw, 10, 64); err == nil {
					d.offset, d.set = n, true
				}
			}
			select {
			case deliveries <- d:
			default: // probe is single-threaded; drops only under a pathological flood
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if err := client.SubscribeWith(channel, messageloopgo.WithRecover(messageloopgo.Position(epoch, offset))); err != nil {
		return fmt.Errorf("subscribe with cursor (epoch=%s offset=%d): %w", epoch, offset, err)
	}
	fmt.Printf("verify: subscribed %s with cursor (epoch=%s offset=%d); settle window %s\n",
		channel, epoch, offset, envOr("ML_PROBE_SETTLE", "5s"))

	// Settle window: a cursor honored as a continuation floor delivers nothing
	// at or below the recorded offset. A replay at or below that offset means
	// the server fell back to "from the start" (cursor lost / fresh semantics),
	// so the recorded resume point was not honored.
	settle, err := time.ParseDuration(envOr("ML_PROBE_SETTLE", "5s"))
	if err != nil {
		return fmt.Errorf("ML_PROBE_SETTLE: %w", err)
	}
	stale := 0
	settleDeadline := time.After(settle)
drain:
	for {
		select {
		case d := <-deliveries:
			if d.set && d.offset <= offset {
				stale++
			}
		case <-settleDeadline:
			break drain
		}
	}
	if stale > 0 {
		return fmt.Errorf("cursor NOT honored: %d replayed message(s) at or below offset %d arrived in the settle window (the server recovered from the start instead of the recorded resume point)", stale, offset)
	}

	// Live round trip through the new edge.
	marker := fmt.Sprintf("recover-probe-%d", time.Now().UnixNano())
	if _, err := client.PublishWithAck(ctx, channel,
		messageloopgo.NewMessageWithData("cutover.probe.verify", messageloopgo.NewTextData(marker))); err != nil {
		return fmt.Errorf("publish marker: %w", err)
	}
	markerDeadline := time.After(15 * time.Second)
	for {
		select {
		case d := <-deliveries:
			if d.text == marker {
				fmt.Printf("verify ok: cursor honored (no replay at or below offset %d); marker delivered live\n", offset)
				return nil
			}
		case <-markerDeadline:
			return fmt.Errorf("live marker was not delivered within 15s (publish was accepted but nothing came back)")
		}
	}
}

// envOr returns the environment value or a fallback.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
