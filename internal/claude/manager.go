package claude

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
)

// This file owns adapters.
//
// # Why something has to own them
//
// An adapter is not a value: starting one binds a socket, mints a hook path and
// leaves a goroutine serving. Something has to hold it for as long as it is
// useful and end it when it is not, and that something cannot be the runtime
// manager - the runtime's job is a terminal, and a runtime that knew what a
// hook endpoint was would be the second place process ownership is decided.
//
// So the ownership is here, one level below the thing that decides when to
// observe, and it is keyed by runtime id. A runtime is what an adapter observes:
// the Claude process inside it comes and goes, and the endpoint that watches it
// is per runtime rather than per session or per project.
//
//	internal/agent  decides when       ──▶  Manager  holds  ──▶  Adapter  observes
//
// # One adapter per runtime
//
// Attach replaces. A second Claude session in the same runtime has a name of its
// own, so the first adapter's hook path, port and settings document are all
// describing something that no longer exists; keeping it would leave a listener
// nothing will ever post to, and a client reading HookURL would be handed an
// address that answers 503. Replacing rather than reusing is what makes "the
// adapter for this runtime" a single fact.
//
// # What this deliberately does not do
//
// It does not decide anything. It does not start Claude, does not stop it, and
// does not know what a task is. It is handed a runtime id, a project id and a
// Claude session id, and it hands back an address. Every judgement about when
// those are true belongs to the caller.

// Attachment is one running adapter, described.
//
// It is a value rather than a handle: a caller is given it and cannot use it to
// reach the adapter, which is what keeps Attach the only door into an adapter's
// lifetime. SessionID is the id AgentMux dictated to Claude, and it is the key
// every hook payload will carry back.
type Attachment struct {
	// ProjectID is the AgentMux project the observed runtime belongs to.
	ProjectID string

	// RuntimeID is the AgentMux runtime being observed.
	RuntimeID string

	// AgentSessionID is the AgentMux attempt this observation belongs to, or
	// empty when the caller named no task.
	AgentSessionID string

	// SessionID is the Claude session id, chosen by AgentMux and passed to the
	// CLI, not read back from it.
	SessionID string

	// HookURL is the endpoint Claude delivers hook events to. It is filled in
	// by Attach, and it is only meaningful while the adapter is running.
	HookURL string
}

// ManagerOptions configures a Manager.
type ManagerOptions struct {
	// Adapter carries the recorder, logger, clock and injectable socket used to
	// build each adapter. Recorder and Logger are the fields that matter; the
	// rest exist so a test can attach without a port.
	Adapter AdapterOptions

	// Logger receives manager diagnostics. Nil uses the adapter's logger, which
	// is itself slog.Default when that is nil too.
	Logger *slog.Logger
}

// Manager owns one Adapter per runtime.
//
// It is safe for concurrent use.
type Manager struct {
	opts AdapterOptions
	log  *slog.Logger

	// opMu serialises Attach, Detach and Close.
	//
	// They stop and start listeners, and two of them at once would race on the
	// same map entry: the loser's adapter could be left running with nothing
	// naming it, which is a port held for the life of the process. It is
	// separate from mu because it is held across socket I/O and mu is not.
	opMu sync.Mutex

	mu          sync.Mutex
	attachments map[string]*Attachment
	adapters    map[string]*Adapter
}

// NewManager builds a Manager.
func NewManager(o ManagerOptions) *Manager {
	m := &Manager{
		opts:        o.Adapter,
		log:         o.Logger,
		attachments: make(map[string]*Attachment),
		adapters:    make(map[string]*Adapter),
	}
	if m.log == nil {
		m.log = m.opts.Logger
	}
	return m
}

// Attach starts observing a runtime and returns what it is observing.
//
// An adapter already attached to the same runtime is stopped first, so this is
// also the call that re-attaches. The registration happens after the listener
// binds and before this returns, so a caller that has an Attachment is holding
// an address that is already accepting deliveries.
//
// The configuration is validated by the adapter, which refuses a project or
// runtime it was not told. That refusal is the reason this returns an error
// rather than a zero Attachment: an adapter that guessed its own identity would
// write rows attributed to no project.
func (m *Manager) Attach(ctx context.Context, in Attachment) (Attachment, error) {
	if strings.TrimSpace(in.RuntimeID) == "" {
		return Attachment{}, newError(CodeInvalidConfig,
			"an adapter must be told which runtime it is observing")
	}

	m.opMu.Lock()
	defer m.opMu.Unlock()

	if err := m.detach(ctx, in.RuntimeID); err != nil {
		// The old adapter did not shut down cleanly. It is reported and the new
		// one still starts: refusing to observe a running session because an
		// older observer was slow to stop would leave the session unobserved
		// for a reason that has nothing to do with it.
		m.log.Warn("the previous claude adapter did not stop cleanly",
			"runtimeId", in.RuntimeID, "error", err)
	}

	adapter := NewAdapter(m.opts)
	if err := adapter.Start(ctx, Config{
		ProjectID:      in.ProjectID,
		RuntimeID:      in.RuntimeID,
		AgentSessionID: in.AgentSessionID,
	}); err != nil {
		return Attachment{}, err
	}

	att := in
	att.HookURL = adapter.HookURL()

	m.mu.Lock()
	m.attachments[in.RuntimeID] = &att
	m.adapters[in.RuntimeID] = adapter
	m.mu.Unlock()

	m.log.Info("claude adapter attached",
		"projectId", att.ProjectID,
		"runtimeId", att.RuntimeID,
		"agentSessionId", att.AgentSessionID,
		"hookUrl", att.HookURL)
	return att, nil
}

// Detach ends observation of a runtime.
//
// It is idempotent: detaching a runtime that is not attached is nothing to do,
// not a failure, because the caller asking is usually reacting to something -
// an attempt that ended, a runtime that was destroyed - and needing to know
// whether an adapter happened to still be running would make every caller
// responsible for a race it cannot see.
func (m *Manager) Detach(ctx context.Context, runtimeID string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.detach(ctx, runtimeID); err != nil {
		return err
	}
	m.log.Info("claude adapter detached", "runtimeId", runtimeID)
	return nil
}

// detach removes and stops one adapter. The caller holds opMu.
func (m *Manager) detach(ctx context.Context, runtimeID string) error {
	m.mu.Lock()
	adapter := m.adapters[runtimeID]
	delete(m.adapters, runtimeID)
	delete(m.attachments, runtimeID)
	m.mu.Unlock()

	if adapter == nil {
		return nil
	}
	return adapter.Stop(ctx)
}

// Attachment reports what is attached to a runtime.
func (m *Manager) Attachment(runtimeID string) (Attachment, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	att, ok := m.attachments[runtimeID]
	if !ok {
		return Attachment{}, false
	}
	return *att, true
}

// Attachments reports every attached runtime, ordered by runtime id.
//
// The order is by identifier rather than by time so that two calls over the
// same set return the same sequence, for the same reason Adapter.Sessions does.
func (m *Manager) Attachments() []Attachment {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Attachment, 0, len(m.attachments))
	for _, att := range m.attachments {
		out = append(out, *att)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RuntimeID < out[j].RuntimeID })
	return out
}

// HookSettings renders the settings document for an attached runtime.
//
// It delegates to the adapter's own renderer rather than rebuilding the
// document here, so that the shape Claude's settings file has to take remains
// known to exactly one file. This manager handles an address and nothing else.
func (m *Manager) HookSettings(runtimeID string) ([]byte, error) {
	m.mu.Lock()
	adapter := m.adapters[runtimeID]
	m.mu.Unlock()
	if adapter == nil {
		return nil, newError(CodeNotStarted,
			"no adapter is observing runtime %s, so it has no hooks to describe", runtimeID)
	}
	return adapter.HookSettings()
}

// Subscribe returns a channel of every observation made about a runtime.
//
// It is how a caller follows an attached session without holding the adapter.
// The channel is closed when the adapter stops or the context ends.
func (m *Manager) Subscribe(ctx context.Context, runtimeID string) (<-chan Event, error) {
	m.mu.Lock()
	adapter := m.adapters[runtimeID]
	m.mu.Unlock()
	if adapter == nil {
		return nil, newError(CodeNotStarted,
			"no adapter is observing runtime %s, so there is nothing to subscribe to", runtimeID)
	}
	return adapter.Subscribe(ctx)
}

// SessionFor reports what a Claude session id was correlated to on an attached
// runtime.
func (m *Manager) SessionFor(runtimeID, sessionID string) (Binding, bool) {
	m.mu.Lock()
	adapter := m.adapters[runtimeID]
	m.mu.Unlock()
	if adapter == nil {
		return Binding{}, false
	}
	return adapter.SessionFor(sessionID)
}

// Close ends every attachment. It is called once, when the server stops.
func (m *Manager) Close(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	runtimes := make([]string, 0, len(m.adapters))
	for runtimeID := range m.adapters {
		runtimes = append(runtimes, runtimeID)
	}
	m.mu.Unlock()

	var firstErr error
	for _, runtimeID := range runtimes {
		if err := m.detach(ctx, runtimeID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
