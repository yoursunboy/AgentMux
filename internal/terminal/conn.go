package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
	"github.com/kutonlagos/agentmux/internal/version"
)

// Socket is the part of a WebSocket this package uses.
//
// It is an interface so that the protocol can be tested without a socket,
// which matters more here than in most places: the interesting cases are a
// client that stops reading, a stream that skips a sequence number, and a
// runtime that vanishes mid-frame. Driving those through a real socket means
// driving them through a real TCP buffer, and a test that has to fill a kernel
// buffer to prove that backpressure works is a test that fails on a fast
// machine and passes on a slow one.
//
// *websocket.Conn satisfies this as it stands; there is no adapter.
type Socket interface {
	ReadMessage() (int, []byte, error)
	WriteMessage(messageType int, data []byte) error
	WriteControl(messageType int, data []byte, deadline time.Time) error
	SetReadLimit(limit int64)
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
	SetPongHandler(h func(appData string) error)
	Close() error
}

// outbound is one message waiting to be written.
type outbound struct {
	// kind is a websocket.TextMessage or websocket.BinaryMessage. It is ignored
	// when closeCode is set.
	kind int
	data []byte

	// closeCode, when non-zero, ends the connection once this message is on the
	// wire. Piggy-backing the close on the queue is what makes the explanation
	// arrive before the close does: a client told only that it was disconnected
	// has nothing to act on.
	closeCode int
	closeText string
}

// Conn is one browser connection.
//
// # Concurrency
//
// One reader: readLoop, in the goroutine Serve runs it in. Message handling is
// synchronous inside it, which is not an oversight - keystrokes must reach the
// pty in the order they were typed, and a handler that spawned a goroutine per
// input would let two keystrokes race.
//
// One writer: writeLoop. Every frame from every subscription is funnelled
// through out, so the socket is never written concurrently, which is a
// requirement of the WebSocket protocol rather than a preference of this code.
//
// One goroutine per subscription, plus the ones the manager owns.
//
// # Lock ordering
//
// Where mu is taken together with the hub's connection lock, the hub's is
// taken first. Nothing here takes mu while calling into the hub.
type Conn struct {
	hub *Hub
	ws  Socket
	id  string
	log *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	// out is the write queue. It is bounded, and that is the whole of the
	// backpressure design: a client that stops reading fills it, the writers
	// that fill it up stop being able to queue frames, their subscriptions fall
	// behind the manager's watchers, and those watchers drop chunks - which the
	// subscriptions detect from the sequence numbers and answer by
	// re-synchronising. Memory stays bounded, output is never silently lost,
	// and nothing has to poll.
	//
	// There are two ways a subscription learns it is behind, and they are the
	// same event seen from different ends: the manager's watcher drops a chunk
	// and the next sequence number skips, or the queue here refuses a frame for
	// longer than Timings.Queue. A subscription answers both the same way. The
	// second one is checked because the first does not always fire - a client
	// that is slow enough to fill this queue may still be fast enough that the
	// watcher's own buffer absorbs the difference, in which case nothing would
	// ever be lost and nothing would ever be noticed, while this queue sat full.
	out chan outbound

	// done is closed when the connection is finished. Every goroutine here
	// selects on it.
	done      chan struct{}
	closeOnce sync.Once

	// writerDone is closed when writeLoop returns, so that Serve can let a
	// queued close frame reach the client first.
	writerDone chan struct{}

	// pendingClose records that a close frame is queued. Serve only waits for
	// the writer when one is, because in the ordinary case - the client closed
	// the tab - there is nothing to flush and waiting would be waiting for a
	// goroutine that is waiting for this one.
	pendingClose atomic.Bool

	mu   sync.Mutex
	subs map[string]*subscription

	// subWG tracks subscription goroutines so that the disconnect record is
	// written after them rather than in the middle of them.
	subWG sync.WaitGroup

	framesIn  atomic.Uint64
	bytesIn   atomic.Uint64
	framesOut atomic.Uint64
	bytesOut  atomic.Uint64
}

// newConn builds a connection. It does not start it; Serve does.
func newConn(h *Hub, ws Socket, remote string, n uint64) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		hub:        h,
		ws:         ws,
		id:         fmt.Sprintf("c_%06d", n),
		log:        h.log.With("clientId", fmt.Sprintf("c_%06d", n), "remote", remote),
		ctx:        ctx,
		cancel:     cancel,
		out:        make(chan outbound, outboundQueueDepth),
		done:       make(chan struct{}),
		writerDone: make(chan struct{}),
		subs:       make(map[string]*subscription),
	}
	return c
}

// outboundQueueDepth bounds how many frames may be waiting for one browser.
//
// The bound is what makes a slow client cost a fixed amount of memory instead
// of an amount that grows with how long it stays slow. Sixty-four frames is
// several seconds of ordinary output and a fraction of a second of a build's,
// and a client that cannot keep up with that is not being served by queueing
// more for it.
const outboundQueueDepth = 64

// run drives the connection until it ends.
func (c *Conn) run() {
	go c.writeLoop()

	// The greeting is queued before anything else can be, so a client always
	// learns the protocol version before it can be sent a frame it does not
	// understand.
	c.enqueueText(encodeJSON(helloMessage{
		Type:     MsgHello,
		Protocol: ProtocolVersion,
		ClientID: c.id,
		Server:   version.AppName,
		Version:  version.Version,
	}))

	c.readLoop()

	// The read loop has ended. If a close is queued, give the writer a moment
	// to put the error and the close on the wire; the browser's console is the
	// only place that explanation can appear.
	if c.pendingClose.Load() {
		select {
		case <-c.writerDone:
		case <-time.After(c.hub.t.CloseGrace):
		}
	}
	c.close()
	c.subWG.Wait()
}

// ---------------------------------------------------------------------------
// Writing

// writeLoop is the connection's only writer.
func (c *Conn) writeLoop() {
	defer close(c.writerDone)

	ticker := time.NewTicker(c.hub.t.Ping)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case msg := <-c.out:
			if !c.write(msg) {
				c.close()
				return
			}
			if msg.closeCode != 0 {
				// The close frame is written after the message that explains
				// it, and then the connection is over.
				deadline := c.hub.now().Add(c.hub.t.Write)
				_ = c.ws.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(msg.closeCode, msg.closeText), deadline)
				c.close()
				return
			}
		case <-ticker.C:
			deadline := c.hub.now().Add(c.hub.t.Write)
			if err := c.ws.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				c.close()
				return
			}
		}
	}
}

// write puts one message on the wire. It reports whether the connection is
// still usable.
func (c *Conn) write(msg outbound) bool {
	deadline := c.hub.now().Add(c.hub.t.Write)
	if err := c.ws.SetWriteDeadline(deadline); err != nil {
		return false
	}
	if msg.closeCode != 0 {
		return true
	}
	if err := c.ws.WriteMessage(msg.kind, msg.data); err != nil {
		c.log.Debug("terminal socket write failed", "error", err)
		return false
	}
	c.framesOut.Add(1)
	c.bytesOut.Add(uint64(len(msg.data)))
	return true
}

// enqueue queues a frame, waiting for room.
//
// It reports whether the frame was queued. A caller that gets false has lost
// the frame and must treat that as it treats any other loss of output.
func (c *Conn) enqueue(msg outbound) bool {
	select {
	case c.out <- msg:
		return true
	case <-c.ctx.Done():
		return false
	}
}

// enqueueText queues a control message. Control messages are never allowed to
// displace terminal output or be dropped silently; the only way they fail is
// the connection ending, and then there is nobody left to tell.
func (c *Conn) enqueueText(data []byte) {
	c.enqueue(outbound{kind: websocket.TextMessage, data: data})
}

// delivery is what became of a frame handed to the connection's writer.
//
// The three outcomes are distinct because the subscription does something
// different for each, and two of them look identical from the outside - a frame
// that did not arrive. Collapsing "the client is behind" into "the connection
// is over" would end a subscription that a fresh snapshot would have fixed, and
// ending it is silent: the browser would show a terminal that had simply
// stopped producing output, with nothing to say why.
type delivery int

const (
	// delivered means the frame is queued for writing.
	delivered delivery = iota

	// congested means the queue stayed full for longer than the transport is
	// willing to hold a frame. The client is behind rather than gone, so the
	// subscription re-establishes itself instead of ending.
	congested

	// disconnected means the connection is over and nothing more will be sent.
	disconnected
)

// enqueueBinary queues terminal bytes, giving up if the client is too far
// behind for them to be worth delivering.
//
// The timer is created rather than taken from time.After because this is the
// hot path - one call per output frame, for the life of every connection - and
// time.After's timer cannot be stopped, so each call would leave a timer alive
// for the whole of Queue whether or not it was needed.
func (c *Conn) enqueueBinary(data []byte) delivery {
	timer := time.NewTimer(c.hub.t.Queue)
	defer timer.Stop()

	select {
	case c.out <- outbound{kind: websocket.BinaryMessage, data: data}:
		return delivered
	case <-c.ctx.Done():
		return disconnected
	case <-timer.C:
		// The client is not reading. The frame is dropped and the subscription
		// that produced it re-synchronises; a fresh snapshot is a better answer
		// than a frame that arrives after everything it was written before.
		return congested
	}
}

// sendError queues an error message.
func (c *Conn) sendError(code, message, projectID, about string) {
	c.enqueueText(encodeJSON(errorMessage{
		Type:      MsgError,
		Code:      code,
		Message:   message,
		ProjectID: projectID,
		About:     about,
	}))
}

// close ends the connection. It is idempotent and safe from any goroutine.
func (c *Conn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.cancel()
		// Closing the socket is what unblocks a write that is stuck on a client
		// that has stopped reading. The writer owns writes; this does not
		// write, it only ends.
		_ = c.ws.Close()
	})
}

// shutdown closes the connection and says why, for a server that is stopping.
//
// The close code is CloseGoingAway, which is the one a WebSocket client is
// expected to reconnect after. A browser that saw CloseNormalClosure would be
// entitled to treat the session as finished.
func (c *Conn) shutdown() {
	c.pendingClose.Store(true)
	c.enqueue(outbound{
		closeCode: websocket.CloseGoingAway,
		closeText: "the server is shutting down",
	})
}

// ---------------------------------------------------------------------------
// Reading

// readLoop reads client messages until the connection ends.
func (c *Conn) readLoop() {
	c.ws.SetReadLimit(MaxClientMessageBytes)
	if err := c.ws.SetReadDeadline(c.hub.now().Add(c.hub.t.Pong)); err != nil {
		return
	}
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(c.hub.now().Add(c.hub.t.Pong))
	})

	for {
		kind, data, err := c.ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.log.Debug("terminal socket read ended", "error", err)
			}
			return
		}
		c.framesIn.Add(1)
		c.bytesIn.Add(uint64(len(data)))

		if kind != websocket.TextMessage {
			// No client message is binary: raw input is bytes, but it is sent
			// as base64 inside a typed, size-limited, individually rejectable
			// message. A binary frame from a browser is a client that is not
			// speaking this protocol, and continuing would mean guessing what
			// it meant.
			c.sendError(CodeUnsupported, "a client sends text frames only", "", "")
			c.closeWith(websocket.CloseUnsupportedData, "binary frames are not accepted")
			return
		}
		if !c.handleText(data) {
			return
		}
	}
}

// closeWith queues a close frame and stops reading.
//
// It returns nothing because the caller is already returning: the read loop
// ends, and Serve waits for the queued close to be written.
func (c *Conn) closeWith(code int, text string) {
	c.pendingClose.Store(true)
	c.enqueue(outbound{closeCode: code, closeText: text})
}

// handleText dispatches one client message. It reports whether the connection
// should keep reading.
func (c *Conn) handleText(data []byte) bool {
	var msg clientMessage
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&msg); err != nil {
		c.sendError(CodeBadMessage, "message is not a valid "+version.AppName+" terminal message: "+err.Error(), "", "")
		return true
	}
	// One message is one JSON object. A message with something after it is a
	// client that has mis-framed or double-sent, and reading the first value and
	// ignoring the rest would answer it with a success it did not earn - which
	// for an input message means typing bytes at a terminal on the strength of a
	// message the server did not fully understand.
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		c.sendError(CodeBadMessage, "a message must be exactly one JSON object", "", "")
		return true
	}

	switch msg.Type {
	case MsgSubscribe:
		return c.handleSubscribe(msg)
	case MsgUnsubscribe:
		return c.handleUnsubscribe(msg)
	case MsgInput:
		return c.handleInput(msg)
	case MsgResize:
		return c.handleResize(msg)
	case MsgResync:
		return c.handleResync(msg)
	case MsgPing:
		if msg.ProjectID != "" || msg.hasExtraFields() {
			c.sendError(CodeBadMessage, "a ping carries no other fields", "", MsgPing)
			return true
		}
		c.enqueueText(encodeJSON(pongMessage{Type: MsgPong}))
		return true
	case "":
		c.sendError(CodeBadMessage, "message has no type", "", "")
		return true
	default:
		c.sendError(CodeBadMessage, fmt.Sprintf("unknown message type %q", msg.Type), "", "")
		return true
	}
}

// ---------------------------------------------------------------------------
// Subscriptions

// handleSubscribe starts watching a project's terminal.
//
// Subscribing to a project already being watched is not an error. It is a
// re-synchronisation: the client is asking to be shown the terminal, and a
// fresh snapshot is exactly that. Treating it as a mistake would mean a client
// whose view drifted had no way to ask for it again short of dropping and
// reopening the whole socket.
func (c *Conn) handleSubscribe(msg clientMessage) bool {
	if !validProjectID(msg.ProjectID) {
		c.sendError(CodeBadProject,
			"projectId must be an AgentMux project identifier", msg.ProjectID, MsgSubscribe)
		return true
	}
	if len(msg.Data) > 0 {
		c.sendError(CodeBadMessage, "a subscribe carries no data", msg.ProjectID, MsgSubscribe)
		return true
	}
	cols, rows, err := normalizeSize(msg.Cols, msg.Rows)
	if err != nil {
		c.sendError(session.CodeInvalidSize, err.Error(), msg.ProjectID, MsgSubscribe)
		return true
	}

	c.mu.Lock()
	existing := c.subs[msg.ProjectID]
	overLimit := existing == nil && len(c.subs) >= MaxSubscriptions
	c.mu.Unlock()
	if overLimit {
		c.sendError(CodeTooManySubscriptions,
			fmt.Sprintf("this connection is already watching %d terminals", MaxSubscriptions),
			msg.ProjectID, MsgSubscribe)
		return true
	}

	// The project is resolved before anything is created. This is the whole of
	// the boundary that stops a browser naming a filesystem path: it passes an
	// identifier, and the manager decides what that identifier is. A client
	// cannot reach a terminal the server does not already know about, and it
	// cannot make the server open one either - subscribing to a stopped
	// project is refused, not answered by starting it.
	ctx, cancel := context.WithTimeout(c.ctx, c.hub.t.Lookup)
	_, err = c.hub.rt.Runtime(ctx, msg.ProjectID)
	cancel()
	if err != nil {
		c.sendError(errorCode(err), errorText(err), msg.ProjectID, MsgSubscribe)
		return true
	}

	// The size is applied before the snapshot is taken, so the screen the
	// client is sent is the one its terminal will be the right shape for.
	// Without this the client would draw a screen at the runtime's old size and
	// then reflow it, which is visible and unnecessary.
	if cols > 0 {
		c.applySize(msg.ProjectID, cols, rows)
	}

	if existing != nil {
		existing.requestResync()
		return true
	}

	sub := c.startSubscription(msg.ProjectID)
	if sub == nil {
		// Lost a race: another message on this same connection subscribed to
		// the same project between the check above and here. The read loop is
		// single-threaded, so this cannot happen today; it is handled rather
		// than asserted because the alternative is a second goroutine owning
		// the same project, which is the kind of bug that surfaces as
		// interleaved output months later.
		c.sendError(CodeInternal, "subscription already exists", msg.ProjectID, MsgSubscribe)
	}
	return true
}

// handleUnsubscribe stops watching a project.
func (c *Conn) handleUnsubscribe(msg clientMessage) bool {
	if !validProjectID(msg.ProjectID) {
		c.sendError(CodeBadProject,
			"projectId must be an AgentMux project identifier", msg.ProjectID, MsgUnsubscribe)
		return true
	}
	if msg.hasExtraFields() {
		c.sendError(CodeBadMessage, "an unsubscribe carries only a projectId", msg.ProjectID, MsgUnsubscribe)
		return true
	}

	c.mu.Lock()
	sub, ok := c.subs[msg.ProjectID]
	if ok {
		delete(c.subs, msg.ProjectID)
	}
	c.mu.Unlock()

	if !ok {
		// Unsubscribing from a terminal this connection does not watch is not
		// an error worth refusing: the client's intent - I am no longer
		// watching this - is already true. The acknowledgement is still sent,
		// because a client tearing down a view needs to know the server has
		// stopped sending for it.
		c.enqueueText(encodeJSON(unsubscribedMessage{Type: MsgUnsubscribed, ProjectID: msg.ProjectID}))
		return true
	}
	sub.stop()
	c.enqueueText(encodeJSON(unsubscribedMessage{Type: MsgUnsubscribed, ProjectID: msg.ProjectID}))
	c.log.Info("terminal unsubscribed", "projectId", msg.ProjectID)
	return true
}

// handleInput delivers raw terminal bytes.
func (c *Conn) handleInput(msg clientMessage) bool {
	if !validProjectID(msg.ProjectID) {
		c.sendError(CodeBadProject,
			"projectId must be an AgentMux project identifier", msg.ProjectID, MsgInput)
		return true
	}
	if msg.Cols != 0 || msg.Rows != 0 {
		c.sendError(CodeBadMessage, "input carries no size", msg.ProjectID, MsgInput)
		return true
	}
	if len(msg.Data) == 0 {
		return true
	}
	if len(msg.Data) > MaxInputBytes {
		c.sendError(CodeInputTooLarge,
			fmt.Sprintf("one input message may carry at most %d bytes; send a paste in several messages", MaxInputBytes),
			msg.ProjectID, MsgInput)
		return true
	}
	// Input is only accepted for a terminal this connection has already asked
	// for. That is what stops the socket being an arbitrary write into any
	// project the server happens to know about, and it matches what a terminal
	// is: you can type into one you are looking at.
	if !c.watches(msg.ProjectID) {
		c.sendError(CodeNotSubscribed,
			"subscribe to this project before sending input to it", msg.ProjectID, MsgInput)
		return true
	}

	ctx, cancel := context.WithTimeout(c.ctx, c.hub.t.Input)
	defer cancel()
	if err := c.hub.rt.Input(ctx, msg.ProjectID, msg.Data); err != nil {
		c.sendError(errorCode(err), errorText(err), msg.ProjectID, MsgInput)
	}
	// Nothing about the bytes is logged, at any level. A prompt is what a
	// person typed, and a log is the easiest place for it to end up somewhere
	// nobody meant it to be.
	return true
}

// handleResize sets a project's canonical terminal size.
func (c *Conn) handleResize(msg clientMessage) bool {
	if !validProjectID(msg.ProjectID) {
		c.sendError(CodeBadProject,
			"projectId must be an AgentMux project identifier", msg.ProjectID, MsgResize)
		return true
	}
	if len(msg.Data) > 0 {
		c.sendError(CodeBadMessage, "a resize carries no data", msg.ProjectID, MsgResize)
		return true
	}
	if !c.watches(msg.ProjectID) {
		c.sendError(CodeNotSubscribed,
			"subscribe to this project before resizing it", msg.ProjectID, MsgResize)
		return true
	}
	cols, rows, err := normalizeSize(msg.Cols, msg.Rows)
	if err != nil {
		c.sendError(session.CodeInvalidSize, err.Error(), msg.ProjectID, MsgResize)
		return true
	}
	if cols == 0 {
		c.sendError(session.CodeInvalidSize, "a resize must carry cols and rows", msg.ProjectID, MsgResize)
		return true
	}
	c.applySize(msg.ProjectID, cols, rows)
	return true
}

// handleResync re-establishes a subscription from a fresh snapshot.
func (c *Conn) handleResync(msg clientMessage) bool {
	if !validProjectID(msg.ProjectID) {
		c.sendError(CodeBadProject,
			"projectId must be an AgentMux project identifier", msg.ProjectID, MsgResync)
		return true
	}
	if len(msg.Data) > 0 || msg.Cols != 0 || msg.Rows != 0 {
		c.sendError(CodeBadMessage, "a resync carries only a projectId", msg.ProjectID, MsgResync)
		return true
	}
	c.mu.Lock()
	sub := c.subs[msg.ProjectID]
	c.mu.Unlock()
	if sub == nil {
		// Resynchronising a terminal the connection is not watching is a
		// subscribe by another name, and that is what it is answered with.
		return c.handleSubscribe(clientMessage{Type: MsgSubscribe, ProjectID: msg.ProjectID})
	}
	sub.requestResync()
	return true
}

// applySize sets a project's size, unless it already has it.
//
// The check is what keeps a client that re-measures on every layout pass from
// sending a pty resize per measurement. A resize is not free at the other end:
// it makes every full-screen program redraw, and a stream of redundant ones is
// visible as flicker. The hub holds the value rather than the connection
// because the value belongs to the terminal, which outlives every browser
// watching it.
func (c *Conn) applySize(projectID string, cols, rows int) {
	if current, ok := c.hub.canonicalSize(projectID); ok && current.cols == cols && current.rows == rows {
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, c.hub.t.Input)
	defer cancel()

	rt, err := c.hub.rt.Resize(ctx, projectID, cols, rows)
	if err != nil {
		c.sendError(errorCode(err), errorText(err), projectID, MsgResize)
		return
	}
	// The runtime's answer is the truth, not the request: the backend may have
	// clamped or adjusted it, and every viewer has to be told the size that was
	// actually applied.
	applied := size{cols: cols, rows: rows}
	if rt != nil {
		applied = size{cols: rt.Cols, rows: rt.Rows}
	}
	message := c.hub.broadcastResize(projectID, applied.cols, applied.rows)

	if !c.watches(projectID) {
		// Nobody broadcast to this connection, because it is not a viewer of
		// the project yet. That is exactly the case a subscribe-with-size is
		// in: the size is applied before the subscription is created, so that
		// the snapshot is drawn at the new size. Without this the client would
		// never learn what was applied - and with a clamped request, which is
		// what a 600-column tablet produces, it would draw a terminal of the
		// size it asked for rather than the size the pane is.
		c.enqueueText(message)
	}
}

// startSubscription creates and starts a subscription, or returns nil if one
// already exists.
func (c *Conn) startSubscription(projectID string) *subscription {
	c.mu.Lock()
	if _, exists := c.subs[projectID]; exists {
		c.mu.Unlock()
		return nil
	}
	sub := newSubscription(c, projectID)
	c.subs[projectID] = sub
	c.mu.Unlock()

	c.subWG.Add(1)
	go func() {
		defer c.subWG.Done()
		sub.run()
	}()
	c.log.Info("terminal subscribed", "projectId", projectID, "subscriptions", c.subscriptionCount())
	return sub
}

// forgetSubscription removes a subscription from this connection if it is still
// the one registered under that project.
//
// The identity check is what makes it safe to call from a subscription that is
// ending while a newer one for the same project has already taken its place: a
// goroutine that has been superseded must not unregister its successor.
func (c *Conn) forgetSubscription(projectID string, s *subscription) {
	c.mu.Lock()
	if c.subs[projectID] == s {
		delete(c.subs, projectID)
	}
	c.mu.Unlock()
}

// dropSubscription removes a subscription that gave up.
func (c *Conn) dropSubscription(projectID string) {
	c.mu.Lock()
	sub, ok := c.subs[projectID]
	if ok {
		delete(c.subs, projectID)
	}
	c.mu.Unlock()
	if ok {
		sub.stop()
		c.enqueueText(encodeJSON(unsubscribedMessage{Type: MsgUnsubscribed, ProjectID: projectID}))
	}
}

// watches reports whether this connection is subscribed to a project.
func (c *Conn) watches(projectID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.subs[projectID]
	return ok
}

// subscriptionCount reports how many projects this connection watches.
func (c *Conn) subscriptionCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.subs)
}

// ---------------------------------------------------------------------------
// Helpers


// validProjectID reports whether a client-supplied identifier could name a
// project.
//
// It is a shape check, not an authorisation: the manager decides what exists.
// What it stops is an identifier that is obviously not one - a path, a name
// with a separator or a control character in it, an enormous string - from
// being carried as far as a lookup, a log line or an error message.
//
// The check itself is the one the rest of the server applies, rather than a
// second, weaker one written here. Two shape checks that disagree is how a
// transport becomes the single path into the runtime that accepts something
// nothing else does, and the disagreement is silent: the identifier is looked
// up, is not found, and is reported as a missing project rather than as a
// request that was never well-formed.
func validProjectID(id string) bool {
	return project.ValidID(id)
}

// normalizeSize validates a requested terminal size.
//
// Zero, zero means "no size given", which is legitimate on a subscribe and not
// on a resize. A size that is given must be complete and within the bounds;
// the bounds are stated in the error so that a client author reading it knows
// what to send instead.
func normalizeSize(cols, rows int) (int, int, error) {
	if cols == 0 && rows == 0 {
		return 0, 0, nil
	}
	if cols == 0 || rows == 0 {
		return 0, 0, errors.New("a terminal size needs both cols and rows")
	}
	if cols < 0 || rows < 0 {
		return 0, 0, fmt.Errorf("terminal size %dx%d is not usable", cols, rows)
	}
	clampedCols := clamp(cols, MinCols, MaxCols)
	clampedRows := clamp(rows, MinRows, MaxRows)
	return clampedCols, clampedRows, nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// errorCode extracts the stable code from a runtime error.
func errorCode(err error) string {
	var runtimeErr *session.Error
	if errors.As(err, &runtimeErr) && runtimeErr.Code != "" {
		return runtimeErr.Code
	}
	return CodeInternal
}

// errorText extracts the message a client may see.
//
// It uses Message rather than Error() on purpose: Error() appends the wrapped
// cause, which is where a filesystem path or a command line lives, and the
// only thing that needs to read those is the server's own log.
func errorText(err error) string {
	var runtimeErr *session.Error
	if errors.As(err, &runtimeErr) && runtimeErr.Message != "" {
		return runtimeErr.Message
	}
	return "the terminal request could not be completed"
}

// sleepContext waits, reporting false if the context ended first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// maxSequence is the highest sequence number, used to ask Watch for a
// registration and no backlog.
//
// A snapshot supersedes everything at or below its boundary, so buffered
// chunks are never used. Asking for them anyway would copy the manager's ring
// buffer once per subscribe, which is a cost paid on every reconnect for
// bytes that are then thrown away.
const maxSequence = math.MaxUint64
