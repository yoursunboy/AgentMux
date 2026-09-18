package terminal

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/kutonlagos/agentmux/internal/session"
)

// Runtime is what the terminal layer needs from the runtime manager.
//
// It is an interface rather than the manager itself so that the transport can
// be tested against a runtime that produces exactly the bytes a test wants,
// including the ones a real tmux will not produce on demand: a gap, a runtime
// that disappears mid-stream, a screen that changes between two captures.
//
// It is deliberately the *only* way this package reaches a terminal. There is
// no tmux socket here, no session name, no command to run - see the package
// comment for why that matters.
type Runtime interface {
	// Screen returns a project's terminal as it is now, with the output
	// sequence the screen already contains.
	Screen(ctx context.Context, projectID string) (session.Screen, uint64, error)

	// Watch registers for a project's output. The returned chunks are the
	// buffered backlog; the channel carries everything published afterwards.
	Watch(ctx context.Context, projectID string, since uint64) ([]session.Chunk, <-chan session.Chunk, func(), error)

	// Input writes raw terminal bytes to a project's running terminal.
	Input(ctx context.Context, projectID string, data []byte) error

	// Resize sets a project's canonical terminal size and returns the runtime
	// as it stands afterwards.
	Resize(ctx context.Context, projectID string, cols, rows int) (*session.Runtime, error)

	// Runtime reports a project's runtime, which is how a project identifier
	// from a browser is resolved against the projects this server knows. It is
	// what makes "subscribe to project X" mean something the server decided
	// rather than something the client asserted.
	Runtime(ctx context.Context, projectID string) (*session.Runtime, error)
}

// maxResyncs is how many times one subscription may re-establish itself.
//
// Past it the subscription is dropped rather than retried, because the cause is
// not transient: either the connection cannot carry this project's output at
// all, or the runtime is producing it faster than any snapshot can be taken. A
// retry loop against either is a loop that does not end.
const maxResyncs = 8

// Timings are the transport's timeouts, in one place.
//
// They are a struct rather than a set of constants because the only way to test
// what happens when a client stops reading, or when a stream cannot be kept up
// with, is to make the seconds short. A test that had to wait ten seconds to
// watch a frame be dropped would be a test people stop running, and a property
// that is only asserted by reading the code is a property that is only believed
// by the person who wrote it.
//
// The zero value is not usable: NewHub fills every unset field from
// defaultTimings, so a caller that cares about one timeout can set that one.
type Timings struct {
	// Write bounds one write to one browser. A client that cannot accept a
	// frame within this is not slow, it is gone, and holding its queue open any
	// longer only delays the moment the connection is reaped.
	Write time.Duration

	// Queue bounds how long a subscription waits to hand a frame to its
	// connection's writer. Past it the client is too far behind for the frame
	// to be worth delivering, and the subscription re-synchronises instead.
	Queue time.Duration

	// Ping and Pong are the liveness pair. The server pings; a browser that does
	// not answer within Pong is treated as gone. Without this, a laptop that
	// closed its lid leaves a connection, a goroutine per subscription and a
	// manager watcher alive until TCP eventually gives up, which on a quiet link
	// it may never do.
	Ping time.Duration
	Pong time.Duration

	// CloseGrace bounds the wait for a queued close frame to reach a client that
	// has just broken the protocol. It exists so that the message explaining the
	// failure is written before the socket is torn down.
	CloseGrace time.Duration

	// ResyncPause separates re-synchronisation attempts. A runtime producing
	// output faster than a connection can carry it would otherwise be captured,
	// drawn and captured again in a tight loop, and each capture is a tmux round
	// trip.
	ResyncPause time.Duration

	// Input bounds one delivery of raw input to a terminal.
	Input time.Duration

	// Lookup bounds resolving a project identifier against the runtime. It is
	// short because it happens on the read loop, and a client waiting for a
	// keystroke should not be waiting for this.
	Lookup time.Duration

	// Shutdown bounds how long Close waits for the connections it just ended to
	// finish saying so.
	Shutdown time.Duration
}

// defaultTimings is what the transport uses when nothing overrides it.
//
// The values are the ones this file used to hold as loose constants; keeping
// them in one function is what makes it possible to see the transport's whole
// sense of time at once, which is the thing that is hardest to reconstruct from
// scattered call sites.
func defaultTimings() Timings {
	return Timings{
		Write:       15 * time.Second,
		Queue:       10 * time.Second,
		Ping:        30 * time.Second,
		Pong:        75 * time.Second,
		CloseGrace:  2 * time.Second,
		ResyncPause: 500 * time.Millisecond,
		Input:       10 * time.Second,
		Lookup:      10 * time.Second,
		Shutdown:    2 * time.Second,
	}
}

// withDefaults returns t with every unset field filled in.
func (t Timings) withDefaults() Timings {
	d := defaultTimings()
	if t.Write == 0 {
		t.Write = d.Write
	}
	if t.Queue == 0 {
		t.Queue = d.Queue
	}
	if t.Ping == 0 {
		t.Ping = d.Ping
	}
	if t.Pong == 0 {
		t.Pong = d.Pong
	}
	if t.CloseGrace == 0 {
		t.CloseGrace = d.CloseGrace
	}
	if t.ResyncPause == 0 {
		t.ResyncPause = d.ResyncPause
	}
	if t.Input == 0 {
		t.Input = d.Input
	}
	if t.Lookup == 0 {
		t.Lookup = d.Lookup
	}
	if t.Shutdown == 0 {
		t.Shutdown = d.Shutdown
	}
	return t
}

// HubOptions configures a Hub.
type HubOptions struct {
	// Logger receives connection and subscription records. Nil means
	// slog.Default.
	//
	// What may be logged is a short list and it is in docs/TERMINAL.md: client
	// id, project id, subscribe, unsubscribe, byte and frame counts, disconnect
	// and resync. Terminal bytes are never logged, in either direction, at any
	// level.
	Logger *slog.Logger

	// Now supplies the current time. Nil means time.Now.
	Now func() time.Time

	// Timings overrides the transport's timeouts. Nil, or a zero field, means
	// the default; see Timings.
	Timings *Timings
}

// Hub owns every browser connection this server has open.
//
// It exists for two reasons that are easy to miss from the outside. The first
// is shutdown: a server stopping has to end its sockets deliberately rather
// than by exiting, so that clients see a close they can reconnect to instead
// of a connection that hangs until TCP notices. The second is that a terminal
// size is a property of the runtime and not of any one browser, so something
// has to hold the current value and tell the other viewers when it changes -
// and that something must not be a connection, because a connection can leave.
type Hub struct {
	rt  Runtime
	log *slog.Logger
	now func() time.Time

	// t is the transport's timeouts, resolved once at construction. It is read
	// by every connection and never written after NewHub, so it needs no lock.
	t Timings

	// mu guards conns, closed and nextID. Where it is taken together with a
	// connection's own lock it is always taken first; see Conn.mu.
	mu     sync.Mutex
	conns  map[*Conn]struct{}
	closed bool
	nextID uint64

	// sizeMu guards sizes. It is a separate lock from mu so that recording a
	// terminal size never has to wait for the connection set, which is held
	// across a broadcast.
	sizeMu sync.Mutex
	sizes  map[string]size
}

// size is a terminal's canonical geometry.
type size struct {
	cols int
	rows int
}

// NewHub builds a Hub over a runtime.
func NewHub(rt Runtime, opts HubOptions) (*Hub, error) {
	if rt == nil {
		return nil, errors.New("terminal: a Runtime is required")
	}
	h := &Hub{
		rt:    rt,
		log:   opts.Logger,
		now:   opts.Now,
		conns: make(map[*Conn]struct{}),
		sizes: make(map[string]size),
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.now == nil {
		h.now = time.Now
	}
	h.t = defaultTimings()
	if opts.Timings != nil {
		h.t = opts.Timings.withDefaults()
	}
	return h, nil
}

// Stats describes the hub's current load.
//
// It exists for tests and for a future diagnostics endpoint. It reports
// without terminal content by construction: there is nowhere in this struct
// for a byte of a terminal to be.
type Stats struct {
	Connections   int
	Subscriptions int
}

// Stats reports the connection and subscription counts.
func (h *Hub) Stats() Stats {
	h.mu.Lock()
	conns := make([]*Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	stats := Stats{Connections: len(conns)}
	for _, c := range conns {
		c.mu.Lock()
		stats.Subscriptions += len(c.subs)
		c.mu.Unlock()
	}
	return stats
}

// Serve runs one browser connection until it ends.
//
// It blocks. The caller is an HTTP handler that has already upgraded the
// request, and it returns when the socket closes, when the client breaks the
// protocol, or when the hub shuts down.
func (h *Hub) Serve(ws Socket, remote string) {
	c, ok := h.addConn(ws, remote)
	if !ok {
		// The hub is shutting down. The socket is closed rather than left to
		// hang, so the browser's reconnect logic starts from a known state.
		_ = ws.Close()
		return
	}
	defer h.removeConn(c)

	c.run()
}

// addConn registers a connection, or reports that the hub will not take one.
func (h *Hub) addConn(ws Socket, remote string) (*Conn, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, false
	}
	h.nextID++
	c := newConn(h, ws, remote, h.nextID)
	h.conns[c] = struct{}{}
	h.log.Info("terminal client connected",
		"clientId", c.id, "remote", remote, "connections", len(h.conns))
	return c, true
}

// removeConn deregisters a connection.
func (h *Hub) removeConn(c *Conn) {
	h.mu.Lock()
	delete(h.conns, c)
	remaining := len(h.conns)
	h.mu.Unlock()

	c.log.Info("terminal client disconnected",
		"clientId", c.id,
		"connections", remaining,
		"framesIn", c.framesIn.Load(),
		"bytesIn", c.bytesIn.Load(),
		"framesOut", c.framesOut.Load(),
		"bytesOut", c.bytesOut.Load(),
	)
}

// Close ends every connection.
//
// It is called when the server is shutting down. Each browser sees an ordinary
// close, which is the signal its reconnect logic is written for; killing the
// process instead would leave the same browsers waiting on a dead socket until
// TCP noticed.
//
// It waits for the close frames to reach their clients, because returning
// immediately would let the process exit first and turn a deliberate shutdown
// into the dropped connection this exists to avoid. The wait is bounded: a
// browser that has stopped reading must not be able to hold a shutdown open.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	conns := make([]*Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	deadline := h.now().Add(h.t.Shutdown)
	for _, c := range conns {
		c.shutdown()
	}
	for _, c := range conns {
		select {
		case <-c.done:
		case <-time.After(time.Until(deadline)):
		}
	}
	return nil
}

// canonicalSize reports the size the hub believes a project's terminal has.
func (h *Hub) canonicalSize(projectID string) (size, bool) {
	h.sizeMu.Lock()
	defer h.sizeMu.Unlock()
	s, ok := h.sizes[projectID]
	return s, ok
}

// rememberSize records a project's terminal size.
func (h *Hub) rememberSize(projectID string, cols, rows int) {
	h.sizeMu.Lock()
	defer h.sizeMu.Unlock()
	h.sizes[projectID] = size{cols: cols, rows: rows}
}

// broadcastResize tells every viewer of a project that its terminal changed
// size, and returns the message it sent.
//
// It reaches the connections that watch the project, which is every browser
// drawing that terminal. The message is returned rather than only sent because
// there is one caller for which "watches the project" is the wrong test: a
// subscribe that carries a size applies it before the subscription exists, so
// that the snapshot is drawn at the new size rather than at the old one. That
// caller is not a viewer yet and has to be told directly.
func (h *Hub) broadcastResize(projectID string, cols, rows int) []byte {
	h.rememberSize(projectID, cols, rows)

	message := encodeJSON(resizedMessage{
		Type: MsgResized, ProjectID: projectID, Cols: cols, Rows: rows,
	})

	h.mu.Lock()
	subscribers := make([]*Conn, 0, len(h.conns))
	for c := range h.conns {
		if c.watches(projectID) {
			subscribers = append(subscribers, c)
		}
	}
	h.mu.Unlock()

	for _, c := range subscribers {
		c.enqueueText(message)
	}
	return message
}

// findConn returns a connection by id, for tests.
func (h *Hub) findConn(id string) *Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.conns {
		if c.id == id {
			return c
		}
	}
	return nil
}
