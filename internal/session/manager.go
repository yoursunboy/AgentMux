package session

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
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

// ManagerOptions configures a Manager. Backend, Projects, and Store are
// required.
type ManagerOptions struct {
	// Backend is the session runtime. Required.
	Backend Backend

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

	// Logger receives runtime lifecycle events. Nil means slog.Default.
	Logger *slog.Logger

	// Now supplies the current time. Nil means time.Now.
	Now func() time.Time
}

// Manager owns every project's terminal runtime.
type Manager struct {
	backend  Backend
	projects ProjectLookup
	store    RuntimeStore
	shell    string
	cols     int
	rows     int
	chunks   int
	bytes    int
	log      *slog.Logger
	now      func() time.Time

	// ctx is the lifetime of every subscription this manager opens. It is
	// deliberately not a request's context: a subscription that died with the
	// HTTP request that started it would make the runtime stop being observed
	// the moment the user's browser navigated away.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	runs    map[string]*runtime
	orphans map[string]*Session
	wg      sync.WaitGroup
	closed  bool
}

// NewManager builds a Manager.
func NewManager(o ManagerOptions) (*Manager, error) {
	if o.Backend == nil {
		return nil, errors.New("session: Backend is required")
	}
	if o.Projects == nil {
		return nil, errors.New("session: Projects is required")
	}
	if o.Store == nil {
		return nil, errors.New("session: Store is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		backend:  o.Backend,
		projects: o.Projects,
		store:    o.Store,
		shell:    o.Shell,
		cols:     o.Cols,
		rows:     o.Rows,
		chunks:   o.HistoryChunks,
		bytes:    o.HistoryBytes,
		log:      o.Logger,
		now:      o.Now,
		ctx:      ctx,
		cancel:   cancel,
		runs:     make(map[string]*runtime),
		orphans:  make(map[string]*Session),
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
	return m, nil
}

// Backend exposes the backend for diagnostics.
func (m *Manager) Backend() Backend { return m.backend }

// runtime is one project's live runtime state.
//
// The mutex protects the fields below it. It is per runtime rather than one
// lock for the whole manager so that a slow operation on one project - a
// resize, a stop - cannot stall a status read on another, which is the whole
// point of having several sessions.
type runtime struct {
	projectID string
	session   string

	mu      sync.Mutex
	state   State
	message string
	cols    int
	rows    int
	seq     uint64
	started time.Time
	updated time.Time

	sub     Subscription
	buffer  *chunkBuffer
	pump    context.CancelFunc
	watched map[int]chan Chunk
	nextID  int
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
	if err := m.backend.Available(ctx); err != nil {
		return nil, err
	}

	rt, err := m.runtimeFor(projectID, p.SessionName())
	if err != nil {
		return nil, err
	}

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
	session, created, err := m.ensureSession(ctx, rt, runtimePath, cols, rows)
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
	if err := m.backend.Resize(ctx, rt.session, cols, rows); err != nil {
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
		Backend:   m.backend.Name(),
		Session:   rt.session,
		State:     StateRunning,
		Cols:      cols,
		Rows:      rows,
		CreatedAt: session.Created,
		UpdatedAt: now,
		// The session was just created or adopted, so this is the moment it was
		// last confirmed to exist.
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
		"projectId", projectID, "session", rt.session, "dir", runtimePath, "cols", cols, "rows", rows)
	return m.describe(ctx, rt), nil
}

// ensureSession returns the project's session, creating it when absent. The
// second result reports whether this call made it.
func (m *Manager) ensureSession(ctx context.Context, rt *runtime, dir string, cols, rows int) (*Session, bool, error) {
	if existing, err := m.backend.Inspect(ctx, rt.session); err == nil {
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
	session, err := m.backend.Create(ctx, spec)
	if err != nil {
		if errors.Is(err, ErrSessionExists) {
			// Lost a race with another start. The session exists, which is all
			// this call needed - but this call did not make it, and its size is
			// therefore not necessarily the size that was asked for.
			existing, inspectErr := m.backend.Inspect(ctx, rt.session)
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

// attach opens the output stream and starts the pump that numbers it.
//
// An existing subscription is reused only while it is healthy. A subscription
// whose stream has ended is a dead end: the session may since have been
// recreated under the same name, and reusing the old stream would leave a
// running terminal with no output arriving and nothing to say why.
func (m *Manager) attach(rt *runtime) error {
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

	sub, err := m.backend.Attach(m.ctx, rt.session)
	if err != nil {
		return wrapError(err, CodeStartFailed, "could not read the output of session %q", rt.session)
	}

	ctx, cancel := context.WithCancel(m.ctx)
	rt.mu.Lock()
	rt.sub = sub
	rt.pump = cancel
	rt.mu.Unlock()

	m.wg.Add(1)
	go m.pump(ctx, rt, sub)
	return nil
}

// pump numbers a subscription's output and fans it out.
//
// This goroutine is the only writer of a runtime's sequence number, which is
// what makes the numbering strictly increasing without a lock around every
// chunk.
func (m *Manager) pump(ctx context.Context, rt *runtime, sub Subscription) {
	defer m.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-sub.Output():
			if !ok {
				// The subscription ended. The session may still exist and be
				// reconnecting, so the runtime is not marked dead here - the
				// next reconciliation or start will decide that.
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

	rt.mu.Lock()
	state := rt.state
	rt.mu.Unlock()
	if state == StateStopped {
		return m.describe(ctx, rt), nil
	}

	rt.setState(StateStopping, "", m.now())
	if err := m.backend.Stop(ctx, rt.session); err != nil && !errors.Is(err, ErrNoSuchSession) {
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
// It ends the session, everything running in it, and its scrollback, and it
// forgets the runtime record. Unlike Stop it is irreversible, which is why the
// API separates them: one is "stop working", the other is "throw it away".
//
// The project is resolved first, like every other method here. A session left
// behind by a project that has since been removed is an orphan, and orphans are
// reported rather than destroyed - killing a terminal on the strength of a
// project id nobody recognises is exactly the guess reconciliation refuses to
// make.
func (m *Manager) Destroy(ctx context.Context, projectID string) error {
	if _, err := m.project(ctx, projectID); err != nil {
		return err
	}
	m.mu.Lock()
	rt, ok := m.runs[projectID]
	if ok {
		delete(m.runs, projectID)
	}
	m.mu.Unlock()

	session := project.SessionNameFor(projectID)

	if ok {
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

	if err := m.backend.Destroy(ctx, session); err != nil {
		return err
	}
	if err := m.store.Delete(ctx, projectID); err != nil && !errors.Is(err, ErrRecordNotFound) {
		return wrapError(err, CodeStorageFailure, "the session was destroyed but its record could not be removed")
	}

	m.log.Info("runtime destroyed", "projectId", projectID, "session", session)
	return nil
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
	return m.backend.SendInput(ctx, rt.session, data)
}

// Launch types a command line into a project's runtime.
func (m *Manager) Launch(ctx context.Context, projectID, command string) error {
	rt, err := m.requireRunning(ctx, projectID)
	if err != nil {
		return err
	}
	return m.backend.Launch(ctx, rt.session, command)
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
	if err := m.backend.Resize(ctx, rt.session, cols, rows); err != nil {
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
// sequence numbers. This is the shape the WebSocket terminal in a later phase
// needs, and it is here now so that the data model is proven rather than
// asserted.
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

// ReconcileReport describes what the runtime looked like when the server
// started.
type ReconcileReport struct {
	// Running are projects whose session exists and whose record said it
	// should be running. They are adopted, not restarted.
	Running []string

	// Stopped are projects whose record exists but whose session does not.
	// They are not started automatically: a server restart is not a request to
	// resume work.
	Stopped []string

	// Orphans are sessions that exist but belong to no registered project.
	// They are reported and left alone.
	Orphans []Orphan
}

// Orphan is a runtime session that no registered project claims.
type Orphan struct {
	Session string `json:"session"`
	Dir     string `json:"dir,omitempty"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`

	// ProjectID is the identifier the session name encodes, which may belong
	// to a project that was deleted or to no project at all.
	ProjectID string `json:"projectId,omitempty"`
}

// Reconcile compares what the database remembers with what the runtime
// actually has, and reports the difference.
//
// It never starts anything and never kills anything. Both would be guesses: a
// session that survived a restart is evidence that the user wanted it, and a
// session whose project is gone may still hold work somebody needs. The
// honest thing to do with a discrepancy is to describe it.
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

	sessions, err := m.backend.List(ctx)
	if err != nil {
		return report, err
	}
	live := make(map[string]*Session, len(sessions))
	for _, s := range sessions {
		live[s.Name] = s
	}

	// Case C: a session with no registered project behind it.
	m.mu.Lock()
	m.orphans = make(map[string]*Session)
	for _, s := range sessions {
		id := strings.TrimPrefix(s.Name, project.SessionPrefix)
		if known[id] {
			continue
		}
		m.orphans[s.Name] = s
		report.Orphans = append(report.Orphans, Orphan{
			Session:   s.Name,
			Dir:       s.Dir,
			Cols:      s.Cols,
			Rows:      s.Rows,
			ProjectID: id,
		})
	}
	m.mu.Unlock()
	sort.Slice(report.Orphans, func(i, j int) bool { return report.Orphans[i].Session < report.Orphans[j].Session })

	for _, p := range projects {
		rec, recorded := byProject[p.ID]
		session, running := live[p.SessionName()]

		switch {
		case running && recorded && rec.State == StateStopped:
			// The user stopped this runtime and the session outlived the
			// server. Reporting it as running would undo their decision.
			rt, err := m.runtimeFor(p.ID, p.SessionName())
			if err != nil {
				return report, err
			}
			rt.mu.Lock()
			rt.state = StateStopped
			rt.cols, rt.rows = rec.Cols, rec.Rows
			rt.started = session.Created
			rt.updated = m.now()
			rt.mu.Unlock()
			report.Stopped = append(report.Stopped, p.ID)

		case running:
			// Case A: adopt the session that is already there.
			rt, err := m.runtimeFor(p.ID, p.SessionName())
			if err != nil {
				return report, err
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

			if err := m.backend.Resize(ctx, p.SessionName(), cols, rows); err != nil {
				rt.setState(StateError, err.Error(), m.now())
				m.log.Error("could not restore the runtime size", "projectId", p.ID, "error", err)
				continue
			}
			if err := m.attach(rt); err != nil {
				rt.setState(StateError, err.Error(), m.now())
				m.log.Error("could not reattach to a surviving session", "projectId", p.ID, "error", err)
				continue
			}
			now := m.now()
			rt.setState(StateRunning, "", now)
			m.persistState(ctx, p.ID, rt, StateRunning)
			report.Running = append(report.Running, p.ID)
			m.log.Info("runtime rediscovered", "projectId", p.ID, "session", session.Name, "dir", session.Dir)

		case recorded:
			// Case B: the record exists, the session does not. Report it
			// stopped and leave starting to the user.
			rt, err := m.runtimeFor(p.ID, p.SessionName())
			if err != nil {
				return report, err
			}
			message := ""
			if rec.State == StateRunning {
				message = "the terminal session ended while the server was not running"
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
	}

	sort.Strings(report.Running)
	sort.Strings(report.Stopped)
	return report, nil
}

// Orphans returns the last reconciliation's orphaned sessions.
func (m *Manager) Orphans() []Orphan {
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

// Describe returns a project's runtime from the backend's point of view.
//
// It is a diagnostic: it answers "what does the runtime think is there",
// independently of what the database remembers or what this manager has in
// memory. The three disagreeing is exactly the situation a reconciliation
// report exists to surface.
func (m *Manager) Describe(ctx context.Context, projectID string) (*Session, error) {
	return m.backend.Inspect(ctx, project.SessionNameFor(projectID))
}

// Close releases the manager's resources without touching any session.
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
	return m.backend.Close()
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

	// Liveness is asked of the backend rather than inferred from the state.
	// The two genuinely differ: a stopped runtime keeps its session so that
	// its scrollback survives, and a runtime whose server died under it has a
	// state that is optimistic and a session that is not there.
	alive, _ := m.backend.Exists(ctx, rt.session)

	return &Runtime{
		ProjectID:    rt.projectID,
		Backend:      m.backend.Name(),
		Session:      rt.session,
		State:        snap.state,
		SessionAlive: alive,
		Cols:         snap.cols,
		Rows:         snap.rows,
		Sequence:     snap.seq,
		StartedAt:    snap.started,
		UpdatedAt:    snap.updated,
		Message:      snap.message,
	}
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
		alive, _ = m.backend.Exists(ctx, session)
	} else if !errors.Is(err, ErrRecordNotFound) {
		return nil, wrapError(err, CodeStorageFailure, "could not read the runtime record for %s", projectID)
	}

	return &Runtime{
		ProjectID:    projectID,
		Backend:      m.backend.Name(),
		Session:      session,
		State:        StateStopped,
		SessionAlive: alive,
		Cols:         cols,
		Rows:         rows,
		UpdatedAt:    m.now(),
	}, nil
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
		Backend:   m.backend.Name(),
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
		Backend:   m.backend.Name(),
		Session:   session,
		State:     StateError,
		UpdatedAt: m.now(),
	}
	if err := m.store.Save(ctx, record); err != nil {
		m.log.Error("could not record a failed runtime start", "projectId", projectID, "error", err)
	}
}
