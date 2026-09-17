package session

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kutonlagos/agentmux/internal/project"
)

// This file is the RuntimeManager: the layer between the HTTP API and the
// backend.
//
// It exists so that the API does not have to know what a session is. A handler
// asks the manager to start a project's runtime; the manager decides that the
// session is named after the project id, that its working directory is the
// project's runtime path, that its geometry is the canonical one, that a
// record is written, and that a sequence number is assigned to every chunk of
// output. None of those decisions belong in a request handler, and putting
// them there is how two endpoints end up disagreeing about what "start" means.

// Default terminal geometry. A session needs a size before any client exists,
// and guessing one from the first client to connect would make the terminal's
// size a side effect of who clicked first.
const (
	DefaultCols = 120
	DefaultRows = 30
)

// ProjectLookup is the slice of the project model the runtime manager needs.
type ProjectLookup interface {
	Get(ctx context.Context, id string) (*project.Project, error)
	List(ctx context.Context, filter project.ListFilter) ([]*project.Project, error)
}

// RuntimeStore persists the runtime metadata that has to survive a server
// restart. It stores no output: terminal bytes belong to the session, and
// copying them into a database would create a second, worse terminal.
type RuntimeStore interface {
	Save(ctx context.Context, rec Record) error
	Get(ctx context.Context, projectID string) (Record, error)
	List(ctx context.Context) ([]Record, error)
	Delete(ctx context.Context, projectID string) error
}

// ErrRecordNotFound means no runtime metadata is stored for a project.
var ErrRecordNotFound = errors.New("no runtime record")

// ManagerOptions configures a Manager. Backends, Projects, Sockets, and Store
// are required.
type ManagerOptions struct {
	// Backends builds the backend that owns one project's runtime. Required.
	//
	// It is a factory rather than one backend because one project's runtime
	// must not share a server with another's: a shared server is a shared
	// failure, which is the thing Phase 2.5 exists to remove.
	Backends BackendFactory

	// Sockets is where the project runtimes live. Required: reconciliation
	// asks it what is actually on disk, and a manager that could not would be
	// back to believing the database about liveness.
	Sockets SocketLayout

	// Projects resolves a project id into the project the runtime runs for.
	// Required.
	Projects ProjectLookup

	// Store persists runtime metadata. Required.
	Store RuntimeStore

	// Shell is the shell a session runs when no command is given.
	Shell string

	// Cols and Rows are the canonical geometry for a new session. Zero means
	// the defaults above.
	Cols int
	Rows int

	// HistoryChunks and HistoryBytes bound the in-memory output history each
	// runtime keeps. Zero means the buffer defaults.
	HistoryChunks int
	HistoryBytes  int

	// Agent resolves the coding agent a runtime can host. Nil means this build
	// hosts none, and every agent call answers that plainly rather than
	// pretending there is nothing to run.
	Agent AgentProvider

	// AgentPoll is how often a running agent is looked for, and how often a
	// stop waits for one to go. Zero means AgentDefaultPoll.
	AgentPoll time.Duration

	// AgentStartTimeout bounds the wait for a started agent to appear. Zero
	// means AgentDefaultStartTimeout.
	AgentStartTimeout time.Duration

	// AgentStopGrace bounds the wait for an interrupted agent to end. Zero
	// means AgentDefaultStopGrace.
	AgentStopGrace time.Duration

	// Logger receives runtime lifecycle events. Nil means slog.Default.
	Logger *slog.Logger

	// Now supplies the current time. Nil means time.Now.
	Now func() time.Time
}

// Agent timing defaults.
//
// The poll is short enough that a click on Stop feels immediate and long enough
// that watching five projects costs nothing worth measuring: each poll reads
// the process table, which is one directory listing.
const (
	AgentDefaultPoll         = 500 * time.Millisecond
	AgentDefaultStartTimeout = 15 * time.Second
	AgentDefaultStopGrace    = 5 * time.Second
)

// Manager owns every project's terminal runtime.
type Manager struct {
	newBackend  BackendFactory
	backendName string
	sockets     SocketLayout
	projects    ProjectLookup
	store       RuntimeStore
	shell       string
	cols        int
	rows        int
	chunks      int
	bytes       int
	log         *slog.Logger
	now         func() time.Time

	// agent resolves the coding agent a runtime hosts, and the timings that
	// decide how often one is looked for and how long a start or a stop waits.
	agent             AgentProvider
	agentPoll         time.Duration
	agentStartTimeout time.Duration
	agentStopGrace    time.Duration

	// ctx is the lifetime of every subscription this manager opens. It is
	// deliberately not a request's context: a subscription that died with the
	// HTTP request that started it would make the runtime stop being observed
	// the moment the user's browser navigated away.
	ctx    context.Context
	cancel context.CancelFunc

	// monitors counts what the Control Monitors have had to do. It is
	// bookkeeping for the stability harness and for diagnostics: "the monitor
	// is fine" is not a measurement, and a reconnect that happened is.
	monitorReconnects atomic.Int64
	monitorFailures   atomic.Int64

	mu       sync.Mutex
	runs     map[string]*runtime
	backends map[string]Backend
	orphans  map[string]Orphan
	wg       sync.WaitGroup
	closed   bool
}

// NewManager builds a Manager.
func NewManager(o ManagerOptions) (*Manager, error) {
	if o.Backends == nil {
		return nil, errors.New("session: Backends is required")
	}
	if o.Sockets == nil {
		return nil, errors.New("session: Sockets is required")
	}
	if o.Projects == nil {
		return nil, errors.New("session: Projects is required")
	}
	if o.Store == nil {
		return nil, errors.New("session: Store is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		newBackend:        o.Backends,
		backendName:       o.Backends.BackendName(),
		sockets:           o.Sockets,
		projects:          o.Projects,
		store:             o.Store,
		shell:             o.Shell,
		cols:              o.Cols,
		rows:              o.Rows,
		chunks:            o.HistoryChunks,
		bytes:             o.HistoryBytes,
		log:               o.Logger,
		now:               o.Now,
		agent:             o.Agent,
		agentPoll:         o.AgentPoll,
		agentStartTimeout: o.AgentStartTimeout,
		agentStopGrace:    o.AgentStopGrace,
		ctx:               ctx,
		cancel:            cancel,
		runs:              make(map[string]*runtime),
		backends:          make(map[string]Backend),
		orphans:           make(map[string]Orphan),
	}
	if m.cols <= 0 {
		m.cols = DefaultCols
	}
	if m.rows <= 0 {
		m.rows = DefaultRows
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.agentPoll <= 0 {
		m.agentPoll = AgentDefaultPoll
	}
	if m.agentStartTimeout <= 0 {
		m.agentStartTimeout = AgentDefaultStartTimeout
	}
	if m.agentStopGrace <= 0 {
		m.agentStopGrace = AgentDefaultStopGrace
	}
	return m, nil
}

// MonitorStats reports what the control monitors have had to do.
//
// A reconnect is not an error, so nothing else would ever surface one. It is
// counted because an installation whose monitors reconnect constantly is one
// with a runtime problem that otherwise looks like a working terminal.
type MonitorStats struct {
	// Reconnects is how many times a monitor had to re-establish its stream
	// after one ended while the session was still there.
	Reconnects int64 `json:"reconnects"`

	// StreamReconnects is how many times a control stream ended and was
	// re-established underneath a monitor that never noticed.
	//
	// It is separate from Reconnects because the two count different events.
	// Reconnects counts the Control Monitor rebuilding its subscription, which
	// only happens when the subscription ended too. StreamReconnects counts the
	// ordinary case: the control client went away, the subscription outlived it
	// and reattached, and everything above saw nothing but a gap in the output.
	// An installation being disconnected from constantly looks healthy in every
	// other number here, and this is the one that would say so.
	StreamReconnects int64 `json:"streamReconnects"`

	// Failures is how many times a monitor could not establish a stream at
	// all. The monitor keeps trying; this is the count of the attempts that
	// did not work.
	Failures int64 `json:"failures"`

	// Active is how many runtimes are being monitored right now.
	Active int `json:"active"`
}

// MonitorStats implements the diagnostic accessor.
func (m *Manager) MonitorStats() MonitorStats {
	m.mu.Lock()
	runs := make([]*runtime, 0, len(m.runs))
	for _, rt := range m.runs {
		runs = append(runs, rt)
	}
	m.mu.Unlock()

	active := 0
	for _, rt := range runs {
		rt.mu.Lock()
		if rt.sub != nil {
			active++
		}
		rt.mu.Unlock()
	}
	return MonitorStats{
		Reconnects:       m.monitorReconnects.Load(),
		StreamReconnects: controlReconnects.Load(),
		Failures:         m.monitorFailures.Load(),
		Active:           active,
	}
}

// SocketDir reports where the project runtimes live, for diagnostics.
func (m *Manager) SocketDir() string { return m.sockets.Dir() }

// BackendName names the kind of backend every project's runtime uses.
func (m *Manager) BackendName() string { return m.backendName }

// RuntimeStatus reports the terminal runtime installation the project runtimes
// will use, and whether the configured factory can describe it at all.
//
// The manager asks the factory rather than probing tmux itself. A second probe
// here would be a second answer to "which tmux", and the two would eventually
// disagree on exactly the machine this exists to explain: the one with more
// than one tmux installed. The factory resolves the same binary for this
// answer that it hands to every backend it builds.
func (m *Manager) RuntimeStatus(ctx context.Context) (TmuxStatus, bool) {
	reporter, ok := m.newBackend.(StatusReporter)
	if !ok {
		return TmuxStatus{}, false
	}
	return reporter.Status(ctx), true
}

// SessionRef is a live runtime session together with the socket it lives on.
//
// The socket is part of a session's identity now rather than decoration. There
// is no installation-wide server to list sessions from, so "which socket is
// this session on" is the first question about it - and the answer is what
// tells two same-named sessions in two different places apart.
type SessionRef struct {
	*Session

	// Socket is the tmux socket the session's server is listening on.
	Socket string `json:"socket"`

	// ProjectID is the project the socket belongs to, empty when no registered
	// project claims it.
	ProjectID string `json:"projectId,omitempty"`

	// Registered reports whether a registered project claims this socket. An
	// unregistered one is an orphan: reported, never touched.
	Registered bool `json:"registered"`
}

// Sessions reports every live runtime session on every socket in the socket
// directory.
//
// It is a diagnostic and it costs one tmux invocation per socket, so it is
// deliberately not on any hot path: the list of sessions is not something the
// runtime needs to know in order to run, only something a person asks when they
// want to see what is actually there. Reconciliation is the operation that does
// this work for a reason; this one does it because someone asked.
func (m *Manager) Sessions(ctx context.Context) ([]SessionRef, error) {
	projects, err := m.projects.List(ctx, project.ListFilter{})
	if err != nil {
		return nil, err
	}

	known := make(map[string]bool, len(projects))
	out := make([]SessionRef, 0, len(projects))
	for _, p := range projects {
		known[p.ID] = true
		path := m.sockets.Path(p.ID)
		if path == "" {
			continue
		}
		probe := m.sockets.Probe(ctx, path)
		if probe.State != SocketLive {
			continue
		}
		for _, s := range probe.Sessions {
			out = append(out, SessionRef{Session: s, Socket: path, ProjectID: p.ID, Registered: true})
		}
	}

	files, err := m.sockets.List()
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if file.WellFormed && known[file.ProjectID] {
			continue
		}
		probe := m.sockets.Probe(ctx, file.Path)
		if probe.State != SocketLive {
			continue
		}
		for _, s := range probe.Sessions {
			out = append(out, SessionRef{Session: s, Socket: file.Path})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Socket != out[j].Socket {
			return out[i].Socket < out[j].Socket
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// How long Screen waits for a runtime's output to stop moving before it
// captures, and how long it is willing to wait.
//
// The wait exists because a screen and a byte stream are not the same thing and
// cannot be made to agree at an instant. A capture and the sequence number that
// says "the stream is complete up to here" are two separate observations, and
// anything the pane writes between them is in the screen *and* in the stream.
//
// Nothing can close that window, because tmux will not report the sequence
// number that belongs to a capture. But it can be made empty in the case that
// dominates every other: if the runtime produces no output at all while the
// capture is taken, then nothing arrived in the window, and the boundary is
// exact. A terminal sitting at a prompt, or an agent waiting for a reply, is
// quiet for far longer than this.
//
// The maximum matters as much as the settle. A build printing continuously
// never goes quiet, and a client asking for a screen must not be made to wait
// for one to finish; past the deadline the capture is taken anyway, and
// docs/TERMINAL.md says what that costs.
const (
	screenSettle    = 30 * time.Millisecond
	screenSettleMax = 250 * time.Millisecond
)

// Screen returns a runtime's current screen together with the output sequence
// the returned screen already contains.
//
// It is the recovery half of the live-output contract rather than a substitute
// for it: a client that has just connected, or one whose stream was
// re-established, draws this and then follows the subscription. Reading the
// screen repeatedly instead of subscribing would turn a live terminal into a
// screenshot, which is why nothing in the runtime's normal path calls this.
//
// # The boundary, exactly
//
// The uint64 returned is the highest sequence number this runtime had published
// when the capture was requested. Its meaning is precise in one direction and
// deliberately not claimed in the other:
//
//   - Every chunk with a sequence number at or below it is already drawn in the
//     returned screen. A client must not replay those.
//   - Every chunk above it was published after the capture was requested, and
//     a client must apply those.
//
// It is not a claim that the screen is the result of applying chunks 1..n and
// nothing else. A chunk published *during* the capture may already be drawn in
// it. The quiesce above is what makes that case rare rather than routine; it is
// not what makes it impossible, and TERMINAL.md records it as a known limit of
// capture-pane rather than as something this code solves.
func (m *Manager) Screen(ctx context.Context, projectID string) (Screen, uint64, error) {
	rt, err := m.lookup(projectID)
	if err != nil {
		return Screen{}, 0, err
	}
	if rt == nil {
		return Screen{}, 0, newError(CodeNotRunning,
			"no terminal runtime is running for project %s", projectID)
	}
	backend, err := m.backendFor(projectID)
	if err != nil {
		return Screen{}, 0, err
	}

	boundary, err := m.quiesce(ctx, rt)
	if err != nil {
		return Screen{}, 0, err
	}

	screen, err := backend.Snapshot(ctx, rt.session)
	if err != nil {
		return Screen{}, 0, err
	}
	return screen, boundary, nil
}

// quiesce waits for a runtime's output to stop moving and returns the sequence
// boundary to capture against.
//
// The boundary is read after the wait rather than before it, and the ordering
// is the whole point. Read first, it would name a moment before the quiet
// period, and every chunk published during the quiet period would be replayed
// on top of a screen that already contains it. Read last, it names the state
// the quiet period ended in, and everything at or below it is provably drawn.
func (m *Manager) quiesce(ctx context.Context, rt *runtime) (uint64, error) {
	deadline := m.now().Add(screenSettleMax)
	last := rt.sequence()
	for {
		if !sleepContext(ctx, screenSettle) {
			return 0, wrapError(ctx.Err(), CodeBackendFailure,
				"gave up waiting for project %s's output to settle", rt.projectID)
		}
		current := rt.sequence()
		if current == last {
			return current, nil
		}
		if !m.now().Before(deadline) {
			// Output never stopped. The capture goes ahead: a client asking to
			// see the terminal must not be blocked by the terminal being busy.
			m.log.Debug("screen captured while output was still arriving",
				"projectId", rt.projectID, "sequence", current)
			return current, nil
		}
		last = current
	}
}

// sequence reads a runtime's current output sequence.
func (r *runtime) sequence() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// backendFor returns the backend that owns a project's runtime.
//
// It is the only way anything in this file reaches a server, which is what
// makes "one project, one server" a property of the code rather than a
// convention: there is no field holding a shared backend to reach for by
// mistake.
func (m *Manager) backendFor(projectID string) (Backend, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, newError(CodeUnavailable, "the runtime manager is shut down")
	}
	if backend, ok := m.backends[projectID]; ok {
		m.mu.Unlock()
		return backend, nil
	}
	m.mu.Unlock()

	// Built outside the lock: a factory may resolve a path or make a
	// directory, and holding the manager's lock across that would let one
	// project's setup stall every other project's status read - the exact
	// coupling per-project runtimes exist to remove.
	backend, err := m.newBackend.Backend(projectID)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, newError(CodeUnavailable, "the runtime manager is shut down")
	}
	// Two callers racing to build the same project's backend is normal - a
	// start and a status read - and only one of them may win, because two
	// handles to one server would disagree about which subscriptions are open.
	if existing, ok := m.backends[projectID]; ok {
		return existing, nil
	}
	m.backends[projectID] = backend
	return backend, nil
}

// runtime is one project's live runtime state.
//
// The mutex protects the fields below it. It is per runtime rather than one
// lock for the whole manager so that a slow operation on one project - a
// resize, a stop - cannot stall a status read on another, which is the whole
// point of having several sessions.
type runtime struct {
	projectID string
	session   string

	// opMu serialises the multi-step operations on this runtime: start, stop,
	// destroy.
	//
	// Those are sequences, not single calls - read a record, inspect a
	// session, create one, resize it, attach - and each step is a separate
	// tmux invocation. Two of them interleaved for the same project is how a
	// start adopts a session a destroy is in the middle of killing, and the
	// result is a runtime that reports RUNNING with nothing behind it. The
	// lock is per runtime so that serialising one project's lifecycle does not
	// serialise another's.
	//
	// Lock order: opMu may be held while taking mu; mu is never held while
	// taking opMu.
	opMu sync.Mutex

	mu      sync.Mutex
	state   State
	message string
	cols    int
	rows    int
	seq     uint64
	started time.Time
	updated time.Time

	// sub is the Control Monitor's stream: the one long-lived control-mode
	// client AgentMux holds for this project. It belongs to the manager, not
	// to any client of the API - see monitor.
	sub     Subscription
	buffer  *chunkBuffer
	pump    context.CancelFunc
	watched map[int]chan Chunk
	nextID  int

	// agent is what AgentMux last decided about the coding agent in this
	// runtime. The process table is the authority on whether it is running;
	// this is the record of how it got there, and of what to say about it.
	// Guarded by mu, like everything else here.
	agent agentState
}

// stateSnapshot is a consistent read of a runtime's mutable state.
//
// It deliberately says nothing about whether the session is alive. That is the
// backend's answer to give, not a flag to cache and grow stale.
type stateSnapshot struct {
	state    State
	message  string
	cols     int
	rows     int
	seq      uint64
	started  time.Time
	updated  time.Time
	attached bool
}

// snapshot reads a runtime's state atomically.
func (r *runtime) snapshot() stateSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	snap := stateSnapshot{
		state:   r.state,
		message: r.message,
		cols:    r.cols,
		rows:    r.rows,
		seq:     r.seq,
		started: r.started,
		updated: r.updated,
	}
	if r.sub != nil {
		snap.attached = r.sub.Connected()
	}
	return snap
}

// setState records a state transition.
func (r *runtime) setState(state State, message string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = state
	r.message = message
	r.updated = at
}

// The rest of this section is the agent half of a runtime's state. Every
// method takes the same lock as the runtime's own state, so a reader never sees
// a runtime that is RUNNING with an agent record that belongs to the runtime
// that came before it.

// setAgentLaunching records that a start was requested and nothing has been
// observed yet.
func (r *runtime) setAgentLaunching(spec AgentSpec, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agent = agentState{spec: spec, state: AgentStarting, since: at}
}

// failAgent records an agent that could not be started, or that started
// somewhere it must not run.
func (r *runtime) failAgent(reason string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agent.state = AgentFailed
	r.agent.reason = reason
	r.agent.pid = 0
	r.agent.dir = ""
	r.agent.ended = at
	r.agent.watched = false
}

// noteAgentStopRequested records that an operation asked the agent to stop.
//
// It is what makes the difference between an agent that was interrupted and one
// that ended on its own: the exit itself looks the same either way, and only
// the request distinguishes them.
func (r *runtime) noteAgentStopRequested(at time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.agent.state {
	case AgentRunning, AgentStarting:
		r.agent.state = AgentStopping
		r.agent.asked = true
		r.agent.reason = ""
		r.agent.ended = at
		return true
	}
	return false
}

// noteAgentStopped records that the agent ended because AgentMux asked it to.
func (r *runtime) noteAgentStopped(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agent.state = AgentStopped
	r.agent.asked = true
	r.agent.reason = ""
	r.agent.pid = 0
	r.agent.dir = ""
	r.agent.ended = at
	r.agent.watched = false
}

// noteAgentInterruptIgnored records that the interrupt was delivered and the
// agent was still there afterwards.
func (r *runtime) noteAgentInterruptIgnored(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agent.state = AgentRunning
	r.agent.asked = true
	r.agent.ended = time.Time{}
	r.agent.reason = "the interrupt was delivered and the agent is still running; " +
		"it may need a second one, or it may be waiting for a decision of its own"
}

// noteAgentEnded records that the agent's process is gone.
//
// Whether that is reported as a stop or as an unexplained exit is decided by
// the record of whether anything asked it to stop. The two are
// indistinguishable from the process table, and guessing "crash" for a Ctrl-C
// somebody typed into the terminal would be inventing a cause.
func (r *runtime) noteAgentEnded(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	requested := r.agent.asked
	if requested {
		r.agent.state = AgentStopped
		r.agent.reason = ""
	} else {
		r.agent.state = AgentExited
		r.agent.reason = "the agent exited and AgentMux did not ask it to"
	}
	r.agent.pid = 0
	r.agent.dir = ""
	r.agent.ended = at
	r.agent.watched = false
}

// noteAgentGone records that a stop was requested for an agent that had already
// ended on its own.
func (r *runtime) noteAgentGone(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agent.state == AgentRunning || r.agent.state == AgentStarting {
		r.agent.state = AgentExited
		r.agent.asked = false
		r.agent.reason = "the agent exited and AgentMux did not ask it to"
		r.agent.ended = at
	}
	r.agent.pid = 0
	r.agent.dir = ""
	r.agent.watched = false
}

// noteAgentSeen refreshes a running agent's observed details.
//
// It keeps the start time from the first observation, because the process
// cannot have started later than the moment it was first seen running.
func (r *runtime) noteAgentSeen(ref processRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agent.state = AgentRunning
	r.agent.pid = ref.PID
	r.agent.dir = ref.Dir
	r.agent.reason = ""
	if !ref.Started.IsZero() {
		r.agent.since = ref.Started
	}
}

// agentSnapshot reads a runtime's agent record atomically.
//
// It reports only what the runtime remembers, which is not the same as what is
// true: a runtime that has never hosted an agent remembers nothing, and one
// adopted from a previous server process remembers an agent that may have ended
// while nothing was watching. Whether an agent can be started here is the
// provider's answer, and whether one is running is the process table's; both are
// asked elsewhere.
func (r *runtime) agentSnapshot() (spec AgentSpec, state AgentState, pid int, dir string,
	since, ended time.Time, reason string, asked bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.agent
	if a.state == "" {
		a.state = AgentStopped
	}
	return a.spec, a.state, a.pid, a.dir, a.since, a.ended, a.reason, a.asked
}

// ProjectStatus implements project.RuntimeState.
//
// It is called while building a project listing, so it takes one short lock
// and allocates nothing.
func (m *Manager) ProjectStatus(projectID string) string {
	m.mu.Lock()
	rt, ok := m.runs[projectID]
	m.mu.Unlock()
	if !ok {
		return ""
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()

	switch rt.state {
	case StateStarting:
		return project.StatusStarting
	case StateRunning:
		// A session that is alive while its output stream is being
		// re-established is neither healthy nor dead, and a UI that shows it
		// as running would be claiming output that is not arriving.
		if rt.sub != nil && !rt.sub.Connected() {
			return project.StatusReconnecting
		}
		return project.StatusRunning
	case StateStopping:
		return project.StatusStopping
	case StateError:
		return project.StatusError
	case StateOrphan:
		return project.StatusOrphan
	default:
		return project.StatusStopped
	}
}

// Start brings a project's terminal runtime up.
//
// It is idempotent: starting a running runtime returns what is already there
// rather than failing or creating a second session. A user who reloads a page
// and clicks a button twice must not end up with two terminals writing to the
// same repository.
func (m *Manager) Start(ctx context.Context, projectID string) (*Runtime, error) {
	p, err := m.project(ctx, projectID)
	if err != nil {
		return nil, err
	}
	backend, err := m.backendFor(projectID)
	if err != nil {
		return nil, err
	}
	if err := backend.Available(ctx); err != nil {
		return nil, err
	}

	rt, err := m.runtimeFor(projectID, p.SessionName())
	if err != nil {
		return nil, err
	}
	rt.opMu.Lock()
	defer rt.opMu.Unlock()

	rt.mu.Lock()
	if rt.state == StateRunning {
		rt.mu.Unlock()
		return m.describe(ctx, rt), nil
	}
	cols, rows := rt.cols, rt.rows
	rt.state = StateStarting
	rt.message = ""
	rt.updated = m.now()
	rt.mu.Unlock()

	runtimePath, err := m.runtimePath(p)
	if err != nil {
		rt.setState(StateError, err.Error(), m.now())
		return nil, err
	}

	record, recordErr := m.store.Get(ctx, projectID)
	recordedSize := false
	if recordErr == nil && record.Cols > 0 && record.Rows > 0 {
		cols, rows = record.Cols, record.Rows
		recordedSize = true
	} else if recordErr != nil && !errors.Is(recordErr, ErrRecordNotFound) {
		rt.setState(StateError, "runtime metadata could not be read", m.now())
		return nil, wrapError(recordErr, CodeStorageFailure, "could not read the runtime record for %s", projectID)
	}

	// A session that exists is adopted rather than recreated. It may have been
	// left by a previous server process, and it may still be running work the
	// user cares about.
	session, created, err := m.ensureSession(ctx, backend, rt, runtimePath, cols, rows)
	if err != nil {
		rt.setState(StateError, err.Error(), m.now())
		m.recordFailure(ctx, projectID, rt.session)
		return nil, err
	}

	// When the session predates this server and nothing was written down about
	// it, the pty that is actually there is the only honest answer for how big
	// it is. Resizing it to a default instead would seize a terminal whose
	// program has already drawn itself to a size somebody chose, and the next
	// thing that program prints would be laid out wrong.
	if !created && !recordedSize && session.Cols > 0 && session.Rows > 0 {
		cols, rows = session.Cols, session.Rows
	}

	// The size is asserted on every start, not only at creation: the canonical
	// geometry is a decision, and re-asserting it is how it stays one.
	if err := backend.Resize(ctx, rt.session, cols, rows); err != nil {
		rt.setState(StateError, err.Error(), m.now())
		return nil, err
	}

	if err := m.attach(rt); err != nil {
		rt.setState(StateError, err.Error(), m.now())
		return nil, err
	}

	now := m.now()
	rt.mu.Lock()
	rt.state = StateRunning
	rt.cols, rt.rows = cols, rows
	rt.started = session.Created
	rt.updated = now
	rt.mu.Unlock()

	if err := m.store.Save(ctx, Record{
		ProjectID: projectID,
		Backend:   m.backendName,
		Session:   rt.session,
		State:     StateRunning,
		Cols:      cols,
		Rows:      rows,
		CreatedAt: session.Created,
		UpdatedAt: now,
		// The session was just created or adopted, so this is the moment it
		// was last confirmed to exist.
		LastSeenAt: &now,
	}); err != nil {
		// The session is running, so failing the whole start would be wrong -
		// but a caller has to know the record is missing, because it means the
		// runtime will not be rediscovered after a restart.
		m.log.Error("could not record the runtime; it will not survive a server restart",
			"projectId", projectID, "session", rt.session, "error", err)
		return nil, wrapError(err, CodeStorageFailure, "the runtime started but could not be recorded")
	}

	m.log.Info("runtime started",
		"projectId", projectID, "session", rt.session, "dir", runtimePath,
		"cols", cols, "rows", rows, "socket", m.sockets.Path(projectID))
	return m.describe(ctx, rt), nil
}

// ensureSession returns the project's session, creating it when absent. The
// second result reports whether this call made it.
func (m *Manager) ensureSession(ctx context.Context, backend Backend, rt *runtime, dir string, cols, rows int) (*Session, bool, error) {
	if existing, err := backend.Inspect(ctx, rt.session); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrNoSuchSession) {
		return nil, false, err
	}

	spec := SessionSpec{
		Name: rt.session,
		Dir:  dir,
		Cols: cols,
		Rows: rows,
	}
	if m.shell != "" {
		spec.Command = []string{m.shell}
	}
	session, err := backend.Create(ctx, spec)
	if err != nil {
		if errors.Is(err, ErrSessionExists) {
			// Lost a race with another start. The session exists, which is all
			// this call needed - but this call did not make it, and its size is
			// therefore not necessarily the size that was asked for.
			existing, inspectErr := backend.Inspect(ctx, rt.session)
			return existing, false, inspectErr
		}
		return nil, false, err
	}
	return session, true, nil
}

// runtimePath is the directory a project's session must run in.
//
// It is the project's runtime path and nothing else. A session started at a
// Projects Root, at a collection, or at the server's own directory would put
// the agent in the wrong repository while looking perfectly healthy.
func (m *Manager) runtimePath(p *project.Project) (string, error) {
	dir := strings.TrimSpace(p.RuntimePath)
	if dir == "" {
		return "", newError(CodeStartFailed,
			"project %s has no runtime path; it cannot host a session", p.ID)
	}
	return dir, nil
}

// attach opens the runtime's Control Monitor, reusing a healthy one.
//
// An existing subscription is reused only while it is healthy. A subscription
// whose stream has ended is a dead end: the session may since have been
// recreated under the same name, and reusing the old stream would leave a
// running terminal with no output arriving and nothing to say why.
func (m *Manager) attach(rt *runtime) error {
	backend, err := m.backendFor(rt.projectID)
	if err != nil {
		return err
	}

	rt.mu.Lock()
	if rt.sub != nil && rt.sub.Err() == nil {
		rt.mu.Unlock()
		return nil
	}
	stale, stalePump := rt.sub, rt.pump
	rt.sub, rt.pump = nil, nil
	rt.mu.Unlock()

	if stalePump != nil {
		stalePump()
	}
	if stale != nil {
		_ = stale.Close()
	}

	sub, err := backend.Attach(m.ctx, rt.session)
	if err != nil {
		return wrapError(err, CodeStartFailed, "could not read the output of session %q", rt.session)
	}

	ctx, cancel := context.WithCancel(m.ctx)
	rt.mu.Lock()
	rt.sub = sub
	rt.pump = cancel
	rt.mu.Unlock()

	m.wg.Add(1)
	go m.monitor(ctx, backend, rt, sub)
	return nil
}

// Re-attach policy for a Control Monitor whose stream ended.
const (
	monitorInitialBackoff = 250 * time.Millisecond
	monitorMaxBackoff     = 5 * time.Second
	monitorBackoffFactor  = 2
)

// monitor keeps one runtime's output stream alive for as long as its session
// exists.
//
// This is the Control Monitor, and its ownership rule is the reason it is a
// goroutine here rather than a connection somewhere else:
//
//   - One project has exactly one, for as long as its runtime exists. It is
//     established when the runtime starts or is adopted, and it is not
//     re-established because somebody connected.
//   - It belongs to the AgentMux server, not to a browser. A viewer reads the
//     runtime's buffered output; it never creates, closes, or interrupts the
//     thing that is reading the project's terminal. Phones that sleep, tablets
//     that reload, and windows that close are therefore not runtime events.
//   - It does not own the session. When the stream ends it re-establishes it;
//     when the session behind it is gone it stops. It never destroys a
//     session, because the session is precisely the thing that is supposed to
//     outlive everything on this side. A tmux server still running after
//     AgentMux has exited is the design, not a leak.
//
// The re-establishment is not defensive padding. A control client can die
// while its session lives - it is a separate process, and it can be killed,
// lose its socket, or be restarted - and the subscription below this is
// entitled to conclude the session is gone when a single probe of it fails.
// Deciding otherwise here, where the session can be asked again rather than
// guessed about, is what keeps one unlucky probe from silently ending a
// project's output.
func (m *Manager) monitor(ctx context.Context, backend Backend, rt *runtime, sub Subscription) {
	defer m.wg.Done()

	backoff := monitorInitialBackoff
	for {
		m.drain(ctx, rt, sub)
		if ctx.Err() != nil || m.ctx.Err() != nil {
			return
		}

		// The stream ended. Before believing that, ask whether the session is
		// still there - a dropped stream is not the same thing as an ended
		// session, and the whole reason this loop exists is that the two are
		// indistinguishable from inside a pump.
		alive, err := backend.Exists(ctx, rt.session)
		if err != nil {
			// The question could not be answered, so no conclusion is drawn.
			// Guessing either way is worse than waiting: guessing "gone" stops
			// observing a session that is still running, and guessing "alive"
			// loops on a dead one.
			m.monitorFailures.Add(1)
			m.log.Warn("could not tell whether the runtime's session is still there",
				"projectId", rt.projectID, "session", rt.session, "error", err)
			if !sleepContext(ctx, backoff) {
				return
			}
			backoff = min(backoff*monitorBackoffFactor, monitorMaxBackoff)
			continue
		}
		if !alive {
			// Nothing to reconnect to, and nothing to destroy. The session
			// ending is the runtime ending; the server it lived on is the
			// project's and outlives it, which is what Stop means - the
			// scrollback stays readable and the next Start reuses the server.
			m.log.Info("the runtime's terminal session is gone; its control monitor is stopping",
				"projectId", rt.projectID, "session", rt.session)
			rt.setState(StateStopped, "the terminal session ended", m.now())
			return
		}

		m.monitorReconnects.Add(1)
		m.log.Warn("the runtime's control monitor lost its stream; re-establishing it",
			"projectId", rt.projectID, "session", rt.session,
			"retryIn", backoff, "error", sub.Err())

		if !sleepContext(ctx, backoff) {
			return
		}
		backoff = min(backoff*monitorBackoffFactor, monitorMaxBackoff)

		next, err := backend.Attach(m.ctx, rt.session)
		if err != nil {
			m.monitorFailures.Add(1)
			m.log.Warn("could not re-establish the control monitor",
				"projectId", rt.projectID, "session", rt.session, "error", err)
			continue
		}
		backoff = monitorInitialBackoff
		rt.mu.Lock()
		rt.sub = next
		rt.mu.Unlock()
		sub = next
	}
}

// drain numbers a subscription's output and fans it out.
//
// This goroutine is the only writer of a runtime's sequence number, which is
// what makes the numbering strictly increasing without a lock around every
// chunk.
func (m *Manager) drain(ctx context.Context, rt *runtime, sub Subscription) {
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-sub.Output():
			if !ok {
				// The subscription ended. The session may still exist and be
				// reconnecting, so the runtime is not marked dead here - the
				// monitor decides that, by asking the session itself.
				if err := sub.Err(); err != nil {
					m.log.Warn("runtime output stream ended",
						"projectId", rt.projectID, "session", rt.session, "error", err)
				}
				return
			}
			m.publish(rt, data)
		}
	}
}

// publish assigns a sequence number to a chunk and delivers it.
func (m *Manager) publish(rt *runtime, data []byte) {
	now := m.now()

	rt.mu.Lock()
	rt.seq++
	chunk := Chunk{
		ProjectID: rt.projectID,
		Sequence:  rt.seq,
		Data:      data,
		Timestamp: now,
	}
	rt.updated = now
	rt.buffer.append(chunk)

	// Watchers are given a bounded buffer and are never allowed to slow the
	// reader down. A watcher that falls behind loses chunks rather than
	// stalling every other reader, and it can tell that it did, because the
	// chunk it receives next has a sequence number that skips. That is what
	// the sequence number is for.
	for id, ch := range rt.watched {
		select {
		case ch <- chunk:
		default:
			m.log.Warn("runtime watcher is behind and will miss output",
				"projectId", rt.projectID, "watcher", id, "sequence", chunk.Sequence)
		}
	}
	rt.mu.Unlock()
}

// Stop interrupts a project's runtime.
//
// It ends the work, not the terminal. The session survives with its shell and
// its scrollback, which is what makes the runtime persistent rather than
// merely restartable; Destroy is the call that ends a session.
//
// It is idempotent: stopping a stopped runtime is a success, because the
// caller's intent - nothing should be running - is already true.
//
// The project is resolved first, so that stopping a project that does not exist
// is reported as such rather than answered with a cheerful "stopped". Every
// method that names a project resolves it, and the four runtime endpoints agree
// on what a bad project id means because of it.
func (m *Manager) Stop(ctx context.Context, projectID string) (*Runtime, error) {
	if _, err := m.project(ctx, projectID); err != nil {
		return nil, err
	}
	rt, err := m.lookup(projectID)
	if err != nil {
		return nil, err
	}
	if rt == nil {
		return m.describeAbsent(ctx, projectID)
	}
	backend, err := m.backendFor(projectID)
	if err != nil {
		return nil, err
	}

	rt.opMu.Lock()
	defer rt.opMu.Unlock()

	rt.mu.Lock()
	state := rt.state
	rt.mu.Unlock()
	if state == StateStopped {
		return m.describe(ctx, rt), nil
	}

	rt.setState(StateStopping, "", m.now())
	// The runtime's stop interrupts the foreground process, which is the agent
	// when one is running. Recording that a stop was asked for is what lets the
	// agent's exit be reported as a stop rather than as a crash.
	rt.noteAgentStopRequested(m.now())
	if err := backend.Stop(ctx, rt.session); err != nil && !errors.Is(err, ErrNoSuchSession) {
		rt.setState(StateError, err.Error(), m.now())
		return nil, err
	}

	now := m.now()
	rt.setState(StateStopped, "", now)
	m.persistState(ctx, projectID, rt, StateStopped)
	m.log.Info("runtime stopped", "projectId", projectID, "session", rt.session)
	return m.describe(ctx, rt), nil
}

// Destroy removes a project's runtime completely.
//
// It ends the session, everything running in it, and its scrollback, it stops
// the project's own tmux server if that leaves the server empty, and it
// forgets the runtime record. Unlike Stop it is irreversible, which is why the
// API separates them: one is "stop working", the other is "throw it away".
//
// Since Phase 2.5 it is also narrowly scoped, and that is a property of the
// socket rather than of this function's care: the session name is this
// project's, the socket is this project's, and the server is this project's.
// Destroying one project cannot reach another's runtime even if it tried,
// because there is no command here that names anything outside this project's
// socket.
//
// The project is resolved first, like every other method here. A session left
// behind by a project that has since been removed is an orphan, and orphans are
// reported rather than destroyed - killing a terminal on the strength of a
// project id nobody recognises is exactly the guess reconciliation refuses to
// make.
func (m *Manager) Destroy(ctx context.Context, projectID string) error {
	p, err := m.project(ctx, projectID)
	if err != nil {
		return err
	}
	backend, err := m.backendFor(projectID)
	if err != nil {
		return err
	}

	m.mu.Lock()
	rt, ok := m.runs[projectID]
	m.mu.Unlock()

	if ok {
		rt.opMu.Lock()
		defer rt.opMu.Unlock()

		// The agent goes with the session, so its watcher is stopped first:
		// otherwise it would spend its next tick discovering the destruction
		// AgentMux just performed and reporting it as an unexplained exit.
		m.untrackAgent(rt)

		rt.mu.Lock()
		cancel, sub := rt.pump, rt.sub
		rt.pump, rt.sub = nil, nil
		rt.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if sub != nil {
			_ = sub.Close()
		}
		rt.setState(StateStopped, "", m.now())
	}

	session := p.SessionName()
	if err := backend.Destroy(ctx, session); err != nil {
		return err
	}

	// The project's tmux server is the project's own resource, so it goes too -
	// but only once it is known to be empty. Killing a server that still holds
	// a session would destroy something this call was not asked to destroy,
	// and the emptiness is established by asking the socket rather than by
	// assuming the session just killed was the only one on it.
	socketPath := m.sockets.Path(projectID)
	m.stopEmptyServer(ctx, backend, socketPath)

	m.mu.Lock()
	delete(m.runs, projectID)
	delete(m.backends, projectID)
	m.mu.Unlock()
	if err := backend.Close(); err != nil {
		m.log.Warn("could not release the project's tmux backend", "projectId", projectID, "error", err)
	}

	if err := m.store.Delete(ctx, projectID); err != nil && !errors.Is(err, ErrRecordNotFound) {
		return wrapError(err, CodeStorageFailure, "the session was destroyed but its record could not be removed")
	}

	m.log.Info("runtime destroyed", "projectId", projectID, "session", session, "socket", socketPath)
	return nil
}

// stopEmptyServer stops a project's tmux server when nothing is left on it,
// and reclaims the socket file afterwards.
//
// The reclaim is not optional tidiness. `kill-server` leaves the socket file
// behind - measured on tmux 3.4 and 3.7c - so a destroyed project without this
// would leave a file that every later reconciliation has to classify, and the
// directory would accumulate one dead socket per project ever destroyed.
func (m *Manager) stopEmptyServer(ctx context.Context, backend Backend, socketPath string) {
	if socketPath == "" {
		return
	}
	probe := m.sockets.Probe(ctx, socketPath)
	switch probe.State {
	case SocketLive:
		if len(probe.Sessions) > 0 {
			m.log.Warn("the project's tmux server still holds sessions; it is left running",
				"socket", socketPath, "sessions", len(probe.Sessions))
			return
		}
		if err := backend.KillServer(ctx); err != nil {
			m.log.Warn("could not stop the project's tmux server", "socket", socketPath, "error", err)
			return
		}
	case SocketStale:
		// The server is already gone and left its socket behind, which is what
		// kill-server does. There is nothing to stop, only a file to clear.
	default:
		// Absent, or a state that could not be classified. Either way this is not
		// the moment to touch the path: Reclaim refuses the unknown case for the
		// same reason.
		return
	}

	// Reclaim rather than os.Remove: it probes again, waits, and refuses unless
	// it is sure there is no server behind the name, which is the only way to
	// delete a socket without risking one that has just been started.
	if removed, err := m.sockets.Reclaim(ctx, socketPath); err != nil {
		m.log.Warn("could not reclaim the project's socket file", "socket", socketPath, "error", err)
	} else if removed {
		m.log.Debug("reclaimed the project's socket file", "socket", socketPath)
	}
}

// Runtime returns a project's runtime state, or a stopped runtime when the
// project has never had one.
//
// A never-started project is not an error. The runtime resource exists and is
// stopped, which gives a client one shape to render instead of two.
func (m *Manager) Runtime(ctx context.Context, projectID string) (*Runtime, error) {
	if _, err := m.project(ctx, projectID); err != nil {
		return nil, err
	}
	rt, err := m.lookup(projectID)
	if err != nil {
		return nil, err
	}
	if rt == nil {
		return m.describeAbsent(ctx, projectID)
	}
	return m.describe(ctx, rt), nil
}

// Input delivers raw terminal bytes to a project's runtime.
func (m *Manager) Input(ctx context.Context, projectID string, data []byte) error {
	rt, err := m.requireRunning(ctx, projectID)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	backend, err := m.backendFor(projectID)
	if err != nil {
		return err
	}
	return backend.SendInput(ctx, rt.session, data)
}

// Launch types a command line into a project's runtime.
func (m *Manager) Launch(ctx context.Context, projectID, command string) error {
	rt, err := m.requireRunning(ctx, projectID)
	if err != nil {
		return err
	}
	backend, err := m.backendFor(projectID)
	if err != nil {
		return err
	}
	return backend.Launch(ctx, rt.session, command)
}

// Resize sets a project's canonical terminal size.
//
// The size is persisted because it is a decision the user made, and the next
// start has to honour it: a runtime that came back at 80x24 after being
// resized would look like the resize had never worked.
func (m *Manager) Resize(ctx context.Context, projectID string, cols, rows int) (*Runtime, error) {
	if cols <= 0 || rows <= 0 {
		return nil, newError(CodeInvalidSize, "terminal size %dx%d is not usable", cols, rows)
	}
	rt, err := m.requireRunning(ctx, projectID)
	if err != nil {
		return nil, err
	}
	backend, err := m.backendFor(projectID)
	if err != nil {
		return nil, err
	}
	if err := backend.Resize(ctx, rt.session, cols, rows); err != nil {
		return nil, err
	}

	now := m.now()
	rt.mu.Lock()
	rt.cols, rt.rows = cols, rows
	rt.updated = now
	rt.mu.Unlock()

	m.persistState(ctx, projectID, rt, StateRunning)
	return m.describe(ctx, rt), nil
}

// History returns the buffered output after a sequence number.
func (m *Manager) History(ctx context.Context, projectID string, since uint64) ([]Chunk, error) {
	rt, err := m.lookup(projectID)
	if err != nil {
		return nil, err
	}
	if rt == nil {
		return nil, nil
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.buffer.since(since), nil
}

// Watch returns buffered chunks after since, plus a channel of new ones.
//
// The channel is closed when the returned cancel function is called or the
// manager shuts down. Its buffer is bounded: a caller that stops reading loses
// chunks rather than stalling the runtime, and detects the loss from the
// sequence numbers. Phase 4's terminal transport is the caller this was shaped
// for - a browser that stops reading is disconnected and re-synchronised with a
// snapshot, and the pane it was watching never notices.
func (m *Manager) Watch(ctx context.Context, projectID string, since uint64) ([]Chunk, <-chan Chunk, func(), error) {
	rt, err := m.lookup(projectID)
	if err != nil {
		return nil, nil, nil, err
	}
	if rt == nil {
		ch := make(chan Chunk)
		close(ch)
		return nil, ch, func() {}, nil
	}

	rt.mu.Lock()
	backlog := rt.buffer.since(since)
	if rt.watched == nil {
		rt.watched = make(map[int]chan Chunk)
	}
	id := rt.nextID
	rt.nextID++
	ch := make(chan Chunk, watchBufferDepth)
	rt.watched[id] = ch
	rt.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			rt.mu.Lock()
			if existing, ok := rt.watched[id]; ok {
				delete(rt.watched, id)
				close(existing)
			}
			rt.mu.Unlock()
		})
	}
	return backlog, ch, cancel, nil
}

// watchBufferDepth bounds one watcher's queue.
const watchBufferDepth = 256

// ReconcileReport describes what the runtimes looked like when the server
// started.
type ReconcileReport struct {
	// Running are projects whose runtime was rediscovered and adopted: their
	// socket has a live server and the server has their session. They are
	// adopted, not restarted.
	Running []string

	// Stopped are projects whose record exists but whose session is not on
	// their socket - because the session ended, or because the server went with
	// it. They are not started automatically: a server restart is not a request
	// to resume work.
	Stopped []string

	// Orphans are runtimes that exist on a socket but belong to no registered
	// project. They are reported and left alone.
	Orphans []Orphan

	// StaleSockets are socket files that were confirmed to have no server
	// behind them and were removed.
	StaleSockets []string

	// UnreadableSockets are socket paths that could not be classified. They are
	// reported rather than cleaned, because a socket AgentMux cannot read may
	// be a server it cannot reach.
	UnreadableSockets []string
}

// Orphan is a runtime that no registered project claims.
type Orphan struct {
	Session string `json:"session"`
	Socket  string `json:"socket,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`

	// ProjectID is the identifier the session name encodes, which may belong
	// to a project that was deleted or to no project at all.
	ProjectID string `json:"projectId,omitempty"`
}

// Reconcile compares what the database remembers with what the runtimes
// actually have, and reports the difference.
//
// It never starts anything and never kills anything. Both would be guesses: a
// session that survived a restart is evidence that the user wanted it, and a
// session whose project is gone may still hold work somebody needs. The honest
// thing to do with a discrepancy is to describe it.
//
// Since Phase 2.5 the unit of reconciliation is the socket rather than the
// installation, because a socket is now what a runtime is. That changes the
// cases it has to distinguish, and each one is answered with what the socket
// says rather than with what the database hopes:
//
//	record RUNNING, server live, session there   adopt it and reattach (A)
//	record RUNNING, server live, session gone    stopped - the session ended
//	record RUNNING, no server                    stopped, never still running (B)
//	socket live, no registered project           orphan; reported, not killed (C)
//	socket file, no server behind it             stale; confirmed, then removed (D)
//	socket path that cannot be read              unknown; reported, untouched
//
// The one thing that is never allowed is the second row's inverse: a runtime
// whose server is gone must not keep being reported as running. A client acting
// on "running" would send input to nothing and wait for output that cannot
// come, and would have no way to learn otherwise.
//
// The only file this removes is a socket that has been confirmed dead, and the
// confirmation is Reclaim's: two probes with a settle between them, and only
// ever on tmux saying there is no server there.
func (m *Manager) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var report ReconcileReport

	records, err := m.store.List(ctx)
	if err != nil {
		return report, wrapError(err, CodeStorageFailure, "could not read stored runtimes")
	}
	byProject := make(map[string]Record, len(records))
	for _, rec := range records {
		byProject[rec.ProjectID] = rec
	}

	projects, err := m.projects.List(ctx, project.ListFilter{})
	if err != nil {
		return report, err
	}
	known := make(map[string]bool, len(projects))
	for _, p := range projects {
		known[p.ID] = true
	}

	sockets, err := m.sockets.List()
	if err != nil {
		return report, err
	}

	m.mu.Lock()
	m.orphans = make(map[string]Orphan)
	m.mu.Unlock()

	// Sockets no registered project claims. Case C and case D, and the two are
	// answered differently on purpose: a server with sessions on it is somebody's
	// work and is only described, while a socket file with no server behind it
	// is litter and is removed once that has been confirmed.
	for _, file := range sockets {
		if file.WellFormed && known[file.ProjectID] {
			continue
		}
		m.reconcileUnclaimedSocket(ctx, file, &report)
	}

	for _, p := range projects {
		if err := m.reconcileProject(ctx, p, byProject[p.ID], &report); err != nil {
			return report, err
		}
	}

	sort.Strings(report.Running)
	sort.Strings(report.Stopped)
	sort.Strings(report.StaleSockets)
	sort.Strings(report.UnreadableSockets)
	sort.Slice(report.Orphans, func(i, j int) bool { return report.Orphans[i].Session < report.Orphans[j].Session })
	return report, nil
}

// reconcileUnclaimedSocket deals with one socket file that no registered
// project owns.
func (m *Manager) reconcileUnclaimedSocket(ctx context.Context, file SocketFile, report *ReconcileReport) {
	probe := m.sockets.Probe(ctx, file.Path)
	switch probe.State {
	case SocketAbsent:
		// It went away between the listing and the probe. Nothing to do, and
		// nothing to report: the state the caller cares about is already true.

	case SocketLive:
		// Case C. A server with no project behind it. It is reported and left
		// running, whether or not it has sessions: killing it would be AgentMux
		// destroying something it cannot account for, and the one thing that is
		// certainly true about it is that AgentMux does not know what it is.
		if len(probe.Sessions) == 0 {
			m.log.Warn("a tmux server is running on a socket that belongs to no registered project",
				"socket", file.Path, "projectId", file.ProjectID, "wellFormed", file.WellFormed)
			return
		}
		for _, s := range probe.Sessions {
			orphan := Orphan{
				Session:   s.Name,
				Socket:    file.Path,
				Dir:       s.Dir,
				Cols:      s.Cols,
				Rows:      s.Rows,
				ProjectID: strings.TrimPrefix(s.Name, project.SessionPrefix),
			}
			m.mu.Lock()
			m.orphans[s.Name] = orphan
			m.mu.Unlock()
			report.Orphans = append(report.Orphans, orphan)
		}

	case SocketStale:
		// Case D. tmux says there is no server here. It is removed, and Reclaim
		// confirms that a second time before unlinking anything.
		removed, err := m.sockets.Reclaim(ctx, file.Path)
		if err != nil {
			m.log.Warn("could not reclaim a stale socket", "socket", file.Path, "error", err)
			return
		}
		if removed {
			m.log.Info("removed a stale tmux socket left by a runtime that no project claims",
				"socket", file.Path, "projectId", file.ProjectID)
			report.StaleSockets = append(report.StaleSockets, file.Path)
		}

	default:
		m.log.Warn("a tmux socket could not be classified; it is left untouched",
			"socket", file.Path, "detail", probe.Detail)
		report.UnreadableSockets = append(report.UnreadableSockets, file.Path)
	}
}

// reconcileProject brings one project's runtime in line with what its socket
// and its record say.
func (m *Manager) reconcileProject(ctx context.Context, p *project.Project, rec Record, report *ReconcileReport) error {
	recorded := rec.ProjectID != ""

	// A socket path is only ever a function of the project id, so this cannot
	// be empty for a project the store issued. Refusing to continue rather than
	// falling back to a shared socket is the whole of Phase 2.5: a project
	// without its own socket has no runtime, not a borrowed one.
	socketPath := m.sockets.Path(p.ID)
	if socketPath == "" {
		m.log.Error("a project has no runtime socket path; its runtime is not reconciled",
			"projectId", p.ID)
		report.Stopped = append(report.Stopped, p.ID)
		return nil
	}

	probe := m.sockets.Probe(ctx, socketPath)
	var session *Session
	if probe.State == SocketLive {
		for _, s := range probe.Sessions {
			if s.Name == p.SessionName() {
				session = s
				continue
			}
			// A second session on a project's own socket. AgentMux puts exactly
			// one session on a server, so this was put there by something else -
			// and since the socket is AgentMux's, "something else" had to reach
			// past the directory's permissions to do it. It is reported and left
			// alone.
			orphan := Orphan{
				Session:   s.Name,
				Socket:    socketPath,
				Dir:       s.Dir,
				Cols:      s.Cols,
				Rows:      s.Rows,
				ProjectID: strings.TrimPrefix(s.Name, project.SessionPrefix),
			}
			m.mu.Lock()
			m.orphans[s.Name] = orphan
			m.mu.Unlock()
			report.Orphans = append(report.Orphans, orphan)
		}
	}

	switch {
	case session != nil && recorded && rec.State == StateStopped:
		// The user stopped this runtime and the session outlived the server.
		// Reporting it as running would undo their decision.
		rt, err := m.runtimeFor(p.ID, p.SessionName())
		if err != nil {
			return err
		}
		rt.mu.Lock()
		rt.state = StateStopped
		rt.cols, rt.rows = rec.Cols, rec.Rows
		rt.started = session.Created
		rt.updated = m.now()
		rt.mu.Unlock()
		report.Stopped = append(report.Stopped, p.ID)

	case session != nil:
		// Case A: adopt the session that is already there.
		rt, err := m.runtimeFor(p.ID, p.SessionName())
		if err != nil {
			return err
		}
		cols, rows := session.Cols, session.Rows
		if recorded && rec.Cols > 0 && rec.Rows > 0 {
			cols, rows = rec.Cols, rec.Rows
		}
		rt.mu.Lock()
		rt.cols, rt.rows = cols, rows
		rt.started = session.Created
		rt.state = StateStarting
		rt.updated = m.now()
		rt.mu.Unlock()

		backend, err := m.backendFor(p.ID)
		if err != nil {
			return err
		}
		if err := backend.Resize(ctx, p.SessionName(), cols, rows); err != nil {
			rt.setState(StateError, err.Error(), m.now())
			m.log.Error("could not restore the runtime size", "projectId", p.ID, "error", err)
			return nil
		}
		if err := m.attach(rt); err != nil {
			rt.setState(StateError, err.Error(), m.now())
			m.log.Error("could not reattach to a surviving session", "projectId", p.ID, "error", err)
			return nil
		}
		now := m.now()
		rt.setState(StateRunning, "", now)
		m.persistState(ctx, p.ID, rt, StateRunning)
		report.Running = append(report.Running, p.ID)
		m.log.Info("runtime rediscovered", "projectId", p.ID, "session", session.Name,
			"socket", socketPath, "dir", session.Dir)

	case recorded:
		// Case B: the record exists, the session does not. Report it stopped
		// and leave starting to the user.
		rt, err := m.runtimeFor(p.ID, p.SessionName())
		if err != nil {
			return err
		}
		message := ""
		if rec.State == StateRunning {
			// Which of the two happened is worth distinguishing, because one is
			// a shell that exited and the other is a server that went away, and
			// only the second one is the Phase 2 anomaly.
			if probe.State == SocketLive {
				message = "the terminal session ended"
			} else {
				message = "the terminal session ended while the server was not running"
			}
		}
		rt.mu.Lock()
		rt.state = StateStopped
		rt.message = message
		rt.cols, rt.rows = rec.Cols, rec.Rows
		if rt.cols <= 0 || rt.rows <= 0 {
			rt.cols, rt.rows = m.cols, m.rows
		}
		rt.updated = m.now()
		rt.mu.Unlock()
		report.Stopped = append(report.Stopped, p.ID)
	}

	// Case D for a project AgentMux does know about: its record may be a
	// leftover, and the socket file behind it certainly is. Only a socket with
	// no server behind it is touched, and only once Reclaim has confirmed it.
	if probe.State == SocketStale {
		removed, err := m.sockets.Reclaim(ctx, socketPath)
		if err != nil {
			m.log.Warn("could not reclaim a stale socket", "projectId", p.ID, "socket", socketPath, "error", err)
		} else if removed {
			m.log.Info("removed a stale tmux socket", "projectId", p.ID, "socket", socketPath)
			report.StaleSockets = append(report.StaleSockets, socketPath)
		}
	}
	if probe.State == SocketUnknown {
		m.log.Warn("a project's tmux socket could not be classified; it is left untouched",
			"projectId", p.ID, "socket", socketPath, "detail", probe.Detail)
		report.UnreadableSockets = append(report.UnreadableSockets, socketPath)
	}
	return nil
}

// orphansForReport is the orphan map in report order.
func (m *Manager) orphansForReport() []Orphan {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Orphan, 0, len(m.orphans))
	for name, s := range m.orphans {
		out = append(out, Orphan{
			Session:   name,
			Dir:       s.Dir,
			Cols:      s.Cols,
			Rows:      s.Rows,
			ProjectID: strings.TrimPrefix(name, project.SessionPrefix),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out
}

// Orphans returns the last reconciliation's orphaned runtimes.
func (m *Manager) Orphans() []Orphan {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Orphan, 0, len(m.orphans))
	for _, o := range m.orphans {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out
}

// Describe returns a project's runtime from its own socket's point of view.
//
// It is a diagnostic: it answers "what does this project's runtime think is
// there", independently of what the database remembers or what this manager has
// in memory. The three disagreeing is exactly the situation a reconciliation
// report exists to surface.
func (m *Manager) Describe(ctx context.Context, projectID string) (*Session, error) {
	backend, err := m.backendFor(projectID)
	if err != nil {
		return nil, err
	}
	return backend.Inspect(ctx, project.SessionNameFor(projectID))
}

// Close releases the manager's resources without touching any session.
//
// Shutting AgentMux down is not a request to end anyone's work, and since
// Phase 2.5 that is a property of the architecture rather than of this
// function's care: the tmux servers belong to the projects, not to AgentMux, so
// there is nothing here to stop. The control monitors are closed and their
// sessions are left running on their own servers, which is what makes a restart
// a reconnection rather than a loss.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	runs := make([]*runtime, 0, len(m.runs))
	for _, rt := range m.runs {
		runs = append(runs, rt)
	}
	backends := make([]Backend, 0, len(m.backends))
	for _, backend := range m.backends {
		backends = append(backends, backend)
	}
	m.mu.Unlock()

	for _, rt := range runs {
		rt.mu.Lock()
		cancel, sub := rt.pump, rt.sub
		rt.pump, rt.sub = nil, nil
		watchers := rt.watched
		rt.watched = nil
		rt.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		if sub != nil {
			_ = sub.Close()
		}
		for _, ch := range watchers {
			close(ch)
		}
	}

	m.cancel()
	m.wg.Wait()

	// Any subscription a backend still holds after its runtime was drained. The
	// per-runtime close above is the normal path; this is the backstop for a
	// backend whose runtime was never adopted, so that no control client is
	// left attached to a server this process is about to stop talking to.
	var firstErr error
	for _, backend := range backends {
		if err := backend.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// project resolves a project id, mapping a miss to a runtime error.
func (m *Manager) project(ctx context.Context, projectID string) (*project.Project, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, newError(CodeProjectNotFound, "no project id was given")
	}
	p, err := m.projects.Get(ctx, projectID)
	if err != nil {
		var projectErr *project.Error
		if errors.As(err, &projectErr) && projectErr.Code == project.CodeNotFound {
			return nil, wrapError(err, CodeProjectNotFound, "no project with id %q", projectID)
		}
		return nil, err
	}
	return p, nil
}

// lookup returns a project's runtime, or nil when it has none.
func (m *Manager) lookup(projectID string) (*runtime, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, newError(CodeProjectNotFound, "no project id was given")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[projectID], nil
}

// requireRunning returns a project's runtime, insisting that it be usable.
//
// The project is resolved first so that addressing one that does not exist is
// reported as such. Without that, every method here would answer "not running"
// for a project id that was never real, and a client would go looking for a
// runtime instead of at its own request.
func (m *Manager) requireRunning(ctx context.Context, projectID string) (*runtime, error) {
	if _, err := m.project(ctx, projectID); err != nil {
		return nil, err
	}
	rt, err := m.lookup(projectID)
	if err != nil {
		return nil, err
	}
	if rt == nil {
		return nil, newError(CodeNotRunning, "no terminal runtime is running for project %s", projectID)
	}
	rt.mu.Lock()
	state := rt.state
	rt.mu.Unlock()
	if state != StateRunning {
		return nil, newError(CodeNotRunning,
			"the terminal runtime for project %s is %s, not running", projectID, strings.ToLower(string(state)))
	}
	return rt, nil
}

// runtimeFor returns a project's runtime state, creating it when absent.
func (m *Manager) runtimeFor(projectID, session string) (*runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, newError(CodeUnavailable, "the runtime manager is shut down")
	}
	if rt, ok := m.runs[projectID]; ok {
		return rt, nil
	}
	rt := &runtime{
		projectID: projectID,
		session:   session,
		state:     StateStopped,
		cols:      m.cols,
		rows:      m.rows,
		buffer:    newChunkBuffer(m.chunks, m.bytes),
		updated:   m.now(),
	}
	m.runs[projectID] = rt
	return rt, nil
}

// describe builds the API view of a live runtime.
func (m *Manager) describe(ctx context.Context, rt *runtime) *Runtime {
	snap := rt.snapshot()

	// Liveness is asked of the project's own socket rather than inferred from
	// the state. The two genuinely differ: a stopped runtime keeps its session
	// so that its scrollback survives, and a runtime whose server died under it
	// has a state that is optimistic and a session that is not there.
	alive := false
	if backend, err := m.backendFor(rt.projectID); err == nil {
		alive, _ = backend.Exists(ctx, rt.session)
	}

	return &Runtime{
		ProjectID:    rt.projectID,
		Backend:      m.backendName,
		Session:      rt.session,
		State:        snap.state,
		SessionAlive: alive,
		Cols:         snap.cols,
		Rows:         snap.rows,
		Sequence:     snap.seq,
		StartedAt:    snap.started,
		UpdatedAt:    snap.updated,
		Message:      snap.message,
		Agent:        m.agentFor(ctx, rt),
	}
}

// agentFor builds a runtime's agent view, or nil when this server has no agent
// to report.
//
// The nil case is a server that hosts terminals and no coding agent, which is a
// legitimate configuration: the field is absent from the response rather than
// present and empty, so a client can tell "there is no agent here" from "the
// agent is not running".
func (m *Manager) agentFor(ctx context.Context, rt *runtime) *AgentStatus {
	if m.agent == nil {
		return nil
	}
	status := m.agentStatus(ctx, rt)
	return &status
}

// describeAbsent builds the API view of a project that has no runtime yet.
func (m *Manager) describeAbsent(ctx context.Context, projectID string) (*Runtime, error) {
	cols, rows := m.cols, m.rows
	session := project.SessionNameFor(projectID)
	alive := false

	if record, err := m.store.Get(ctx, projectID); err == nil {
		if record.Cols > 0 && record.Rows > 0 {
			cols, rows = record.Cols, record.Rows
		}
		if record.Session != "" {
			session = record.Session
		}
		if backend, err := m.backendFor(projectID); err == nil {
			alive, _ = backend.Exists(ctx, session)
		}
	} else if !errors.Is(err, ErrRecordNotFound) {
		return nil, wrapError(err, CodeStorageFailure, "could not read the runtime record for %s", projectID)
	}

	return &Runtime{
		ProjectID:    projectID,
		Backend:      m.backendName,
		Session:      session,
		State:        StateStopped,
		SessionAlive: alive,
		Cols:         cols,
		Rows:         rows,
		UpdatedAt:    m.now(),
		Agent:        m.absentAgentFor(ctx),
	}, nil
}

// absentAgentFor describes a project whose runtime does not exist.
//
// It reports the agent installation rather than nothing, because "Claude Code
// is installed and available, and no agent is running" is what the client needs
// to decide whether to offer Start.
func (m *Manager) absentAgentFor(ctx context.Context) *AgentStatus {
	if m.agent == nil {
		return nil
	}
	status := m.absentAgentStatus(ctx)
	return &status
}

// persistState writes a runtime's settled state.
//
// Only settled states are written. A transition recorded on disk would be a
// claim about a process that is no longer running, and the reader of that
// record is a server that just started and cannot check.
func (m *Manager) persistState(ctx context.Context, projectID string, rt *runtime, state State) {
	if !state.Settled() {
		return
	}
	now := m.now()
	snap := rt.snapshot()
	existing, err := m.store.Get(ctx, projectID)
	if err != nil && !errors.Is(err, ErrRecordNotFound) {
		m.log.Error("could not read the runtime record before updating it",
			"projectId", projectID, "error", err)
		return
	}

	record := Record{
		ProjectID: projectID,
		Backend:   m.backendName,
		Session:   rt.session,
		State:     state,
		Cols:      snap.cols,
		Rows:      snap.rows,
		CreatedAt: snap.started,
		UpdatedAt: now,
		// A stopped runtime has nothing new to say about liveness, so the last
		// confirmation is carried forward rather than erased. A running one was
		// just confirmed to exist, which is exactly what this field records.
		LastSeenAt: existing.LastSeenAt,
	}
	if state == StateRunning {
		record.LastSeenAt = &now
	}
	if !existing.CreatedAt.IsZero() {
		record.CreatedAt = existing.CreatedAt
	}
	if err := m.store.Save(ctx, record); err != nil {
		m.log.Error("could not persist runtime state", "projectId", projectID, "state", state, "error", err)
	}
}

// recordFailure records a runtime that failed to start, so that a restart
// explains itself instead of silently reporting the project as never started.
func (m *Manager) recordFailure(ctx context.Context, projectID, session string) {
	record := Record{
		ProjectID: projectID,
		Backend:   m.backendName,
		Session:   session,
		State:     StateError,
		UpdatedAt: m.now(),
	}
	if err := m.store.Save(ctx, record); err != nil {
		m.log.Error("could not record a failed runtime start", "projectId", projectID, "error", err)
	}
}
