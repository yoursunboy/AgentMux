package claude

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kutonlagos/agentmux/internal/idgen"
)

// This file is the adapter itself: the lifecycle, the receiver's address, and
// the one path every observation takes.
//
// # What an adapter is, and what it is not
//
// It is an observation layer. Claude hands it facts - a hook delivery, a line
// on a stream - and it turns each one into an AgentMux event and offers it to
// the event service. It starts nothing, stops nothing, and answers nothing.
//
// It is not a supervisor and it does not own the Claude process. Phase 7.3B is
// under an explicit prohibition on starting a Claude task, and the prohibition
// is also the right design: the process is started by whoever owns the runtime,
// inside a tmux session that outlives this server, and an adapter that spawned
// its own would be a second thing owning a session that already has an owner.
// An adapter is started with the identity of something already running.
//
// # Two adapters, one process
//
// Everything an adapter owns is per-instance: its listener, its hook path, its
// bindings and its subscribers. Two adapters - two projects, or one project and
// a test - do not share an address, a path or a channel, so a hook delivered to
// one is never attributed to the other.

// hookPathSpec is the shape of a receiver path.
//
// The path is a nonce rather than a fixed string so that the endpoint is not
// guessable by a process that never saw the settings file this adapter
// generated. It is the whole of the receiver's authentication and
// docs/CLAUDE_ADAPTER.md §8 is explicit about how much that is worth.
var hookPathSpec = idgen.Spec{Prefix: "hook_", Bytes: 16}

// subscriberBuffer is how many observations a subscriber may fall behind by.
//
// A subscriber that falls further behind has stopped reading, and the
// observation is dropped rather than the adapter blocked. Dropping is
// acceptable here because it costs nothing that matters: the recording path
// does not go through a subscriber channel, so an event missed by a subscriber
// is still in the event log.
const subscriberBuffer = 64

// AdapterOptions configures an Adapter.
//
// The name carries the Adapter prefix because this package already had an
// Options for the launcher, and a second `Options` in the same package would be
// a name that means one thing in claude.go and another here. The constructor is
// named to match.
type AdapterOptions struct {
	// Recorder receives the events the adapter produces. Nil means nothing is
	// recorded, which is a legitimate configuration - see EventRecorder.
	Recorder EventRecorder

	// Logger receives diagnostics. Nil means slog.Default.
	Logger *slog.Logger

	// Now supplies the current time, which is an event's CreatedAt. Nil means
	// time.Now.
	Now func() time.Time

	// Listen binds the hook receiver's socket. Nil binds a real one.
	//
	// It is injectable so that the lifecycle can be tested without a port, and
	// so that a test can produce the bind failure a caller has to handle.
	Listen func(ctx context.Context, network, address string) (net.Listener, error)

	// NewPath mints the receiver's nonce path. Nil mints a real one.
	NewPath func() (string, error)
}

// Adapter observes one Claude Code session and records what it sees.
//
// It is safe for concurrent use. Start and Stop are serialised against each
// other; Subscribe, HookURL and HookSettings may be called while a stream is
// being read.
type Adapter struct {
	recorder EventRecorder
	log      *slog.Logger
	now      func() time.Time
	listen   func(ctx context.Context, network, address string) (net.Listener, error)
	newPath  func() (string, error)

	// opMu serialises Start and Stop. It is held across the socket operations,
	// which is why it is separate from mu: mu guards the fields and is never
	// held while blocking on I/O.
	opMu sync.Mutex

	// mu guards everything below it.
	mu       sync.Mutex
	cfg      Config
	started  bool
	url      string
	server   *http.Server
	listener net.Listener
	bindings map[string]*Binding
	subs     map[int]chan Event
	nextSub  int
	dropped  int

	wg sync.WaitGroup
}

// NewAdapter builds an Adapter.
func NewAdapter(o AdapterOptions) *Adapter {
	a := &Adapter{
		recorder: o.Recorder,
		log:      o.Logger,
		now:      o.Now,
		listen:   o.Listen,
		newPath:  o.NewPath,
		bindings: make(map[string]*Binding),
		subs:     make(map[int]chan Event),
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.listen == nil {
		a.listen = (&net.ListenConfig{}).Listen
	}
	if a.newPath == nil {
		a.newPath = hookPathSpec.New
	}
	return a
}

// Start begins observing.
//
// It binds the hook receiver and returns. It does not start Claude, does not
// wait for one, and does not require one to be running: the receiver is an
// endpoint, and an endpoint that exists before the first delivery is what makes
// a session that starts a moment later observable from its first hook.
//
// The configuration is validated first and is refused rather than defaulted. A
// project id or a runtime id that cannot be given is a correlation that would
// have to be guessed, and a guessed correlation lands in a history that cannot
// be corrected. docs/CLAUDE_ADAPTER.md §7.
func (a *Adapter) Start(ctx context.Context, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	a.opMu.Lock()
	defer a.opMu.Unlock()

	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return newError(CodeAlreadyStarted, "this adapter is already observing")
	}
	a.mu.Unlock()

	path, err := a.newPath()
	if err != nil {
		return wrapError(err, CodeInvalidConfig, "could not mint the hook receiver's path")
	}
	path = "/hooks/" + path

	addr := strings.TrimSpace(cfg.HookAddr)
	if addr == "" {
		addr = DefaultHookAddr
	}
	listener, err := a.listen(ctx, "tcp", addr)
	if err != nil {
		return wrapError(err, CodeInvalidConfig,
			"could not bind the claude hook receiver to %s", addr).
			withDetail("address", addr)
	}

	url := "http://" + listener.Addr().String() + path
	server := &http.Server{
		Handler: &hookReceiver{adapter: a, path: path},
		// A hook delivery is a small POST answered immediately. The timeouts
		// are the ordinary ones for a loopback endpoint; the write timeout is
		// the one that has to clear the event write, which is a single local
		// insert.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}

	// The receiver's state is published before it starts serving. A delivery
	// that arrived between the two would otherwise be handled by an adapter
	// whose configuration is still zero, and would be recorded against no
	// project - or, worse, refused by the event service and dropped. The
	// window is small and closing it costs one ordering.
	a.mu.Lock()
	a.cfg = cfg
	a.started = true
	a.url = url
	a.server = server
	a.listener = listener
	// A restart observes a new session context, so what the previous run
	// correlated is not carried over.
	a.bindings = make(map[string]*Binding)
	a.dropped = 0
	a.mu.Unlock()

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		// Serve returns ErrServerClosed on a clean Stop, and the listener's own
		// error if it was closed underneath. Neither is worth a log line: Stop
		// is the only way out of this loop that is not a failure of the host.
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Warn("the claude hook receiver stopped serving", "error", err)
		}
	}()

	a.log.Info("claude adapter started",
		"projectId", cfg.ProjectID, "runtimeId", cfg.RuntimeID, "url", url)
	return nil
}

// Stop ends observation.
//
// It is idempotent - stopping an adapter that is not running is nothing to do,
// not a failure - and it closes every subscriber's channel. Nothing is sent to
// a closed channel: the lock that guards the subscriber set is held by both the
// close and the send, so the two cannot interleave.
//
// It does not touch Claude. A session that was being observed keeps running,
// with the runtime and the terminal that own it; what ends is AgentMux's
// attention to it.
func (a *Adapter) Stop(ctx context.Context) error {
	a.opMu.Lock()
	defer a.opMu.Unlock()

	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return nil
	}
	server := a.server
	listener := a.listener
	subs := a.subs
	dropped := a.dropped

	a.started = false
	a.subs = make(map[int]chan Event)
	a.server = nil
	a.listener = nil
	// The address goes with the receiver. A settings file naming a port nothing
	// is listening on is worse than no settings file, so a caller that renders
	// one after Stop is told the adapter is not running rather than handed a
	// URL that will silently drop every hook delivered to it.
	a.url = ""
	a.dropped = 0
	a.mu.Unlock()

	for _, ch := range subs {
		close(ch)
	}

	var err error
	if server != nil {
		if shutdownErr := server.Shutdown(ctx); shutdownErr != nil {
			// Shutdown gives up when its context does, and a listener left open
			// is a port held for the life of the process. Closing it directly
			// is the guarantee that Stop means the address is released.
			if listener != nil {
				_ = listener.Close()
			}
			err = wrapError(shutdownErr, CodeShutdownFailed,
				"the claude hook receiver did not shut down cleanly")
		}
	}
	a.wg.Wait()

	if dropped > 0 {
		a.log.Warn("claude observations were dropped for a subscriber that was not reading",
			"dropped", dropped)
	}
	a.log.Info("claude adapter stopped")
	return err
}

// Started reports whether the adapter is observing.
func (a *Adapter) Started() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.started
}

// configNow reports the configuration an observation should be attributed to,
// together with whether the adapter is observing at all.
//
// It is the read path for code that runs on a goroutine the adapter does not
// own - the hook receiver runs on the http server's - where reading the fields
// directly would race Start and Stop replacing them. The two are returned
// together because they are one question: a configuration without a running
// adapter names a session nothing is watching, and a running adapter whose
// configuration has not been read yet is the window Start closes by publishing
// before it serves.
func (a *Adapter) configNow() (Config, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg, a.started
}

// Subscribe returns a channel of every observation the adapter makes.
//
// It carries all of them, including the ones that are not recorded - the
// stream's init and hook lifecycle messages - so that a caller can follow a
// session at the granularity Claude reports it rather than at the granularity
// the event log keeps.
//
// The channel is closed by Stop, and by the end of the context it was opened
// with. Both are ordinary: a range over it ends, which is the signal to stop
// reading.
func (a *Adapter) Subscribe(ctx context.Context) (<-chan Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.started {
		return nil, newError(CodeNotStarted,
			"the adapter has not started, so there is nothing to subscribe to")
	}

	id := a.nextSub
	a.nextSub++
	ch := make(chan Event, subscriberBuffer)
	a.subs[id] = ch

	// A subscriber that goes away without calling Stop is released when its
	// context ends, so a forgotten channel is not written to for the life of
	// the process. A context with no Done channel - context.Background - has
	// nothing to wait on, and is left to Stop.
	if done := ctx.Done(); done != nil {
		go func() {
			<-done
			a.unsubscribe(id)
		}()
	}
	return ch, nil
}

// unsubscribe releases one subscriber.
func (a *Adapter) unsubscribe(id int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch, ok := a.subs[id]
	if !ok {
		return
	}
	delete(a.subs, id)
	close(ch)
}

// observe is the one path every observation takes.
//
// Three things happen, in this order: the session is correlated, the
// observation is published to subscribers, and it is offered to the event
// service. The order is the one that lets a subscriber that reacts to an event
// find the binding already in place.
func (a *Adapter) observe(ctx context.Context, ev Event) {
	a.bind(ev.SessionID, ev.CreatedAt)
	a.publish(ev)
	a.noteEvent(ctx, ev)
}

// publish hands an observation to every subscriber.
//
// It never blocks. A subscriber that has stopped reading loses observations
// rather than holding up a hook delivery, and the count of what it lost is
// reported when the adapter stops. Claude is waiting on the other end of a hook
// delivery, so the one thing this must not do is wait.
func (a *Adapter) publish(ev Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ch := range a.subs {
		select {
		case ch <- ev:
		default:
			a.dropped++
		}
	}
}

// bind correlates a Claude session id with the AgentMux context it was observed
// in.
//
// This is the whole of the session mapping, and it lives in memory. It is what
// lets a hook delivery, a stream init and a final result be recognised as one
// session, and it is rebuilt by the next adapter that observes the same
// session. docs/CLAUDE_ADAPTER.md §5 records why it is not stored and what a
// later phase would have to add to keep it.
func (a *Adapter) bind(sessionID string, at time.Time) {
	if sessionID == "" {
		// An observation that did not carry a session id - a stream message
		// before init, or a hook that omitted it. It is still recorded; it
		// simply does not extend the mapping.
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	binding, ok := a.bindings[sessionID]
	if !ok {
		binding = &Binding{
			SessionID:      sessionID,
			ProjectID:      a.cfg.ProjectID,
			RuntimeID:      a.cfg.RuntimeID,
			AgentSessionID: a.cfg.AgentSessionID,
			FirstSeen:      at,
		}
		a.bindings[sessionID] = binding
	}
	binding.LastSeen = at
	binding.Events++
}

// SessionFor reports what a Claude session id was correlated to.
func (a *Adapter) SessionFor(sessionID string) (Binding, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	binding, ok := a.bindings[sessionID]
	if !ok {
		return Binding{}, false
	}
	return *binding, true
}

// Sessions reports every session the adapter has observed, ordered by the
// Claude session id.
//
// The order is by identifier rather than by time so that two calls over the
// same set return the same sequence - a listing that reordered itself between
// reads would make a diff of two of them meaningless.
func (a *Adapter) Sessions() []Binding {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]Binding, 0, len(a.bindings))
	for _, binding := range a.bindings {
		out = append(out, *binding)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// validate refuses a configuration that cannot identify what is being observed.
//
// It checks that the two required identifiers are present and nothing more. It
// does not check that they name anything: resolving a project id to a project
// would make the adapter depend on the project store, which is a dependency
// this layer does not need and a decision - what to do about an event for a
// project that no longer exists - that belongs to the caller.
func (c Config) validate() error {
	if strings.TrimSpace(c.ProjectID) == "" {
		return newError(CodeInvalidConfig,
			"a claude adapter must be told which project it is observing").
			withDetail("field", "projectId")
	}
	if strings.TrimSpace(c.RuntimeID) == "" {
		return newError(CodeInvalidConfig,
			"a claude adapter must be told which runtime it is observing").
			withDetail("field", "runtimeId")
	}
	return nil
}
