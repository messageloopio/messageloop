package runtime

// Broadcast batching (docs/design/2026-10-20-broadcast-batching.md): the
// publicationBatcher sits between the broker handler (serial per channel)
// and the hub fan-out and merges the publications of high-occupancy exact
// channels into one multi-message frame per flush window, dividing the
// per-subscriber enqueue and write-confirm cost by the batch size.
//
// Memory safety contract: every buffer here is bounded (max_messages /
// max_batch_bytes), and "full" means "block the producer" — never drop,
// never buffer without limit. Blocking the delivery worker propagates
// backpressure along the broker's existing chain (worker queue → consumer
// loop → Redis) instead of inventing a new buffering policy; the Redis
// client-output-buffer disconnect + history catch-up remain the ultimate
// relief valve. Transient (offset 0) publications bypass batching entirely.

import (
	"sync"
	"time"

	"github.com/messageloopio/messageloop/internal/config"
)

// Batch flush triggers, used as the histogram label. "explicit" is the
// oversize bypass draining the buffer ahead of an unbatchable publication;
// "shutdown" is the final flush on Close.
const (
	batchTriggerDelay     = "delay"
	batchTriggerMessages  = "messages"
	batchTriggerExplicit  = "explicit"
	batchTriggerShutdown  = "shutdown"
	batchCloseWaitTimeout = 2 * time.Second
)

// publicationBatcher dispatches broker handler calls either straight to the
// hub fan-out or into the per-channel accumulator, depending on the
// publication kind, the channel policy, and the local subscriber count.
type publicationBatcher struct {
	settings config.BatchingSettings
	node     *Node

	// Indirection seams for tests; the production constructor wires them to
	// the node's hub. immediate is the unbatched path (today's fan-out),
	// broadcast the batched one.
	immediate func(ch string, pub *Publication) error
	broadcast func(ch string, pubs []*Publication) error
	subCount  func(ch string) int
	policyFor func(ch string) ChannelPolicy

	mu   sync.Mutex
	accs map[string]*channelAccumulator
	done chan struct{}
	wg   sync.WaitGroup
	// closeOnce keeps Close idempotent: a double Shutdown (or a test
	// cleanup after an explicit Close) must not panic on the done channel.
	closeOnce sync.Once
}

// newPublicationBatcher builds the batcher for a node. The node must have
// its hub set (NewNode does before any Deliver can run).
func newPublicationBatcher(node *Node, settings config.BatchingSettings) *publicationBatcher {
	b := &publicationBatcher{
		settings: settings,
		node:     node,
		accs:     make(map[string]*channelAccumulator),
		done:     make(chan struct{}),
	}
	b.immediate = node.hub.BroadcastPublication
	b.broadcast = node.hub.BroadcastPublications
	b.subCount = node.hub.NumSubscribers
	b.policyFor = node.ChannelPolicy
	return b
}

// Deliver is the broker publication handler entry point. The broker invokes
// it serially per channel, so per-channel state needs no synchronization
// beyond the accumulator's own.
func (b *publicationBatcher) Deliver(ch string, pub *Publication) error {
	// Transient publications (presence join/leave, offset 0) keep their
	// low-latency path; they are also not resumable, so batching buys them
	// nothing.
	if pub.Offset == 0 {
		return b.immediate(ch, pub)
	}
	if !b.enabledFor(ch) {
		return b.immediate(ch, pub)
	}
	// Small channels pay no batching latency: below the threshold the
	// publication goes out immediately and no accumulator exists.
	if b.subCount(ch) < b.settings.MinSubscribers {
		return b.immediate(ch, pub)
	}
	// A single publication larger than the budget cannot enter the buffer
	// (it would block forever against the byte cap). Drain any buffered
	// messages first — sending it now would jump ahead of them — then send
	// it unbatched.
	if len(pub.Payload) > b.settings.MaxBatchBytes {
		if acc := b.lookup(ch); acc != nil {
			acc.flushAndWait()
		}
		return b.immediate(ch, pub)
	}
	return b.accumulatorFor(ch).append(pub)
}

// enabledFor resolves the channel policy override; the batcher only exists
// when server.broadcast is configured, so no override means enabled.
func (b *publicationBatcher) enabledFor(ch string) bool {
	if pol := b.policyFor(ch); pol.Batching != nil {
		return *pol.Batching
	}
	return true
}

// lookup returns the channel's accumulator without creating one.
func (b *publicationBatcher) lookup(ch string) *channelAccumulator {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.accs[ch]
}

// accumulatorFor returns the channel's accumulator, creating its flusher
// goroutine on first use. Only channels above the subscriber threshold get
// this far, so the map stays small; idle accumulators reap themselves.
func (b *publicationBatcher) accumulatorFor(ch string) *channelAccumulator {
	b.mu.Lock()
	acc, ok := b.accs[ch]
	if !ok {
		acc = newChannelAccumulator(b, ch)
		b.accs[ch] = acc
		b.wg.Add(1)
		go acc.run()
	}
	b.mu.Unlock()
	return acc
}

// removeAccumulator drops a reaped accumulator; the identity check keeps a
// concurrently recreated entry intact.
func (b *publicationBatcher) removeAccumulator(ch string, acc *channelAccumulator) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.accs[ch] == acc {
		delete(b.accs, ch)
	}
}

// Close flushes every accumulator one last time and stops the flushers. It
// bounds the wait: a final flush still fighting the 10s broadcast write
// budget is abandoned, and whatever it carried is recovered by clients
// through history catch-up after the restart — the same semantics as
// today's process exit. The post-wait sweep drains a race window: an append
// that slipped past its flusher's shutdown branch would otherwise sit in a
// buffer nobody reads.
func (b *publicationBatcher) Close() {
	b.closeOnce.Do(func() { close(b.done) })
	finished := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(batchCloseWaitTimeout):
	}
	b.mu.Lock()
	accs := make([]*channelAccumulator, 0, len(b.accs))
	for _, acc := range b.accs {
		accs = append(accs, acc)
	}
	b.mu.Unlock()
	for _, acc := range accs {
		acc.flush(batchTriggerShutdown)
	}
}

// --- metrics helpers (all nil-safe: tests build nodes without metrics) ---

func (b *publicationBatcher) observeBatch(trigger string, messages, bytes int, took time.Duration) {
	if m := b.node.metrics; m != nil {
		m.BroadcastBatchMessages.WithLabelValues(trigger).Observe(float64(messages))
		m.BroadcastBatchFlushSeconds.Observe(took.Seconds())
		m.BroadcastBatchPendingBytes.Sub(float64(bytes))
	}
}

func (b *publicationBatcher) addPending(bytes int) {
	if m := b.node.metrics; m != nil {
		m.BroadcastBatchPendingBytes.Add(float64(bytes))
	}
}

func (b *publicationBatcher) countBackpressure() {
	if m := b.node.metrics; m != nil {
		m.BroadcastBatchBackpressureTotal.Inc()
	}
}

// channelAccumulator buffers publications for one channel until a flush
// trigger fires. Appends happen on the broker's delivery worker (serial per
// channel); the flusher goroutine owns flushing. Double-buffered: the
// flusher swaps out the whole buffer, so appends continue into a fresh one
// while the in-flight batch broadcasts.
type channelAccumulator struct {
	batch *publicationBatcher
	ch    string

	mu         sync.Mutex
	cond       *sync.Cond // "space available" / "closed" / "drained" signal
	buf        []*Publication
	bytes      int
	lastAppend time.Time
	// inFlight counts broadcasts started but not yet returned. The oversize
	// bypass waits on it, not just on an empty buffer: an empty buffer alone
	// would let the immediate send race the drained batch's fan-out (they
	// run on different goroutines), reordering the channel on the wire.
	inFlight int
	closed   bool

	// notify wakes the flusher on the first buffered message (window start)
	// and on threshold crossings; flushNow requests an immediate drain
	// (oversize bypass). Both are cap-1 so append never blocks on them.
	notify   chan struct{}
	flushNow chan struct{}
}

func newChannelAccumulator(batch *publicationBatcher, ch string) *channelAccumulator {
	a := &channelAccumulator{
		batch:    batch,
		ch:       ch,
		notify:   make(chan struct{}, 1),
		flushNow: make(chan struct{}, 1),
	}
	a.cond = sync.NewCond(&a.mu)
	return a
}

// append buffers pub, blocking while the buffer is at either cap — that
// block is the backpressure contract, not a defect. On a closed accumulator
// (reaped or shutting down) the publication falls through to the immediate
// path so nothing is dropped.
func (a *channelAccumulator) append(pub *Publication) error {
	size := len(pub.Payload)
	a.mu.Lock()
	for !a.closed && (len(a.buf) >= a.batch.settings.MaxMessages || a.bytes+size > a.batch.settings.MaxBatchBytes) {
		a.batch.countBackpressure()
		a.cond.Wait()
	}
	if a.closed {
		a.mu.Unlock()
		return a.batch.immediate(a.ch, pub)
	}
	wasEmpty := len(a.buf) == 0
	a.buf = append(a.buf, pub)
	a.bytes += size
	a.lastAppend = time.Now()
	threshold := len(a.buf) >= a.batch.settings.MaxMessages || a.bytes >= a.batch.settings.MaxBatchBytes
	a.mu.Unlock()

	// The first buffered message starts the flush window; a threshold
	// crossing flushes right away. One signal covers both via the flusher's
	// threshold re-check.
	if wasEmpty || threshold {
		select {
		case a.notify <- struct{}{}:
		default:
		}
	}
	a.batch.addPending(size)
	return nil
}

// flushAndWait requests a drain and blocks until the buffer is empty AND
// the drained batch's broadcast has returned. It backs the oversize bypass:
// the unbatchable publication must neither jump ahead of buffered ones nor
// race their fan-out. Never deadlocks: the flusher never waits on the
// appender.
func (a *channelAccumulator) flushAndWait() {
	a.mu.Lock()
	select {
	case a.flushNow <- struct{}{}:
	default:
	}
	for (len(a.buf) > 0 || a.inFlight > 0) && !a.closed {
		a.cond.Wait()
	}
	a.mu.Unlock()
}

// run is the flusher loop. The timer serves two armed states: with a
// non-empty buffer it is the max_delay flush window; with an empty buffer it
// counts down the idle-reap TTL. A trickle (messages below the threshold)
// therefore still flushes within max_delay of its first message.
func (a *channelAccumulator) run() {
	defer a.batch.wg.Done()
	timer := time.NewTimer(a.batch.settings.IdleReap)
	defer timer.Stop()
	for {
		select {
		case <-a.batch.done:
			a.flush(batchTriggerShutdown)
			a.close()
			return
		case <-a.flushNow:
			a.flush(batchTriggerExplicit)
		case <-a.notify:
			// Threshold crossing flushes now; a first-message signal only
			// starts the window (the re-arm below handles it).
			if a.thresholdReached() {
				a.flush(batchTriggerMessages)
			}
		case <-timer.C:
			a.mu.Lock()
			nonEmpty := len(a.buf) > 0
			a.mu.Unlock()
			if nonEmpty {
				a.flush(batchTriggerDelay)
			} else {
				// The idle TTL elapsed with nothing buffered: reap.
				a.reap()
				return
			}
		}

		// Re-arm: window timer when messages are buffered, idle countdown
		// otherwise.
		a.mu.Lock()
		nonEmpty := len(a.buf) > 0
		idle := time.Since(a.lastAppend)
		a.mu.Unlock()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if nonEmpty {
			timer.Reset(a.batch.settings.MaxDelay)
		} else {
			remain := a.batch.settings.IdleReap - idle
			if remain <= 0 {
				remain = time.Millisecond
			}
			timer.Reset(remain)
		}
	}
}

// thresholdReached reports whether the buffer is at either cap.
func (a *channelAccumulator) thresholdReached() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.buf) >= a.batch.settings.MaxMessages || a.bytes >= a.batch.settings.MaxBatchBytes
}

// flush swaps out the buffered publications and broadcasts them as one
// frame. Delivery errors are logged and counted inside the hub fan-out;
// they never fail the broker handler. inFlight brackets the broadcast so
// flushAndWait observers see the drain, not just the swap.
func (a *channelAccumulator) flush(trigger string) {
	a.mu.Lock()
	if len(a.buf) == 0 {
		a.mu.Unlock()
		return
	}
	buf := a.buf
	a.buf = make([]*Publication, 0, 4)
	byteCount := a.bytes
	a.bytes = 0
	a.inFlight++
	a.cond.Broadcast()
	a.mu.Unlock()

	start := time.Now()
	_ = a.batch.broadcast(a.ch, buf)

	a.mu.Lock()
	a.inFlight--
	a.cond.Broadcast()
	a.mu.Unlock()
	a.batch.observeBatch(trigger, len(buf), byteCount, time.Since(start))
}

// close marks the accumulator closed and wakes everyone: blocked appends
// fall through to the immediate path, flushAndWait returns.
func (a *channelAccumulator) close() {
	a.mu.Lock()
	a.closed = true
	a.cond.Broadcast()
	a.mu.Unlock()
}

// reap closes the accumulator and unregisters it. A Deliver racing the reap
// recreates a fresh accumulator on its next threshold-passing publication.
func (a *channelAccumulator) reap() {
	a.close()
	a.batch.removeAccumulator(a.ch, a)
}
