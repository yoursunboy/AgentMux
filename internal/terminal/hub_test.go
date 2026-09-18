package terminal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kutonlagos/agentmux/internal/session"
)

// These tests drive the transport through its own interfaces rather than
// through a socket, and the reason is in Socket's comment: the cases that
// matter are a client that stops reading, a stream that skips a sequence
// number, and a runtime that vanishes mid-frame. Built on a real TCP
// connection, each of those is a timing experiment; built on these doubles,
// each is a statement about what the code does.
//
// The socket-level half of the contract - the handshake, the greeting over a
// real connection, the origin check - is tested against a real socket in
// internal/httpapi, because a double cannot check a handshake it is not part
// of.

const testProject = "p_0123456789abcdef0123"

// socketWait is how long a test waits for something it is sure will happen.
// The negative assertions use much shorter waits of their own.
const socketWait = 5 * time.Second

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// The runtime double

// fakeTerminal is one project's terminal inside a fakeRuntime.
type fakeTerminal struct {
	cols int
	rows int

	// seq is the highest sequence number emitted.
	seq uint64

	// advance is how far the sequence number moves on each emit. One is what
	// the real runtime does. Two manufactures the gap the manager produces when
	// a watcher falls behind and a chunk is dropped: the chunk the watcher
	// receives skips the one it never got.
	advance uint64

	// screen is what a capture returns. It accumulates everything emitted, so
	// the screen and the boundary it carries stay consistent with each other
	// the way the real manager's do.
	screen    []byte
	alternate bool
	cursorX   int
	cursorY   int

	watchers map[int]chan session.Chunk
	nextID   int

	// refuse, when set, is returned by every call that reaches this terminal.
	// It is how a test produces a runtime that has failed.
	refuse *session.Error

	// retired makes Watch report a closed channel and everything else report
	// that the terminal is not running, which is what the manager does for a
	// project whose runtime record is gone.
	retired bool
}

// fakeRuntime is a Runtime that produces exactly the bytes a test asks for.
type fakeRuntime struct {
	mu    sync.Mutex
	terms map[string]*fakeTerminal

	inputs  []inputCall
	resizes []resizeCall

	// resizeAnswer, when set, is the geometry Resize reports back, so that a
	// test can make the runtime's answer differ from the request. A real
	// backend does that when it clamps.
	resizeAnswer *size

	// screenGate, when non-nil, is waited on at the top of every Screen call,
	// and screenEntered is signalled on entry to one. Together they are how a
	// test arranges for output to pile up before a subscription can read any of
	// it, which is what makes the batching assertion exact rather than a matter
	// of which goroutine the scheduler ran first.
	screenGate    chan struct{}
	screenEntered chan struct{}
}

type inputCall struct {
	projectID string
	data      []byte
}

type resizeCall struct {
	projectID string
	cols      int
	rows      int
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{terms: make(map[string]*fakeTerminal)}
}

// add registers a project with a running terminal of a given size.
func (r *fakeRuntime) add(projectID string, cols, rows int) *fakeRuntime {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.terms[projectID] = &fakeTerminal{cols: cols, rows: rows}
	return r
}

// ---------------------------------------------------------------------------
// fakeRuntime: the Runtime interface

func (r *fakeRuntime) Screen(ctx context.Context, projectID string) (session.Screen, uint64, error) {
	// The terminal is read before the gate, not after it. That is the order the
	// real manager uses and the order the boundary means anything in: the
	// boundary is decided when the capture is asked for, so output produced
	// while the capture is running has a higher sequence number than the screen
	// carries and has to be streamed rather than being assumed to be in it.
	r.mu.Lock()
	t, err := r.termLocked(projectID)
	if err != nil {
		r.mu.Unlock()
		return session.Screen{}, 0, err
	}
	if t.refuse != nil {
		r.mu.Unlock()
		return session.Screen{}, 0, t.refuse
	}
	if t.retired {
		r.mu.Unlock()
		return session.Screen{}, 0, notRunning(projectID)
	}
	screen := session.Screen{
		Data:      append([]byte(nil), t.screen...),
		Cols:      t.cols,
		Rows:      t.rows,
		CursorX:   t.cursorX,
		CursorY:   t.cursorY,
		Alternate: t.alternate,
	}
	boundary := t.seq
	gate, entered := r.screenGate, r.screenEntered
	r.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return session.Screen{}, 0, ctx.Err()
		}
	}
	return screen, boundary, nil
}

func (r *fakeRuntime) Watch(_ context.Context, projectID string, _ uint64) ([]session.Chunk, <-chan session.Chunk, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, err := r.termLocked(projectID)
	if err != nil {
		return nil, nil, nil, err
	}
	if t.retired {
		ch := make(chan session.Chunk)
		close(ch)
		return nil, ch, func() {}, nil
	}
	if t.watchers == nil {
		t.watchers = make(map[int]chan session.Chunk)
	}
	id := t.nextID
	t.nextID++
	ch := make(chan session.Chunk, 256)
	t.watchers[id] = ch

	// The channel is unregistered but not closed. The real manager closes it,
	// and a subscription that saw the close would treat it as the terminal
	// going away - but cancel runs from a defer after the subscription has
	// stopped reading, so the close would say something the subscriber is in no
	// position to hear. retired is how a test produces the case that matters.
	cancel := func() {
		r.mu.Lock()
		delete(t.watchers, id)
		r.mu.Unlock()
	}
	return nil, ch, cancel, nil
}

func (r *fakeRuntime) Input(_ context.Context, projectID string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.termLocked(projectID)
	if err != nil {
		return err
	}
	if t.refuse != nil {
		return t.refuse
	}
	r.inputs = append(r.inputs, inputCall{projectID: projectID, data: append([]byte(nil), data...)})
	return nil
}

func (r *fakeRuntime) Resize(_ context.Context, projectID string, cols, rows int) (*session.Runtime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.termLocked(projectID)
	if err != nil {
		return nil, err
	}
	if t.refuse != nil {
		return nil, t.refuse
	}
	t.cols, t.rows = cols, rows
	r.resizes = append(r.resizes, resizeCall{projectID: projectID, cols: cols, rows: rows})

	answer := size{cols: cols, rows: rows}
	if r.resizeAnswer != nil {
		answer = *r.resizeAnswer
		t.cols, t.rows = answer.cols, answer.rows
	}
	return &session.Runtime{
		ProjectID:    projectID,
		Backend:      "fake",
		Session:      "amx-" + projectID,
		State:        session.StateRunning,
		SessionAlive: true,
		Cols:         answer.cols,
		Rows:         answer.rows,
		Sequence:     t.seq,
	}, nil
}

func (r *fakeRuntime) Runtime(_ context.Context, projectID string) (*session.Runtime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.termLocked(projectID)
	if err != nil {
		return nil, err
	}
	if t.refuse != nil {
		return nil, t.refuse
	}
	if t.retired {
		return nil, notRunning(projectID)
	}
	return &session.Runtime{
		ProjectID:    projectID,
		Backend:      "fake",
		Session:      "amx-" + projectID,
		State:        session.StateRunning,
		SessionAlive: true,
		Cols:         t.cols,
		Rows:         t.rows,
		Sequence:     t.seq,
	}, nil
}

// ---------------------------------------------------------------------------
// fakeRuntime: the test's side

// publish emits one chunk and returns its sequence number.
func (r *fakeRuntime) publish(projectID, data string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.emitLocked(projectID, data)
}

// publishAll emits several chunks with nothing in between, so that a
// subscription that is not yet reading finds all of them waiting.
func (r *fakeRuntime) publishAll(projectID string, chunks ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, chunk := range chunks {
		r.emitLocked(projectID, chunk)
	}
}

// emitLocked assigns the next sequence number, records the bytes in the screen
// a later capture will return, and fans the chunk out to every watcher.
//
// It mirrors Manager.publish, including the part that matters here: a watcher
// that cannot take the chunk loses it rather than slowing the producer down.
func (r *fakeRuntime) emitLocked(projectID, data string) uint64 {
	t := r.terms[projectID]
	advance := t.advance
	if advance == 0 {
		advance = 1
	}
	t.seq += advance
	t.screen = append(t.screen, data...)

	chunk := session.Chunk{ProjectID: projectID, Sequence: t.seq, Data: []byte(data)}
	for _, ch := range t.watchers {
		select {
		case ch <- chunk:
		default:
		}
	}
	return t.seq
}

// skipOnce advances the sequence number without emitting anything, which is
// the state a subscription is in when the manager dropped a chunk it published:
// the chunk exists in the terminal and in the ring buffer, and the watcher
// never saw it.
func (r *fakeRuntime) skipOnce(projectID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.terms[projectID].seq++
}

// skipEveryChunk makes every emitted chunk skip a number, so that a
// subscription can never be re-established without immediately finding another
// gap.
func (r *fakeRuntime) skipEveryChunk(projectID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.terms[projectID].advance = 2
}

// retire makes the terminal go away under a subscription.
func (r *fakeRuntime) retire(projectID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.terms[projectID]
	t.retired = true
	for id, ch := range t.watchers {
		delete(t.watchers, id)
		close(ch)
	}
}

// refuses makes every call that reaches a terminal fail with a runtime error.
func (r *fakeRuntime) refuses(projectID string, err *session.Error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.terms[projectID].refuse = err
}

// setScreen replaces what a capture returns, for a test that needs a terminal
// showing something specific.
func (r *fakeRuntime) setScreen(projectID string, data string, cols, rows int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.terms[projectID]
	t.screen = []byte(data)
	t.cols, t.rows = cols, rows
}

// answersResizeWith makes Resize report a different geometry than it was asked
// for, which is what a backend that clamps does.
func (r *fakeRuntime) answersResizeWith(cols, rows int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resizeAnswer = &size{cols: cols, rows: rows}
}

// holdScreen stops the next capture until releaseScreen, and reports captures
// through tookScreen.
func (r *fakeRuntime) holdScreen() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.screenGate = make(chan struct{})
	r.screenEntered = make(chan struct{}, 16)
}

func (r *fakeRuntime) releaseScreen() {
	r.mu.Lock()
	gate := r.screenGate
	r.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

// tookScreen reports that a capture has started.
func (r *fakeRuntime) tookScreen() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.screenEntered
}

func (r *fakeRuntime) recordedInputs() []inputCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]inputCall(nil), r.inputs...)
}

func (r *fakeRuntime) recordedResizes() []resizeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]resizeCall(nil), r.resizes...)
}

func (r *fakeRuntime) gate() (chan struct{}, chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.screenGate, r.screenEntered
}

func (r *fakeRuntime) termLocked(projectID string) (*fakeTerminal, error) {
	t, ok := r.terms[projectID]
	if !ok {
		return nil, &session.Error{
			Code:    session.CodeProjectNotFound,
			Message: fmt.Sprintf("no project with id %q", projectID),
		}
	}
	return t, nil
}

func notRunning(projectID string) *session.Error {
	return &session.Error{
		Code:    session.CodeNotRunning,
		Message: fmt.Sprintf("no terminal runtime is running for project %s", projectID),
	}
}

// ---------------------------------------------------------------------------
// The socket double

// socketRead is one message ReadMessage returns.
type socketRead struct {
	kind int
	data []byte
	err  error
}

// socketWrite is one message the connection wrote.
type socketWrite struct {
	kind int
	data []byte
}

// socketControl is one control frame the connection wrote.
type socketControl struct {
	messageType int
	data        []byte
	deadline    time.Time
}

// pipeSocket is a Socket with nothing on the other end of it.
//
// Its capacity is the knob for a slow client: a writes channel that has no room
// is a browser that has stopped reading, and the write deadline is what stops
// the connection from waiting on one forever.
type pipeSocket struct {
	reads  chan socketRead
	writes chan socketWrite

	mu            sync.Mutex
	controls      []socketControl
	readLimit     int64
	readDeadline  time.Time
	writeDeadline time.Time
	closed        bool
	pongHandler   func(string) error

	closedCh  chan struct{}
	closeOnce sync.Once
}

var (
	errPipeClosed  = fmt.Errorf("pipeSocket: closed")
	errPipeTimeout = fmt.Errorf("pipeSocket: deadline exceeded")
)

func newPipeSocket(writeBuffer int) *pipeSocket {
	if writeBuffer <= 0 {
		writeBuffer = 256
	}
	return &pipeSocket{
		reads:    make(chan socketRead),
		writes:   make(chan socketWrite, writeBuffer),
		closedCh: make(chan struct{}),
	}
}

func (p *pipeSocket) ReadMessage() (int, []byte, error) {
	for {
		kind, data, err := p.readOne()
		if err != nil {
			return 0, nil, err
		}
		// A control frame is consumed by the handler the connection installed,
		// exactly as the real library consumes it, and does not end the read.
		if kind == websocket.PingMessage || kind == websocket.PongMessage {
			if h := p.handler(); h != nil {
				if err := h(string(data)); err != nil {
					return 0, nil, err
				}
			}
			continue
		}
		return kind, data, nil
	}
}

func (p *pipeSocket) readOne() (int, []byte, error) {
	p.mu.Lock()
	deadline := p.readDeadline
	p.mu.Unlock()

	var timeout <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		timeout = timer.C
	}

	select {
	case r := <-p.reads:
		return r.kind, r.data, r.err
	case <-timeout:
		return 0, nil, errPipeTimeout
	case <-p.closedCh:
		return 0, nil, errPipeClosed
	}
}

func (p *pipeSocket) WriteMessage(kind int, data []byte) error {
	p.mu.Lock()
	deadline := p.writeDeadline
	p.mu.Unlock()

	var timeout <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		timeout = timer.C
	}

	select {
	case p.writes <- socketWrite{kind: kind, data: append([]byte(nil), data...)}:
		return nil
	case <-timeout:
		return errPipeTimeout
	case <-p.closedCh:
		return errPipeClosed
	}
}

func (p *pipeSocket) WriteControl(messageType int, data []byte, deadline time.Time) error {
	select {
	case <-p.closedCh:
		return errPipeClosed
	default:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.controls = append(p.controls, socketControl{
		messageType: messageType,
		data:        append([]byte(nil), data...),
		deadline:    deadline,
	})
	return nil
}

func (p *pipeSocket) SetReadLimit(limit int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readLimit = limit
}

func (p *pipeSocket) SetReadDeadline(t time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readDeadline = t
	return nil
}

func (p *pipeSocket) SetWriteDeadline(t time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writeDeadline = t
	return nil
}

func (p *pipeSocket) SetPongHandler(h func(appData string) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pongHandler = h
}

func (p *pipeSocket) Close() error {
	p.closeOnce.Do(func() { close(p.closedCh) })
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

func (p *pipeSocket) handler() func(string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pongHandler
}

// ---------------------------------------------------------------------------
// Driving the socket

func (p *pipeSocket) sendText(data string) {
	select {
	case p.reads <- socketRead{kind: websocket.TextMessage, data: []byte(data)}:
	case <-p.closedCh:
	}
}

func (p *pipeSocket) sendBinary(data []byte) {
	select {
	case p.reads <- socketRead{kind: websocket.BinaryMessage, data: data}:
	case <-p.closedCh:
	}
}

// sendPong feeds a control frame, which is how a test answers a ping.
func (p *pipeSocket) sendPong() {
	select {
	case p.reads <- socketRead{kind: websocket.PongMessage}:
	case <-p.closedCh:
	}
}

// endReads makes the next read fail, as a socket that was closed under the
// connection does.
func (p *pipeSocket) endReads() {
	select {
	case p.reads <- socketRead{err: errPipeClosed}:
	case <-p.closedCh:
	}
}

func (p *pipeSocket) next(t *testing.T) socketWrite {
	t.Helper()
	select {
	case w := <-p.writes:
		return w
	case <-time.After(socketWait):
		t.Fatal("timed out waiting for the server to write a message")
		return socketWrite{}
	}
}

// awaitControl waits for a control frame of a given type, leaving the others
// in place: the connection pings, and a test waiting for a close frame must not
// be defeated by a ping that arrived first.
func (p *pipeSocket) awaitControl(t *testing.T, messageType int) socketControl {
	t.Helper()
	deadline := time.Now().Add(socketWait)
	for {
		p.mu.Lock()
		for i, ctl := range p.controls {
			if ctl.messageType == messageType {
				p.controls = append(p.controls[:i], p.controls[i+1:]...)
				p.mu.Unlock()
				return ctl
			}
		}
		p.mu.Unlock()

		if time.Now().After(deadline) {
			t.Fatalf("no control frame of type %d was written", messageType)
		}
		time.Sleep(time.Millisecond)
	}
}

func (p *pipeSocket) controlCount(messageType int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, ctl := range p.controls {
		if ctl.messageType == messageType {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// The client harness

// client is one browser: a hub, a connection the hub is serving, and the fake
// socket underneath it.
type client struct {
	t      *testing.T
	hub    *Hub
	rt     *fakeRuntime
	ws     *pipeSocket
	conn   *Conn
	hello  serverMessage
	served chan struct{}
}

type clientOptions struct {
	// timings overrides the transport's timeouts. Nil means fastTimings.
	timings *Timings

	// writeBuffer is the capacity of the socket's outgoing queue. Zero means a
	// comfortable one; a small number is how a test makes the client slow.
	writeBuffer int

	// clientID is the browser session this client claims. Empty means it claims
	// none, which is how a program that is not a browser connects - and the
	// server issues one, which is what most tests below are exercising without
	// meaning to.
	clientID string

	// userAgent is what the client sends as its User-Agent header. It is the
	// only source of the device label other clients see.
	userAgent string

	// controlGrace overrides how long a disconnected controller's lease is held
	// for it. Zero means the production default, which is a test that would have
	// to wait thirty seconds to see a lease lapse.
	controlGrace time.Duration
}

// fastTimings is the transport's timeouts at values a test can wait for.
// Nothing here changes behaviour: every one of them is a wait, and a test that
// waited the production value of any of them would be a test nobody runs.
func fastTimings() *Timings {
	return &Timings{
		Write:       2 * time.Second,
		Queue:       30 * time.Millisecond,
		Ping:        time.Hour,
		Pong:        time.Hour,
		CloseGrace:  200 * time.Millisecond,
		ResyncPause: 5 * time.Millisecond,
		Input:       time.Second,
		Lookup:      time.Second,
		Shutdown:    2 * time.Second,
	}
}

// newHub builds a hub over a runtime, with the transport's timeouts shortened.
func newHub(t *testing.T, rt *fakeRuntime, opts clientOptions) *Hub {
	t.Helper()

	timings := fastTimings()
	if opts.timings != nil {
		timings = opts.timings
	}
	hub, err := NewHub(rt, HubOptions{
		Logger:       discardLogger(),
		Timings:      timings,
		ControlGrace: opts.controlGrace,
	})
	if err != nil {
		t.Fatalf("terminal.NewHub returned an error: %v", err)
	}
	// Registered before any connection is, so that it runs after them: cleanup
	// is last in, first out, and a hub closed under a live connection would
	// turn every test's teardown into a shutdown test.
	t.Cleanup(func() {
		if err := hub.Close(); err != nil {
			t.Errorf("closing the hub failed: %v", err)
		}
	})
	return hub
}

// attach starts a browser on a hub and returns it, without consuming anything
// the server wrote.
func attach(t *testing.T, hub *Hub, rt *fakeRuntime, opts clientOptions) *client {
	t.Helper()
	ws := newPipeSocket(opts.writeBuffer)
	c := &client{
		t:      t,
		hub:    hub,
		rt:     rt,
		ws:     ws,
		served: make(chan struct{}),
	}
	before := hub.latestConnID()
	go func() {
		defer close(c.served)
		hub.Serve(ws, ConnInfo{
			ClientID:  opts.clientID,
			UserAgent: opts.userAgent,
		})
	}()
	c.conn = hub.awaitConn(t, before)

	t.Cleanup(func() {
		_ = ws.Close()
		select {
		case <-c.served:
		case <-time.After(socketWait):
			t.Error("the connection did not end when its socket was closed")
		}
	})
	return c
}

// newClient is one browser on a hub of its own, greeted.
func newClient(t *testing.T, rt *fakeRuntime, opts clientOptions) *client {
	t.Helper()
	c := attach(t, newHub(t, rt, opts), rt, opts)
	c.greet()
	return c
}

// greet consumes the greeting, which is the first thing every connection is
// sent.
func (c *client) greet() {
	c.t.Helper()
	c.hello = c.expectMessage()
}

// awaitConn waits for the hub to register a connection newer than `after`, and
// returns it.
//
// Serve creates the Conn itself, so polling the hub is the only way a test can
// reach the one it just started. The bound is a connection identifier rather
// than "any connection" because a hub in a test is not quiet, and returning
// whichever connection happens to be in the map first would make every
// assertion about client identity a statement about somebody else's client. The
// identifiers are zero-padded, so comparing them as strings compares them in the
// order they were issued.
func (h *Hub) awaitConn(t *testing.T, after string) *Conn {
	t.Helper()
	deadline := time.Now().Add(socketWait)
	for {
		h.mu.Lock()
		var found *Conn
		for c := range h.conns {
			if c.id > after && (found == nil || c.id > found.id) {
				found = c
			}
		}
		h.mu.Unlock()
		if found != nil {
			return found
		}
		if time.Now().After(deadline) {
			t.Fatal("the hub never registered the connection")
		}
		time.Sleep(time.Millisecond)
	}
}

// latestConnID is the highest connection identifier the hub has issued, or the
// empty string if it has issued none.
func (h *Hub) latestConnID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	latest := ""
	for c := range h.conns {
		if c.id > latest {
			latest = c.id
		}
	}
	return latest
}

// ---------------------------------------------------------------------------
// Talking to the client

// serverMessage is every field of every server control message, in one struct,
// so that a test can assert on the JSON a browser actually receives.
type serverMessage struct {
	Type         string      `json:"type"`
	Protocol     int         `json:"protocol"`
	ClientID     string      `json:"clientId"`
	ConnectionID string      `json:"connectionId"`
	Device       string      `json:"device"`
	Server       string      `json:"server"`
	Version      string      `json:"version"`
	Code         string      `json:"code"`
	Message      string      `json:"message"`
	ProjectID    string      `json:"projectId"`
	About        string      `json:"about"`
	Reason       string      `json:"reason"`
	Cols         int         `json:"cols"`
	Rows         int         `json:"rows"`
	Control      controlView `json:"control"`
}

// next reads the next message of either kind.
func (c *client) next() socketWrite {
	c.t.Helper()
	return c.ws.next(c.t)
}

// expectMessage reads until the next control message, skipping terminal frames.
// A browser with a live terminal receives both, and a test waiting for an error
// should not have to know how many frames arrived first.
func (c *client) expectMessage() serverMessage {
	c.t.Helper()
	for {
		w := c.next()
		if w.kind == websocket.TextMessage {
			return c.decode(w)
		}
	}
}

// expectMessageOfType reads until the next control message of a given type,
// failing on any other control message.
func (c *client) expectMessageOfType(want string) serverMessage {
	c.t.Helper()
	for {
		msg := c.expectMessage()
		if msg.Type == want {
			return msg
		}
		c.t.Fatalf("got a %q message while waiting for %q: %+v", msg.Type, want, msg)
	}
}

// expectFrame reads until the next binary frame, skipping control messages.
func (c *client) expectFrame() Frame {
	c.t.Helper()
	for {
		w := c.next()
		if w.kind == websocket.BinaryMessage {
			frame, err := DecodeFrame(w.data)
			if err != nil {
				c.t.Fatalf("the server wrote a frame that does not decode: %v", err)
			}
			return frame
		}
	}
}

// expectSilence asserts that nothing else arrives for a while.
//
// The wait is short and the assertion is negative, so it is used only where the
// server's next action, if it had one, would be immediate.
func (c *client) expectSilence(d time.Duration) {
	c.t.Helper()
	select {
	case w := <-c.ws.writes:
		c.t.Fatalf("the server wrote an unexpected %s", describe(w))
	case <-time.After(d):
	}
}

func (c *client) decode(w socketWrite) serverMessage {
	c.t.Helper()
	var msg serverMessage
	if err := json.Unmarshal(w.data, &msg); err != nil {
		c.t.Fatalf("the server wrote text that is not JSON: %v (%q)", err, w.data)
	}
	return msg
}

func describe(w socketWrite) string {
	kind := "text"
	if w.kind == websocket.BinaryMessage {
		kind = "frame"
	}
	if len(w.data) > 120 {
		return fmt.Sprintf("%s message of %d bytes", kind, len(w.data))
	}
	return fmt.Sprintf("%s message %q", kind, w.data)
}

// send writes one client message.
func (c *client) send(v any) {
	c.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		c.t.Fatalf("the test built a message that will not encode: %v", err)
	}
	c.ws.sendText(string(data))
}

func (c *client) sendRaw(text string) {
	c.t.Helper()
	c.ws.sendText(text)
}

// subscribe watches a project and consumes the roster that answers it.
//
// Every subscribe is answered with the roster - who holds the lease, who is
// waiting, how many others are watching - before anything else reaches the
// connection. Consuming it here keeps the tests that are about output, input
// and size from each having to know about control; the tests that *are* about
// control use sendSubscribe and read the roster themselves.
func (c *client) subscribe(projectID string, cols, rows int) {
	c.t.Helper()
	c.sendSubscribe(projectID, cols, rows)
	c.expectMessageOfType(MsgControlChanged)
}

// sendSubscribe asks to watch a project without waiting for the answer.
func (c *client) sendSubscribe(projectID string, cols, rows int) {
	c.t.Helper()
	msg := map[string]any{"type": MsgSubscribe, "projectId": projectID}
	if cols > 0 {
		msg["cols"] = cols
		msg["rows"] = rows
	}
	c.send(msg)
}

func (c *client) unsubscribe(projectID string) {
	c.t.Helper()
	c.send(map[string]any{"type": MsgUnsubscribe, "projectId": projectID})
}

func (c *client) resize(projectID string, cols, rows int) {
	c.t.Helper()
	c.send(map[string]any{"type": MsgResize, "projectId": projectID, "cols": cols, "rows": rows})
}

func (c *client) input(projectID string, data []byte) {
	c.t.Helper()
	c.send(map[string]any{
		"type":      MsgInput,
		"projectId": projectID,
		"data":      base64.StdEncoding.EncodeToString(data),
	})
}

// requestControl asks for a project's lease without waiting for the answer.
func (c *client) requestControl(projectID string) {
	c.t.Helper()
	c.send(map[string]any{"type": MsgControlRequest, "projectId": projectID})
}

// releaseControl gives up a project's lease.
func (c *client) releaseControl(projectID string) {
	c.t.Helper()
	c.send(map[string]any{"type": MsgControlRelease, "projectId": projectID})
}

// transferControl accepts or declines a pending request from another client.
func (c *client) transferControl(projectID, targetID string, accept bool) {
	c.t.Helper()
	kind := MsgControlAccept
	if !accept {
		kind = MsgControlReject
	}
	c.send(map[string]any{"type": kind, "projectId": projectID, "clientId": targetID})
}

// becomeController claims an unheld project's lease and consumes the two
// messages that answer it: the grant, and the roster that follows it.
//
// A viewer is the default state, so a test that types or resizes has to say it
// wants to be the controller first. That is the whole of this phase, and it is
// why this is a helper a test calls rather than something subscribing does.
func (c *client) becomeController(projectID string) {
	c.t.Helper()
	c.requestControl(projectID)
	granted := c.expectMessageOfType(MsgControlGranted)
	if granted.Reason != ReasonAvailable {
		c.t.Fatalf("the grant gave reason %q, want %q", granted.Reason, ReasonAvailable)
	}
	if granted.Control.Controller == nil || granted.Control.Controller.ClientID != c.conn.clientID {
		c.t.Fatalf("the grant names controller %+v, want this client %q",
			granted.Control.Controller, c.conn.clientID)
	}
	c.expectMessageOfType(MsgControlChanged)
}

// watch starts watching a project and takes its control, which is the state
// most of the tests below are actually about.
func (c *client) watch(projectID string) {
	c.t.Helper()
	c.subscribe(projectID, 0, 0)
	c.becomeController(projectID)
}

// waitForSubscriptions waits until the connection reports a number of
// subscriptions, which is how a test knows a subscribe was acted on before it
// publishes anything.
func (c *client) waitForSubscriptions(n int) {
	c.t.Helper()
	deadline := time.Now().Add(socketWait)
	for {
		if c.conn.subscriptionCount() == n {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("the connection reports %d subscriptions, want %d",
				c.conn.subscriptionCount(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func (c *client) waitForInputs(n int) {
	c.t.Helper()
	deadline := time.Now().Add(socketWait)
	for {
		if len(c.rt.recordedInputs()) >= n {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("the runtime received %d inputs, want %d", len(c.rt.recordedInputs()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Tests: the greeting

func TestTheFirstThingAConnectionSendsIsItsVersion(t *testing.T) {
	c := newClient(t, newFakeRuntime(), clientOptions{})

	if c.hello.Type != MsgHello {
		t.Errorf("first message type = %q, want %q", c.hello.Type, MsgHello)
	}
	if c.hello.Protocol != ProtocolVersion {
		t.Errorf("protocol = %d, want %d", c.hello.Protocol, ProtocolVersion)
	}
	if c.hello.ClientID == "" {
		t.Error("the greeting carried no client id")
	}
	if c.hello.Server == "" || c.hello.Version == "" {
		t.Errorf("the greeting did not name the server: %+v", c.hello)
	}
}

// ---------------------------------------------------------------------------
// Tests: subscribe, snapshot, live output

func TestASubscribeDrawsTheScreenAndThenFollowsIt(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 100, 30)
	// Published before anyone is watching: it is in the terminal, and a client
	// joining now must see it in the screen rather than never.
	rt.publish(testProject, "before ")
	rt.publish(testProject, "anyone")

	c := newClient(t, rt, clientOptions{})
	c.subscribe(testProject, 0, 0)

	snapshot := c.expectFrame()
	if snapshot.Type != FrameSnapshot {
		t.Fatalf("the first frame was type %#x, want a snapshot", snapshot.Type)
	}
	if snapshot.ProjectID != testProject {
		t.Errorf("snapshot project = %q, want %q", snapshot.ProjectID, testProject)
	}
	if snapshot.Cols != 100 || snapshot.Rows != 30 {
		t.Errorf("snapshot geometry = %dx%d, want 100x30", snapshot.Cols, snapshot.Rows)
	}
	if snapshot.FirstSequence != 2 || snapshot.LastSequence != 2 {
		t.Errorf("snapshot boundary = %d..%d, want 2..2", snapshot.FirstSequence, snapshot.LastSequence)
	}
	if !bytes.Contains(snapshot.Payload, []byte("before anyone")) {
		t.Errorf("the screen does not contain what the terminal produced: %q", snapshot.Payload)
	}
	// A rendered screen is self-contained: it says which buffer to be on and
	// where the cursor is, because a client applying it is coming from any
	// state at all.
	if !bytes.HasPrefix(snapshot.Payload, []byte("\x1b[?1049l\x1b[0m\x1b[2J\x1b[H")) {
		t.Errorf("the screen does not reset the terminal before drawing: %q", snapshot.Payload)
	}

	// The boundary is where the client's own bookkeeping starts.
	rt.publish(testProject, "after")
	output := c.expectFrame()
	if output.Type != FrameOutput {
		t.Fatalf("the second frame was type %#x, want output", output.Type)
	}
	if output.ProjectID != testProject {
		t.Errorf("output project = %q, want %q", output.ProjectID, testProject)
	}
	if output.FirstSequence != 3 || output.LastSequence != 3 {
		t.Errorf("output covers %d..%d, want 3..3", output.FirstSequence, output.LastSequence)
	}
	if string(output.Payload) != "after" {
		t.Errorf("output payload = %q, want %q", output.Payload, "after")
	}
	if !output.Accounts(snapshot.LastSequence) {
		t.Error("the output frame does not follow the snapshot it was sent after")
	}
}

func TestTheScreenIsDrawnAtTheSizeTheRuntimeIs(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 132, 43)
	rt.setScreen(testProject, "a wide screen", 132, 43)

	c := newClient(t, rt, clientOptions{})
	c.subscribe(testProject, 0, 0)

	snapshot := c.expectFrame()
	if snapshot.Cols != 132 || snapshot.Rows != 43 {
		t.Errorf("snapshot geometry = %dx%d, want the runtime's 132x43", snapshot.Cols, snapshot.Rows)
	}
	if !bytes.Contains(snapshot.Payload, []byte("a wide screen")) {
		t.Errorf("the screen was not carried: %q", snapshot.Payload)
	}
}

// ---------------------------------------------------------------------------
// Tests: batching

// TestABurstOfOutputIsFramedOnceAndInOrder is §九十二's "output batching".
//
// The capture is held open while the output is produced. That is not a
// contrivance to make the assertion exact: it is what a terminal does the
// moment a program writes a burst, and it also pins the boundary rule. The
// capture was asked for before any of these bytes existed, so the screen does
// not contain them and its boundary says so - each of them is streamed, and
// together they make one frame rather than eight.
func TestABurstOfOutputIsFramedOnceAndInOrder(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})

	rt.holdScreen()
	c.subscribe(testProject, 0, 0)

	select {
	case <-rt.tookScreen():
	case <-time.After(socketWait):
		t.Fatal("the subscription never captured the terminal")
	}

	chunks := []string{"one ", "two ", "three ", "four ", "five ", "six ", "seven ", "eight"}
	rt.publishAll(testProject, chunks...)
	rt.releaseScreen()

	snapshot := c.expectFrame()
	if snapshot.Type != FrameSnapshot {
		t.Fatalf("the first frame was type %#x, want a snapshot", snapshot.Type)
	}
	if snapshot.LastSequence != 0 {
		t.Errorf("snapshot boundary = %d, want 0: nothing had been published when it was asked for",
			snapshot.LastSequence)
	}

	output := c.expectFrame()
	if output.Type != FrameOutput {
		t.Fatalf("the second frame was type %#x, want output", output.Type)
	}
	if output.FirstSequence != 1 || output.LastSequence != uint64(len(chunks)) {
		t.Errorf("the batch covers %d..%d, want 1..%d",
			output.FirstSequence, output.LastSequence, len(chunks))
	}
	if want := strings.Join(chunks, ""); string(output.Payload) != want {
		t.Errorf("the batch carries %q, want %q", output.Payload, want)
	}
	if !output.Accounts(snapshot.LastSequence) {
		t.Error("the batch does not follow the snapshot")
	}

	// Eight chunks, one frame. A frame per chunk would be eight, and the
	// difference is the whole of the batching.
	c.expectSilence(200 * time.Millisecond)
}

// TestABatchThatWouldBeTooLargeIsSplit keeps one burst from becoming a frame
// larger than a browser will take comfortably.
//
// The cap is a watermark rather than a hard limit, so a frame may exceed it by
// the size of the chunk that crossed it. What must not happen is one frame
// carrying the whole burst.
func TestABatchThatWouldBeTooLargeIsSplit(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})

	rt.holdScreen()
	c.subscribe(testProject, 0, 0)
	<-rt.tookScreen()

	const chunkLen = 20 << 10
	const count = 4
	chunk := strings.Repeat("x", chunkLen)
	rt.publishAll(testProject, chunk, chunk, chunk, chunk)
	rt.releaseScreen()

	c.expectFrame() // the snapshot

	total, frames := 0, 0
	next := uint64(1)
	for total < count*chunkLen {
		frame := c.expectFrame()
		if frame.Type != FrameOutput {
			t.Fatalf("frame %d was type %#x, want output", frames, frame.Type)
		}
		if frame.FirstSequence != next {
			t.Errorf("frame %d starts at %d, want %d", frames, frame.FirstSequence, next)
		}
		if len(frame.Payload) > flushBytes+chunkLen {
			t.Errorf("frame %d carries %d bytes, more than one chunk past the %d watermark",
				frames, len(frame.Payload), flushBytes)
		}
		next = frame.LastSequence + 1
		total += len(frame.Payload)
		frames++
		if frames > count {
			t.Fatalf("the burst produced more than %d frames", count)
		}
	}
	if total != count*chunkLen {
		t.Errorf("the frames carried %d bytes, want %d", total, count*chunkLen)
	}
	if frames < 2 {
		t.Errorf("a %d-byte burst arrived in %d frame(s)", count*chunkLen, frames)
	}
	c.expectSilence(200 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// Tests: a gap, and what happens instead

func TestOutputThatSkipsASequenceIsReplacedByAFreshScreen(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})
	c.subscribe(testProject, 0, 0)
	c.expectFrame()

	rt.publish(testProject, "one")
	first := c.expectFrame()
	if first.Type != FrameOutput || first.LastSequence != 1 {
		t.Fatalf("expected output covering 1..1, got type %#x at %d", first.Type, first.LastSequence)
	}

	// The manager dropped a chunk: the terminal published it, the watcher never
	// saw it, and the next chunk the subscription receives has a number that
	// skips. There is no way to ask for what is missing and no safe place to
	// put it, so the only repair is a screen.
	rt.skipOnce(testProject)
	rt.publish(testProject, "two")

	again := c.expectFrame()
	if again.Type != FrameSnapshot {
		t.Fatalf("after a gap the server sent type %#x, want a snapshot", again.Type)
	}
	if again.LastSequence != 3 {
		t.Errorf("the new screen reports boundary %d, want 3", again.LastSequence)
	}
	if !bytes.Contains(again.Payload, []byte("onetwo")) {
		t.Errorf("the new screen does not contain the output that was missed: %q", again.Payload)
	}
	if !again.Accounts(first.LastSequence) {
		t.Error("a snapshot must account for whatever the client already had")
	}
}

// TestAResyncRequestIsAnsweredWithAFreshScreen covers the client's own half of
// the recovery: a browser that knows it lost output asks, and is given a screen
// rather than being told to reconnect.
func TestAResyncRequestIsAnsweredWithAFreshScreen(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})
	c.subscribe(testProject, 0, 0)
	c.expectFrame()

	rt.publish(testProject, "hello")
	c.expectFrame()

	c.send(map[string]any{"type": MsgResync, "projectId": testProject})

	again := c.expectFrame()
	if again.Type != FrameSnapshot {
		t.Fatalf("a resync was answered with type %#x, want a snapshot", again.Type)
	}
	if again.LastSequence != 1 {
		t.Errorf("the new screen reports boundary %d, want 1", again.LastSequence)
	}
	// The subscription was re-established, not duplicated.
	c.waitForSubscriptions(1)
}

// TestResyncingATerminalThatIsNotWatchedSubscribesToIt: the client's intent -
// show me this terminal - is the same either way, so it is answered the same
// way.
func TestResyncingATerminalThatIsNotWatchedSubscribesToIt(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	rt.publish(testProject, "already here")
	c := newClient(t, rt, clientOptions{})

	c.send(map[string]any{"type": MsgResync, "projectId": testProject})

	snapshot := c.expectFrame()
	if snapshot.Type != FrameSnapshot {
		t.Fatalf("got type %#x, want a snapshot", snapshot.Type)
	}
	if !bytes.Contains(snapshot.Payload, []byte("already here")) {
		t.Errorf("the screen does not contain the terminal's contents: %q", snapshot.Payload)
	}
	c.waitForSubscriptions(1)
}

// TestATerminalTheConnectionCannotKeepUpWithIsDroppedRatherThanRetried covers
// the end of the resync loop.
//
// Every chunk skips a number, so no attempt to re-establish the stream can
// succeed. The subscription must stop and say why: a loop against a runtime
// that is outrunning the connection does not end on its own.
func TestATerminalTheConnectionCannotKeepUpWithIsDroppedRatherThanRetried(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	rt.skipEveryChunk(testProject)
	c := newClient(t, rt, clientOptions{})

	c.subscribe(testProject, 0, 0)

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			rt.publish(testProject, "churn")
			time.Sleep(2 * time.Millisecond)
		}
	}()

	msg := c.expectMessageOfType(MsgError)
	if msg.Code != CodeStreamUnstable {
		t.Fatalf("error code = %q, want %q", msg.Code, CodeStreamUnstable)
	}
	if msg.ProjectID != testProject {
		t.Errorf("the error names project %q, want %q", msg.ProjectID, testProject)
	}

	// The client is told the subscription ended rather than being left to
	// wonder why the terminal stopped.
	gone := c.expectMessageOfType(MsgUnsubscribed)
	if gone.ProjectID != testProject {
		t.Errorf("the acknowledgement names %q, want %q", gone.ProjectID, testProject)
	}
	c.waitForSubscriptions(0)
}

// ---------------------------------------------------------------------------
// Tests: input

func TestAKeystrokeReachesTheTerminalItWasTypedInto(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})
	c.subscribe(testProject, 0, 0)
	c.expectFrame()
	c.becomeController(testProject)

	// Not text: a carriage return, an escape sequence, a UTF-8 prompt, and a
	// byte that is not valid UTF-8 at all. All four have to arrive unchanged,
	// because a terminal is a byte stream and a client that re-encoded any of
	// them would type something the program did not ask for.
	typed := []byte("hello\r\x1b[A中文\xff")
	c.input(testProject, typed)

	c.waitForInputs(1)
	got := rt.recordedInputs()[0]
	if got.projectID != testProject {
		t.Errorf("the input went to %q, want %q", got.projectID, testProject)
	}
	if !bytes.Equal(got.data, typed) {
		t.Errorf("the terminal received %q, want %q", got.data, typed)
	}
}

func TestInputToATerminalTheClientIsNotWatchingIsRefused(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})

	c.input(testProject, []byte("rm -rf /\r"))

	msg := c.expectMessageOfType(MsgError)
	if msg.Code != CodeNotSubscribed {
		t.Errorf("error code = %q, want %q", msg.Code, CodeNotSubscribed)
	}
	if msg.About != MsgInput {
		t.Errorf("the error is about %q, want %q", msg.About, MsgInput)
	}
	if inputs := rt.recordedInputs(); len(inputs) != 0 {
		t.Errorf("the runtime received %d inputs, want none", len(inputs))
	}
}

func TestAnOversizeInputIsRefusedRatherThanForwarded(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})
	c.subscribe(testProject, 0, 0)
	c.expectFrame()

	c.input(testProject, bytes.Repeat([]byte("a"), MaxInputBytes+1))

	msg := c.expectMessageOfType(MsgError)
	if msg.Code != CodeInputTooLarge {
		t.Errorf("error code = %q, want %q", msg.Code, CodeInputTooLarge)
	}
	if inputs := rt.recordedInputs(); len(inputs) != 0 {
		t.Errorf("the runtime received %d inputs, want none", len(inputs))
	}
}

// ---------------------------------------------------------------------------
// Tests: resize

func TestAResizeIsSentToTheRuntimeAndReportedToEveryViewer(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	// Two browsers on one hub, because the size belongs to the terminal rather
	// than to either of them: a second viewer drawing its own idea of the size
	// would draw a terminal that does not match the one the program is drawing
	// for.
	hub := newHub(t, rt, clientOptions{})
	one := attach(t, hub, rt, clientOptions{})
	one.greet()
	two := attach(t, hub, rt, clientOptions{})
	two.greet()

	one.subscribe(testProject, 0, 0)
	one.expectFrame()
	two.subscribe(testProject, 0, 0)
	two.expectFrame()
	// `one` is told the roster changed, because how many other people are
	// watching a terminal is part of the roster and `two` has just become the
	// second of them.
	one.expectMessageOfType(MsgControlChanged)

	// The size belongs to the terminal, and the terminal has one owner. `one`
	// asks for it and gets it; `two` only ever watches.
	one.becomeController(testProject)
	// `two` is told that the roster changed, because it is watching a terminal
	// whose owner it had wrong.
	two.expectMessageOfType(MsgControlChanged)

	one.resize(testProject, 120, 40)

	for name, c := range map[string]*client{"the one that asked": one, "the other": two} {
		msg := c.expectMessageOfType(MsgResized)
		if msg.ProjectID != testProject || msg.Cols != 120 || msg.Rows != 40 {
			t.Errorf("%s was told %+v, want %s at 120x40", name, msg, testProject)
		}
	}

	resizes := rt.recordedResizes()
	if len(resizes) != 1 {
		t.Fatalf("the runtime was resized %d times, want 1: %+v", len(resizes), resizes)
	}
	if resizes[0].cols != 120 || resizes[0].rows != 40 {
		t.Errorf("the runtime was resized to %dx%d, want 120x40", resizes[0].cols, resizes[0].rows)
	}
}

// TestAResizeThatChangesNothingIsNotSentToTheRuntime is the deduplication a
// browser needs: a terminal that re-measures on every layout pass must not
// produce a pty resize per measurement, because each one makes the program
// inside redraw.
func TestAResizeThatChangesNothingIsNotSentToTheRuntime(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})
	c.subscribe(testProject, 0, 0)
	c.expectFrame()
	c.becomeController(testProject)

	c.resize(testProject, 120, 40)
	c.expectMessageOfType(MsgResized)

	// The same size again, twice. Neither reaches the runtime and neither is
	// broadcast.
	c.resize(testProject, 120, 40)
	c.resize(testProject, 120, 40)
	c.expectSilence(150 * time.Millisecond)

	if resizes := rt.recordedResizes(); len(resizes) != 1 {
		t.Errorf("the runtime was resized %d times, want 1: %+v", len(resizes), resizes)
	}
}

// TestViewersAreToldTheSizeTheRuntimeApplied: the request is a request. A
// backend may clamp it, and a client drawing the size it asked for would draw a
// terminal that does not match the one the program is drawing for.
func TestViewersAreToldTheSizeTheRuntimeApplied(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	rt.answersResizeWith(90, 25)
	c := newClient(t, rt, clientOptions{})
	c.subscribe(testProject, 0, 0)
	c.expectFrame()
	c.becomeController(testProject)

	c.resize(testProject, 120, 40)

	msg := c.expectMessageOfType(MsgResized)
	if msg.Cols != 90 || msg.Rows != 25 {
		t.Errorf("the client was told %dx%d, want the runtime's 90x25", msg.Cols, msg.Rows)
	}
	// The hub now holds the applied size, so a request for it is not forwarded.
	c.resize(testProject, 90, 25)
	c.expectSilence(150 * time.Millisecond)
	if resizes := rt.recordedResizes(); len(resizes) != 1 {
		t.Errorf("the runtime was resized %d times, want 1", len(resizes))
	}
}

// TestASubscribeMayCarryTheTerminalSize covers the reason the size travels with
// the subscribe: the runtime is resized once, before the screen is taken, so
// the client never draws a screen at the old size and then reflows it.
func TestASubscribeMayCarryTheTerminalSize(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})
	c.subscribe(testProject, 0, 0)
	c.expectFrame()
	c.becomeController(testProject)

	// Wider than any display: it is clamped, and the client is told what it got
	// rather than what it asked for.
	c.sendSubscribe(testProject, 600, 24)

	resized := c.expectMessageOfType(MsgResized)
	if resized.Cols != MaxCols || resized.Rows != 24 {
		t.Errorf("the size applied was %dx%d, want %dx24", resized.Cols, resized.Rows, MaxCols)
	}
	c.expectMessageOfType(MsgControlChanged)

	snapshot := c.expectFrame()
	if snapshot.Cols != MaxCols || snapshot.Rows != 24 {
		t.Errorf("the screen is %dx%d, want %dx24", snapshot.Cols, snapshot.Rows, MaxCols)
	}
}

// TestAViewersSubscribeDoesNotResizeTheTerminal is the other half of the rule
// the test above covers.
//
// A phone opening a project a desktop is working in sends its own viewport with
// its subscribe. If that shaped the pty, the desktop's terminal would reflow to
// forty columns because somebody looked at it - so the size is a request, the
// request needs the lease, and a viewer does not have it.
func TestAViewersSubscribeDoesNotResizeTheTerminal(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})

	c.sendSubscribe(testProject, 600, 24)
	c.expectMessageOfType(MsgControlChanged)

	snapshot := c.expectFrame()
	if snapshot.Cols != 80 || snapshot.Rows != 24 {
		t.Errorf("the screen is %dx%d, want the runtime's 80x24", snapshot.Cols, snapshot.Rows)
	}
	if resizes := rt.recordedResizes(); len(resizes) != 0 {
		t.Errorf("the runtime was resized %d times by a viewer, want none", len(resizes))
	}
}

func TestAResizeOfATerminalTheClientIsNotWatchingIsRefused(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})

	c.resize(testProject, 120, 40)

	msg := c.expectMessageOfType(MsgError)
	if msg.Code != CodeNotSubscribed {
		t.Errorf("error code = %q, want %q", msg.Code, CodeNotSubscribed)
	}
	if resizes := rt.recordedResizes(); len(resizes) != 0 {
		t.Errorf("the runtime was resized %d times, want none", len(resizes))
	}
}

// ---------------------------------------------------------------------------
// Tests: unsubscribe

func TestUnsubscribingStopsTheOutputAndTheTyping(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})
	c.subscribe(testProject, 0, 0)
	c.expectFrame()

	c.unsubscribe(testProject)
	c.expectMessageOfType(MsgUnsubscribed)
	c.waitForSubscriptions(0)

	rt.publish(testProject, "after the client left")
	c.expectSilence(200 * time.Millisecond)

	// A terminal this connection no longer watches is a terminal it may no
	// longer type into.
	c.input(testProject, []byte("x"))
	msg := c.expectMessageOfType(MsgError)
	if msg.Code != CodeNotSubscribed {
		t.Errorf("error code = %q, want %q", msg.Code, CodeNotSubscribed)
	}
}

func TestUnsubscribingFromATerminalThatIsNotWatchedIsAcknowledgedAnyway(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})

	c.unsubscribe(testProject)

	msg := c.expectMessageOfType(MsgUnsubscribed)
	if msg.ProjectID != testProject {
		t.Errorf("the acknowledgement names %q, want %q", msg.ProjectID, testProject)
	}
}

// ---------------------------------------------------------------------------
// Tests: a terminal that goes away

func TestATerminalThatIsGoneIsReportedRatherThanSilentlyStopped(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})
	c.subscribe(testProject, 0, 0)
	c.expectFrame()

	rt.retire(testProject)

	msg := c.expectMessageOfType(MsgError)
	if msg.Code != session.CodeNotRunning {
		t.Errorf("error code = %q, want %q", msg.Code, session.CodeNotRunning)
	}
	c.expectMessageOfType(MsgUnsubscribed)
	c.waitForSubscriptions(0)
}

func TestASubscribeToAnUnregisteredProjectIsRefused(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})

	c.sendSubscribe("p_ffffffffffffffffffff", 0, 0)

	msg := c.expectMessageOfType(MsgError)
	if msg.Code != session.CodeProjectNotFound {
		t.Errorf("error code = %q, want %q", msg.Code, session.CodeProjectNotFound)
	}
	c.waitForSubscriptions(0)
}

func TestASubscribeToAStoppedRuntimeIsRefusedRatherThanStartingIt(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	rt.refuses(testProject, &session.Error{
		Code:    session.CodeNotRunning,
		Message: "no terminal runtime is running for " + testProject,
	})
	c := newClient(t, rt, clientOptions{})

	c.sendSubscribe(testProject, 0, 0)

	msg := c.expectMessageOfType(MsgError)
	if msg.Code != session.CodeNotRunning {
		t.Errorf("error code = %q, want %q", msg.Code, session.CodeNotRunning)
	}
	if msg.Message == "" {
		t.Error("the refusal carried no explanation")
	}
	c.waitForSubscriptions(0)
}

// ---------------------------------------------------------------------------
// Tests: the protocol's limits

func TestOneBrowserMayOnlyWatchSoManyTerminals(t *testing.T) {
	rt := newFakeRuntime()
	ids := make([]string, 0, MaxSubscriptions+1)
	for i := 0; i <= MaxSubscriptions; i++ {
		id := fmt.Sprintf("p_%020x", i)
		ids = append(ids, id)
		rt.add(id, 80, 24)
	}

	c := newClient(t, rt, clientOptions{})
	for _, id := range ids[:MaxSubscriptions] {
		c.subscribe(id, 0, 0)
	}
	c.waitForSubscriptions(MaxSubscriptions)

	c.sendSubscribe(ids[MaxSubscriptions], 0, 0)
	msg := c.expectMessageOfType(MsgError)
	if msg.Code != CodeTooManySubscriptions {
		t.Errorf("error code = %q, want %q", msg.Code, CodeTooManySubscriptions)
	}
	c.waitForSubscriptions(MaxSubscriptions)
}

// TestAMessageThatIsNotProtocolIsRefusedWithACode is the strictness half of
// §五十三 and §五十四: every one of these is a client that is not speaking the
// protocol, and each is told which part it got wrong.
func TestAMessageThatIsNotProtocolIsRefusedWithACode(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		code string
	}{
		{"not JSON at all", `not json`, CodeBadMessage},
		{"no type", `{}`, CodeBadMessage},
		{"an unknown type", `{"type":"runCommand"}`, CodeBadMessage},
		{"an array", `[]`, CodeBadMessage},
		{"a bare string", `"subscribe"`, CodeBadMessage},
		{"two messages in one", `{"type":"ping"}{"type":"ping"}`, CodeBadMessage},
		{"a field the message does not have", `{"type":"unsubscribe","projectId":"` + testProject + `","cols":80}`, CodeBadMessage},
		{"a ping carrying a payload", `{"type":"ping","projectId":"` + testProject + `"}`, CodeBadMessage},
		{"a subscribe carrying data", `{"type":"subscribe","projectId":"` + testProject + `","data":"aGk="}`, CodeBadMessage},
		{"a resync carrying a size", `{"type":"resync","projectId":"` + testProject + `","cols":80,"rows":24}`, CodeBadMessage},
		{"a path where a project id goes", `{"type":"subscribe","projectId":"/etc/passwd"}`, CodeBadProject},
		{"a project id with a separator", `{"type":"subscribe","projectId":"p_a/b"}`, CodeBadProject},
		{"an enormous project id", `{"type":"subscribe","projectId":"p_` + strings.Repeat("a", 200) + `"}`, CodeBadProject},
		{"a negative size", `{"type":"subscribe","projectId":"` + testProject + `","cols":-5,"rows":24}`, session.CodeInvalidSize},
		{"half a size", `{"type":"subscribe","projectId":"` + testProject + `","cols":80}`, session.CodeInvalidSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFakeRuntime().add(testProject, 80, 24)
			c := newClient(t, rt, clientOptions{})

			c.sendRaw(tc.raw)

			msg := c.expectMessageOfType(MsgError)
			if msg.Code != tc.code {
				t.Errorf("error code = %q, want %q (%s)", msg.Code, tc.code, msg.Message)
			}

			// A refused message does not end the connection: the client is
			// still speaking to a server that will answer it.
			c.sendRaw(`{"type":"ping"}`)
			c.expectMessageOfType(MsgPong)
		})
	}
}

func TestAPingIsAnsweredWithAPong(t *testing.T) {
	c := newClient(t, newFakeRuntime(), clientOptions{})

	c.sendRaw(`{"type":"ping"}`)

	if msg := c.expectMessageOfType(MsgPong); msg.Type != MsgPong {
		t.Errorf("got %q, want %q", msg.Type, MsgPong)
	}
}

// TestABinaryMessageFromABrowserEndsTheConnection: no client message is binary.
// Continuing would mean guessing what the client meant, and the explanation is
// put on the wire before the socket is torn down.
func TestABinaryMessageFromABrowserEndsTheConnection(t *testing.T) {
	c := newClient(t, newFakeRuntime(), clientOptions{})

	c.ws.sendBinary([]byte{0x01, 0x02, 0x03})

	msg := c.expectMessageOfType(MsgError)
	if msg.Code != CodeUnsupported {
		t.Errorf("error code = %q, want %q", msg.Code, CodeUnsupported)
	}

	ctl := c.ws.awaitControl(t, websocket.CloseMessage)
	if code, _ := closeCodeOf(ctl.data); code != websocket.CloseUnsupportedData {
		t.Errorf("close code = %d, want %d", code, websocket.CloseUnsupportedData)
	}

	select {
	case <-c.served:
	case <-time.After(socketWait):
		t.Fatal("the connection stayed open after a binary client message")
	}
}

// closeCodeOf splits a close frame into its code and its text.
func closeCodeOf(data []byte) (int, string) {
	if len(data) < 2 {
		return 0, ""
	}
	return int(binary.BigEndian.Uint16(data[:2])), string(data[2:])
}

// ---------------------------------------------------------------------------
// Tests: reading limits

func TestTheTransportSetsAReadLimitOnEveryConnection(t *testing.T) {
	c := newClient(t, newFakeRuntime(), clientOptions{})

	c.ws.mu.Lock()
	limit := c.ws.readLimit
	c.ws.mu.Unlock()

	if limit != MaxClientMessageBytes {
		t.Errorf("read limit = %d, want %d", limit, MaxClientMessageBytes)
	}
}

// ---------------------------------------------------------------------------
// Tests: a client that stops reading

// TestAFrameForAClientThatIsBehindReSynchronisesRatherThanEnding tests the
// decision at the queue, in isolation.
//
// The connection is built and deliberately not started, so nothing drains its
// write queue: that is precisely the state a browser which has stopped reading
// puts it in. What is being asserted is which of three things the subscription
// concludes, because two of them look identical from the outside - a frame that
// did not arrive - and choosing wrong is silent.
func TestAFrameForAClientThatIsBehindReSynchronisesRatherThanEnding(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	timings := fastTimings()
	timings.Queue = 20 * time.Millisecond
	hub, err := NewHub(rt, HubOptions{Logger: discardLogger(), Timings: timings})
	if err != nil {
		t.Fatalf("terminal.NewHub returned an error: %v", err)
	}

	stalled := newConn(hub, newPipeSocket(1), ConnInfo{}, 1)
	for i := 0; i < outboundQueueDepth; i++ {
		stalled.out <- outbound{kind: websocket.TextMessage, data: []byte("x")}
	}
	sub := newSubscription(stalled, testProject)
	if got := sub.flush(&batch{pending: []byte("hello"), first: 1, last: 1}); got != streamGap {
		t.Errorf("a frame for a client that is behind was handled as %d, want a gap", got)
	}
	if got := sub.flush(&batch{pending: []byte("again"), first: 2, last: 2}); got != streamGap {
		t.Errorf("the second frame was handled as %d, want a gap", got)
	}
	// Congestion is not disconnection: the connection is still open, and this
	// is the difference the three outcomes exist to keep.
	select {
	case <-stalled.ctx.Done():
		t.Error("a congested connection was treated as a closed one")
	default:
	}

	// With room in the queue the same frame is delivered.
	roomy := newConn(hub, newPipeSocket(4), ConnInfo{}, 2)
	open := newSubscription(roomy, testProject)
	if got := open.flush(&batch{pending: []byte("hello"), first: 1, last: 1}); got != streamContinues {
		t.Errorf("a frame for a client that is keeping up was handled as %d, want delivery", got)
	}

	// A connection that has ended says so, rather than reporting congestion it
	// could never recover from.
	//
	// The queue is filled first so that both conditions are true and the answer
	// is not left to which case a select picks. What decides it is that a
	// cancelled connection is ready at once while the queue's timer is not: a
	// dead connection is recognised immediately rather than after a wait that
	// could only ever end in the same place.
	dead := newConn(hub, newPipeSocket(4), ConnInfo{}, 3)
	full := newSubscription(dead, testProject)
	for i := 0; i < outboundQueueDepth; i++ {
		dead.out <- outbound{kind: websocket.TextMessage, data: []byte("x")}
	}
	dead.close()
	if got := full.flush(&batch{pending: []byte("hello"), first: 1, last: 1}); got != streamEnded {
		t.Errorf("a frame for a closed connection was handled as %d, want the end", got)
	}
}

// TestAClientThatStopsReadingIsReapedRatherThanHeldOpen is the other half: the
// memory the connection holds is bounded, and the connection does not outlive
// its usefulness.
//
// The greeting is deliberately never read. One slot of the socket's outgoing
// queue is all there is, so the greeting fills it and the screen that follows
// cannot be written at all - which is a browser that has stopped reading, with
// nothing draining the queue behind it.
func TestAClientThatStopsReadingIsReapedRatherThanHeldOpen(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	timings := fastTimings()
	timings.Write = 100 * time.Millisecond

	hub := newHub(t, rt, clientOptions{timings: timings})
	ws := newPipeSocket(1)
	t.Cleanup(func() { _ = ws.Close() })

	served := make(chan struct{})
	go func() {
		defer close(served)
		hub.Serve(ws, ConnInfo{})
	}()
	hub.awaitConn(t, "")

	ws.sendText(`{"type":"subscribe","projectId":"` + testProject + `"}`)

	select {
	case <-served:
	case <-time.After(socketWait):
		t.Fatal("a client that cannot accept a frame was held open")
	}

	if stats := hub.Stats(); stats.Connections != 0 {
		t.Errorf("the hub still holds %d connections", stats.Connections)
	}
}

// ---------------------------------------------------------------------------
// Tests: liveness

func TestAClientThatStopsAnsweringPingsIsReaped(t *testing.T) {
	timings := fastTimings()
	timings.Ping = 20 * time.Millisecond
	timings.Pong = 80 * time.Millisecond

	c := newClient(t, newFakeRuntime(), clientOptions{timings: timings})

	select {
	case <-c.served:
	case <-time.After(socketWait):
		t.Fatal("a client that stopped answering was held open")
	}
	if c.ws.controlCount(websocket.PingMessage) == 0 {
		t.Error("the connection was ended without ever pinging")
	}
}

func TestAClientThatAnswersPingsStaysConnected(t *testing.T) {
	timings := fastTimings()
	timings.Ping = 20 * time.Millisecond
	timings.Pong = 300 * time.Millisecond

	c := newClient(t, newFakeRuntime(), clientOptions{timings: timings})

	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				c.ws.sendPong()
			}
		}
	}()

	time.Sleep(500 * time.Millisecond)
	close(stop)

	select {
	case <-c.served:
		t.Fatal("a client that answered every ping was disconnected")
	default:
	}
	if n := c.ws.controlCount(websocket.PingMessage); n < 2 {
		t.Errorf("the connection sent %d pings in half a second, want several", n)
	}
}

// ---------------------------------------------------------------------------
// Tests: shutdown

func TestShuttingDownEndsEveryConnectionWithAGoingAway(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	hub := newHub(t, rt, clientOptions{})
	one := attach(t, hub, rt, clientOptions{})
	one.greet()
	two := attach(t, hub, rt, clientOptions{})
	two.greet()

	one.subscribe(testProject, 0, 0)
	one.expectFrame()

	if err := hub.Close(); err != nil {
		t.Fatalf("closing the hub failed: %v", err)
	}

	// CloseGoingAway is the code a client is expected to reconnect after.
	// CloseNormalClosure would tell a browser its session was over.
	for name, c := range map[string]*client{"the first": one, "the second": two} {
		ctl := c.ws.awaitControl(t, websocket.CloseMessage)
		code, text := closeCodeOf(ctl.data)
		if code != websocket.CloseGoingAway {
			t.Errorf("%s connection got close code %d (%q), want %d",
				name, code, text, websocket.CloseGoingAway)
		}
		select {
		case <-c.served:
		case <-time.After(socketWait):
			t.Errorf("%s connection did not end", name)
		}
	}
}

func TestClosingTheHubTwiceIsHarmless(t *testing.T) {
	hub, err := NewHub(newFakeRuntime(), HubOptions{Logger: discardLogger()})
	if err != nil {
		t.Fatalf("terminal.NewHub returned an error: %v", err)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("the first close failed: %v", err)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("the second close failed: %v", err)
	}
}

func TestAHubNeedsARuntime(t *testing.T) {
	if _, err := NewHub(nil, HubOptions{}); err == nil {
		t.Error("NewHub accepted a nil runtime")
	}
}

// TestAConnectionRefusedDuringShutdownIsClosedRatherThanLeftHanging: a socket
// the hub will not take has to be closed, because the browser on the other end
// is waiting for a handshake that has already happened.
func TestAConnectionRefusedDuringShutdownIsClosedRatherThanLeftHanging(t *testing.T) {
	hub, err := NewHub(newFakeRuntime(), HubOptions{Logger: discardLogger()})
	if err != nil {
		t.Fatalf("terminal.NewHub returned an error: %v", err)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("closing the hub failed: %v", err)
	}

	ws := newPipeSocket(4)
	hub.Serve(ws, ConnInfo{})

	select {
	case <-ws.closedCh:
	case <-time.After(socketWait):
		t.Fatal("the hub left a socket it refused open")
	}
	if hub.Stats().Connections != 0 {
		t.Error("the hub registered a connection it refused")
	}
}

// ---------------------------------------------------------------------------
// Tests: the hub's bookkeeping

func TestTheHubCountsWhatItIsCarrying(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24).add("p_ffffffffffffffffffff", 80, 24)
	c := newClient(t, rt, clientOptions{})

	if stats := c.hub.Stats(); stats.Connections != 1 || stats.Subscriptions != 0 {
		t.Errorf("hub stats = %+v, want one connection and no subscriptions", stats)
	}

	c.subscribe(testProject, 0, 0)
	c.waitForSubscriptions(1)
	c.sendSubscribe("p_ffffffffffffffffffff", 0, 0)
	c.waitForSubscriptions(2)

	if stats := c.hub.Stats(); stats.Connections != 1 || stats.Subscriptions != 2 {
		t.Errorf("hub stats = %+v, want one connection and two subscriptions", stats)
	}
}

// ---------------------------------------------------------------------------
// Tests: what the transport refuses to do

// TestTheTransportReachesNoTerminalOfItsOwn is §十一 as a property rather than
// a promise.
//
// The whole of this package's access to a terminal is the Runtime interface,
// and the double here counts every call. If anything in the transport grew a
// tmux socket, a session name, or a command to run, that path would bypass this
// double entirely - and the number of projects this connection could reach
// would stop being the number the runtime knows about.
func TestTheTransportReachesNoTerminalOfItsOwn(t *testing.T) {
	rt := newFakeRuntime().add(testProject, 80, 24)
	c := newClient(t, rt, clientOptions{})

	// A client asks for a project the runtime does not know, using a name that
	// is well-formed and looks like a real one.
	c.sendSubscribe("p_00000000000000000000", 0, 0)
	if msg := c.expectMessageOfType(MsgError); msg.Code != session.CodeProjectNotFound {
		t.Errorf("error code = %q, want %q", msg.Code, session.CodeProjectNotFound)
	}

	// A client asks with a filesystem path.
	c.sendSubscribe("/home/sunboy/projects", 0, 0)
	if msg := c.expectMessageOfType(MsgError); msg.Code != CodeBadProject {
		t.Errorf("error code = %q, want %q", msg.Code, CodeBadProject)
	}

	// Neither created anything, and the transport never touched a terminal that
	// the runtime was not already serving.
	c.waitForSubscriptions(0)
}
