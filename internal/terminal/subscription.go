package terminal

import (
	"context"
	"sync"

	"github.com/kutonlagos/agentmux/internal/session"
)

// A subscription is one project's terminal, carried to one browser.
//
// It owns three things that have to agree with each other: a manager watcher,
// a snapshot boundary, and a sequence number that says what the client has
// been given. The rest of this file is about keeping those three consistent
// while output arrives, and about what to do at the moment they cannot be.
//
// # Why a subscription re-establishes itself rather than catching up
//
// When output is lost - the connection could not carry it, or the manager's
// watcher fell behind - there are two things this could do. It could ask the
// manager for the missing chunks and replay them. Or it could throw away what
// it has and take a fresh screen.
//
// It takes the fresh screen, for a reason that is a property of terminals
// rather than a preference. A chunk is not self-contained: it is a run of
// escape sequences whose meaning depends on the state of the terminal that
// received everything before it. Replaying a suffix of a stream into a
// terminal that never saw the prefix draws something that is not merely
// incomplete but wrong - colours leak, a mode set before the gap is never set,
// a cursor placed relative to a screen that is not there. A snapshot has no
// such dependency: it is a statement about the terminal now, and it is true
// whenever it is applied.
//
// The cost is a capture-pane round trip. The alternative is a client showing
// the wrong thing while believing it is showing the right thing, which is
// worse than a client that briefly shows nothing.
type subscription struct {
	conn      *Conn
	projectID string

	// ctx is cancelled when the connection closes or the subscription stops.
	ctx    context.Context
	cancel context.CancelFunc

	// done is closed when run returns.
	done chan struct{}

	// resync requests carry a re-establishment. The buffer is one deep: a
	// second request while one is pending asks for the same thing, and the
	// answer to both is one fresh snapshot.
	resync chan struct{}

	stopOnce sync.Once
}

// newSubscription builds a stopped-until-run subscription.
func newSubscription(c *Conn, projectID string) *subscription {
	ctx, cancel := context.WithCancel(c.ctx)
	return &subscription{
		conn:      c,
		projectID: projectID,
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		resync:    make(chan struct{}, 1),
	}
}

// stop ends the subscription. It is idempotent.
func (s *subscription) stop() {
	s.stopOnce.Do(s.cancel)
}

// requestResync asks for a fresh snapshot.
//
// It never blocks. A caller asking for a resync is on the read loop, and a
// read loop that blocks on a subscription is a connection that has stopped
// answering the person using it.
func (s *subscription) requestResync() {
	select {
	case s.resync <- struct{}{}:
	default:
	}
}

// streamOutcome says why a stream ended, and therefore what to do next.
type streamOutcome int

const (
	// streamGap means the connection lost output. A fresh snapshot fixes it.
	streamGap streamOutcome = iota

	// streamEnded means the stream stopped for a reason that retrying would not
	// change: the connection is closing, or the runtime behind it is no longer
	// the one that was subscribed to.
	streamEnded

	// streamFailed means the runtime refused, or the terminal is gone. Retrying
	// would be a loop against something that is not there.
	streamFailed

	// streamContinues means nothing went wrong. It is never returned by stream,
	// only by the steps inside it, and it is last so that the outcomes that end
	// something come first.
	streamContinues
)

// run carries one project's terminal until it is stopped.
func (s *subscription) run() {
	defer close(s.done)

	// A subscription removes itself from its connection when it ends, whatever
	// the reason. Leaving it in the map would be worse than a leak: the next
	// subscribe to the same project would find it, take it for a subscription
	// that is still running, and answer with a re-synchronisation request - a
	// message to a goroutine that has already returned, and a terminal that
	// never redraws for the rest of the connection's life.
	defer s.conn.forgetSubscription(s.projectID, s)

	resyncs := 0
	for {
		if s.ctx.Err() != nil {
			return
		}
		switch s.stream() {
		case streamEnded, streamFailed:
			return
		case streamGap:
		}

		resyncs++
		if resyncs > maxResyncs {
			// The connection cannot carry this project's output, or the runtime
			// produces it faster than a snapshot can be taken. Either way,
			// another attempt is another trip round a loop that has already
			// failed eight times.
			s.conn.sendError(CodeStreamUnstable,
				"this terminal is producing more output than the connection can carry; the subscription was stopped",
				s.projectID, MsgSubscribe)
			s.conn.dropSubscription(s.projectID)
			return
		}
		s.conn.log.Info("terminal re-synchronising",
			"projectId", s.projectID, "resync", resyncs)

		if !sleepContext(s.ctx, s.conn.hub.t.ResyncPause) {
			return
		}
	}
}

// batch is the output waiting to be framed.
type batch struct {
	pending []byte
	first   uint64
	last    uint64
}

// flushBytes caps one output frame.
//
// Frames are batched so that the frame count follows the number of bursts a
// program produces rather than the number of chunks tmux emitted, and this cap
// is what stops one burst from becoming a frame larger than a browser will
// take comfortably. Beyond it the frame is sent and a new one begins, which
// costs one extra frame per 32 KiB and bounds the transient allocation.
const flushBytes = 32 << 10

// stream establishes the subscription, forwards output, and returns when it
// can no longer be trusted.
func (s *subscription) stream() streamOutcome {
	// Register before capturing, never the other way round.
	//
	// Chunks published between a capture and a registration are in neither: too
	// late for the screen that was already taken, too early for a watcher that
	// did not exist. That is a gap, and a gap is the one thing this design must
	// not manufacture, because it is the one thing a client cannot distinguish
	// from data it simply has not been sent. Registering first costs a watcher
	// that briefly buffers chunks the snapshot will supersede, and it removes
	// the gap entirely.
	//
	// The backlog is deliberately not used. Everything at or below the
	// snapshot's boundary is already drawn in it, so the only thing replaying
	// those chunks could do is draw them twice.
	_, live, cancel, err := s.conn.hub.rt.Watch(s.ctx, s.projectID, maxSequence)
	if err != nil {
		return s.refuse(err)
	}
	defer cancel()

	screen, boundary, err := s.conn.hub.rt.Screen(s.ctx, s.projectID)
	if err != nil {
		return s.refuse(err)
	}
	// The hub's idea of this terminal's size is refreshed from the screen,
	// which is the only thing that actually knows it.
	s.conn.hub.rememberSize(s.projectID, screen.Cols, screen.Rows)

	flags := uint8(0)
	if screen.Alternate {
		flags |= FlagAlternateScreen
	}
	frame, err := EncodeSnapshot(s.projectID, boundary, screen.Cols, screen.Rows, flags, screen.Render())
	if err != nil {
		return s.refuse(err)
	}
	switch s.conn.enqueueBinary(frame) {
	case delivered:
	case congested:
		// The client was already behind when it asked to be shown this
		// terminal. That is the same condition the rest of this file treats as
		// a gap, and it is answered the same way - one step earlier.
		return streamGap
	default:
		// The connection ended while the snapshot was being taken.
		return streamEnded
	}

	// Everything at or below the boundary is in the screen just sent.
	expected := boundary + 1
	var b batch

	for {
		// Block for the first chunk of a frame.
		select {
		case <-s.ctx.Done():
			return streamEnded
		case <-s.resync:
			return streamGap
		case chunk, ok := <-live:
			if !ok {
				s.reportTerminalGone()
				return streamFailed
			}
			if s.absorb(chunk, &expected, &b) {
				return streamGap
			}
		}

		// Take whatever else is already waiting, up to the frame cap.
		//
		// This drain is the whole of the batching, and there is no timer in it
		// on purpose. A timer would add latency to every frame in order to
		// coalesce the ones that arrive together, and the coalescing only
		// matters when there is something to coalesce - which is exactly the
		// case this loop detects by finding another chunk already queued. A
		// single keystroke produces a single frame immediately; a build's
		// output, which tmux delivers as a burst, produces one frame per
		// burst.
		for len(b.pending) < flushBytes {
			select {
			case <-s.ctx.Done():
				return streamEnded
			case <-s.resync:
				return streamGap
			case chunk, ok := <-live:
				if !ok {
					s.reportTerminalGone()
					return streamFailed
				}
				if s.absorb(chunk, &expected, &b) {
					return streamGap
				}
			default:
				goto send
			}
		}
	send:
		switch s.flush(&b) {
		case streamContinues:
		case streamGap:
			return streamGap
		default:
			return streamEnded
		}
	}
}

// absorb adds a chunk to the batch. It reports that output was lost.
//
// The two comparisons are the entire gap-detection scheme, and they are worth
// spelling out because both of them are silent if they are wrong:
//
//   - Below the expected sequence, a chunk is either a duplicate or something
//     the snapshot already contains. It is dropped, not drawn.
//   - Above it, a chunk starts after the one that should have come next, which
//     means the bytes in between were discarded somewhere between the runtime
//     and here. There is no way to ask for them and no safe place to put them,
//     so the batch in progress is abandoned and the stream restarts from a
//     screen.
func (s *subscription) absorb(chunk session.Chunk, expected *uint64, b *batch) (gap bool) {
	if chunk.Sequence < *expected {
		return false
	}
	if chunk.Sequence > *expected {
		return true
	}
	if len(b.pending) == 0 {
		b.first = chunk.Sequence
	}
	b.pending = append(b.pending, chunk.Data...)
	b.last = chunk.Sequence
	*expected = chunk.Sequence + 1
	return false
}

// flush frames the batch and queues it. It reports what should happen next.
//
// A batch that is abandoned rather than flushed - because a gap was found - is
// safe to drop. The snapshot that replaces it is captured after those chunks
// were published, so its boundary is at or above their sequence numbers, and
// the screen it carries already contains them.
func (s *subscription) flush(b *batch) streamOutcome {
	if len(b.pending) == 0 {
		return streamContinues
	}
	payload, first, last := b.pending, b.first, b.last
	b.pending, b.first, b.last = nil, 0, 0

	frame, err := EncodeOutput(s.projectID, first, last, payload)
	if err != nil {
		// Unreachable for a project id that has already been validated and a
		// sequence range built by absorb. It is handled rather than asserted
		// because the alternative to handling it is a panic in a goroutine
		// nothing is watching.
		s.conn.sendError(CodeInternal, err.Error(), s.projectID, MsgSubscribe)
		s.conn.dropSubscription(s.projectID)
		return streamFailed
	}

	switch s.conn.enqueueBinary(frame) {
	case delivered:
		return streamContinues
	case congested:
		// The client is behind. The frame is dropped rather than held for
		// longer, and the subscription re-establishes itself - a snapshot taken
		// when the client catches up is a better answer than a frame that
		// arrives after everything written after it. A client that never
		// catches up ends at maxResyncs with an explanation, which is the
		// outcome a silently frozen terminal does not have.
		return streamGap
	default:
		return streamEnded
	}
}

// refuse reports a runtime failure and ends the subscription.
func (s *subscription) refuse(err error) streamOutcome {
	if s.ctx.Err() != nil {
		// The connection is going away, so the failure is a consequence of
		// that rather than something the client needs told.
		return streamEnded
	}
	s.conn.sendError(errorCode(err), errorText(err), s.projectID, MsgSubscribe)
	s.conn.dropSubscription(s.projectID)
	return streamFailed
}

// reportTerminalGone tells the client that the stream it was watching ended
// because the terminal did.
func (s *subscription) reportTerminalGone() {
	if s.ctx.Err() != nil {
		return
	}
	s.conn.sendError(session.CodeNotRunning,
		"the terminal for this project is no longer running", s.projectID, MsgSubscribe)
	s.conn.dropSubscription(s.projectID)
}
