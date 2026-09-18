package terminal

import (
	"log/slog"
	"sync"
	"time"
)

// This file is the control plane: who is allowed to type into a terminal, and
// who is allowed to resize it.
//
// # Why a lease and not a flag
//
// Control is held for as long as somebody is using it, by a client that may
// disconnect at any moment. A boolean "this project has a controller" answers
// the question only while the answer is stale: the controller closes their
// laptop, and the flag sits there saying a terminal is owned by nobody who is
// present. A lease carries who holds it, when it was granted, and what happens
// when they stop answering - which is the difference between a fact and a
// guess.
//
// # Why the input and resize questions are separate methods
//
// They have the same answer today, and they are not the same question. A phone
// typing into a terminal drawn on a desktop is a configuration people will ask
// for, and a design where `canType == canResize` cannot express it: the phone
// would resize the pty to its own width and reflow the desktop's screen. Two
// methods that currently return the same thing cost one function each, and the
// later change is an edit to a policy rather than to every call site.
//
// # Concurrency
//
// One mutex over all of it. The critical sections are map lookups and a slice
// walk; the connections this serves number in the tens. A finer-grained design
// would be a lock ordering to get wrong in exchange for contention nobody can
// measure.

// The reasons a control decision or a control message carries.
//
// They travel on the wire because a client switches on them, and they are
// constants here so that the set is one list rather than a scattering of
// literals that drift apart from the documentation.
const (
	// ReasonAvailable means the lease was free and went to the asker.
	ReasonAvailable = "available"

	// ReasonTransferred means the previous controller handed it over.
	ReasonTransferred = "transferred"

	// ReasonResumed means the controller came back inside the grace period.
	ReasonResumed = "resumed"

	// ReasonReleased means the controller let go deliberately.
	ReasonReleased = "released"

	// ReasonExpired means a suspended lease lapsed. It is only ever carried by
	// an expiry, because it is the only thing that produces one.
	ReasonExpired = "expired"

	// ReasonControllerExists means somebody else holds the lease, so a request
	// could not be granted. It is not a refusal by a person; it is a fact.
	ReasonControllerExists = "controller_exists"

	// ReasonRejected means the controller declined a request.
	ReasonRejected = "rejected"

	// ReasonHeld means the asker already held the lease. Asking twice is not an
	// error, and it did not change anything.
	ReasonHeld = "held"

	// ReasonTooManyRequests means more clients are already waiting for this
	// project than the server will queue.
	ReasonTooManyRequests = "too_many_requests"
)

// MaxPendingRequests bounds how many clients may be waiting for one project.
//
// A pending request is one map entry and one line in a roster sent to every
// viewer, so without a bound a client could ask to be queued in a loop and make
// every other viewer's roster grow. Eight is far more than the number of people
// who will ever ask for one terminal at once.
const MaxPendingRequests = 8

// Decision is the authority's answer to "may this client do this".
//
// It carries the code and the message rather than a bare boolean because the
// authority is what knows *why* it refused, and a caller that had to reconstruct
// the reason would be reconstructing it from a rule that lives here. The zero
// value is a refusal with nothing to say, which is the safe direction to fail.
type Decision struct {
	Allowed bool
	Code    string
	Message string
}

// allow is the decision for a client that holds the lease.
func allow() Decision { return Decision{Allowed: true} }

// PendingRequest is a client waiting for a project's lease.
type PendingRequest struct {
	ClientID string
	Device   string
	Since    time.Time
}

// Lease is a snapshot of who holds a project's control.
//
// It is a copy rather than a pointer into the authority's state, so that a
// caller building a roster cannot read it while it changes underneath. The zero
// value means "nobody holds this", which is also what a project that has never
// had a controller reports - a distinction nothing needs to draw.
type Lease struct {
	ControllerID string
	ConnectionID string
	Device       string
	GrantedAt    time.Time
	Suspended    bool
	ExpiresAt    time.Time
	Pending      []PendingRequest
}

// Holder reports whether anybody - present or suspended - holds the lease.
func (l Lease) Holder() bool { return l.ControllerID != "" }

// RequestOutcome is what became of a request for control.
type RequestOutcome int

const (
	// RequestGranted means the asker now holds the lease.
	RequestGranted RequestOutcome = iota

	// RequestHeld means the asker already held it. Asking twice is not an error,
	// and nothing changed - so the caller answers the asker and tells nobody
	// else.
	RequestHeld

	// RequestQueued means a controller holds the lease and has been told that
	// somebody is asking. Nothing has been transferred.
	RequestQueued

	// RequestDenied means the lease is held by a controller that is not the
	// asker and cannot be asked - either it is suspended, so there is a
	// controller record but no live client to answer, or the queue is full.
	RequestDenied
)

// TransferOutcome is what became of an accept or a reject.
type TransferOutcome int

const (
	// TransferDone means the lease moved.
	TransferDone TransferOutcome = iota

	// TransferNotController means the sender does not hold the lease, so it has
	// nothing to hand over.
	TransferNotController

	// TransferNotPending means the named client has not asked for control.
	TransferNotPending
)

// AuthorityOptions configures an Authority.
type AuthorityOptions struct {
	// Grace is how long a disconnected controller's lease is held for it. Zero
	// means DefaultControlGrace. A test sets it short so that the expiry path
	// can be watched rather than waited for.
	Grace time.Duration

	// Now supplies the current time. Nil means time.Now.
	Now func() time.Time

	// Logger receives control records. Nil means slog.Default.
	//
	// What may be logged is a short list: project id, client id, and the event.
	// Terminal bytes are never logged, in either direction, at any level - and
	// that includes the input this file refuses.
	Logger *slog.Logger

	// Expired is called when a suspended lease lapses, with the project it
	// belonged to. It is called without the authority's lock held, from the
	// timer's own goroutine.
	//
	// It is a callback rather than something the authority does itself because
	// releasing a lease is a fact and telling everybody is a broadcast, and
	// broadcasts belong to the thing that owns the connections.
	Expired func(projectID string)
}

// DefaultControlGrace is how long a controller's lease survives a disconnect.
//
// Thirty seconds is long enough for a phone to move between networks, a laptop
// to be reopened, or a page to be reloaded, and short enough that somebody else
// can take over without going to find a person. It is the one number in this
// design that is a judgement about people rather than about machines.
const DefaultControlGrace = 30 * time.Second

// NewAuthority builds an authority.
func NewAuthority(opts AuthorityOptions) *Authority {
	a := &Authority{
		grace:   opts.Grace,
		now:     opts.Now,
		log:     opts.Logger,
		expired: opts.Expired,
		project: make(map[string]*controlState),
		client:  make(map[string]*clientPresence),
	}
	if a.grace <= 0 {
		a.grace = DefaultControlGrace
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	return a
}

// clientPresence is the connections one client currently has open.
//
// It exists so that "the client left" can be told apart from "one of the
// client's sockets closed". A browser reconnecting opens the new socket before
// the old one is reaped, and a design that suspended a lease on every socket
// close would suspend it on every reconnect - which is the case the grace
// period exists to make invisible.
type clientPresence struct {
	conns map[string]struct{}
}

// controlState is one project's lease and the requests against it.
type controlState struct {
	controllerID string
	connectionID string
	device       string
	grantedAt    time.Time

	// suspended and expiresAt are meaningful only while controllerID is set.
	suspended bool
	expiresAt time.Time

	// generation counts grants to this project. A grace timer captures the
	// generation it was started for and does nothing if it has moved on, which
	// is what stops a timer started before a reconnect from releasing the lease
	// that reconnect restored.
	generation uint64
	timer      *time.Timer

	pending map[string]PendingRequest
}

// Authority owns every project's lease.
//
// It is the only thing in the server that decides whether a client may type or
// resize, and it is deliberately not a WebSocket handler: a rule enforced at
// the point of use is a rule that has to be remembered at every point of use,
// and the second place somebody adds an input path is the place it is forgotten.
type Authority struct {
	grace   time.Duration
	now     func() time.Time
	log     *slog.Logger
	expired func(string)

	mu      sync.Mutex
	project map[string]*controlState
	client  map[string]*clientPresence
}

// ---------------------------------------------------------------------------
// Presence

// Attach records that a client has a connection open, and reports the projects
// whose leases it resumed by doing so.
//
// The returned list is what a reconnect is: the client's leases were suspended
// rather than released, and this is the moment they come back. The caller
// broadcasts for each one, because every other viewer's idea of who is in
// charge has been wrong since the disconnect.
func (a *Authority) Attach(clientID, connectionID, device string) []string {
	if clientID == "" || connectionID == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	presence := a.client[clientID]
	if presence == nil {
		presence = &clientPresence{conns: make(map[string]struct{})}
		a.client[clientID] = presence
	}
	presence.conns[connectionID] = struct{}{}

	var resumed []string
	for projectID, state := range a.project {
		if state.controllerID != clientID || !state.suspended {
			continue
		}
		if state.timer != nil {
			state.timer.Stop()
			state.timer = nil
		}
		state.suspended = false
		state.expiresAt = time.Time{}
		state.connectionID = connectionID
		state.generation++
		resumed = append(resumed, projectID)
	}
	if len(resumed) > 0 {
		// The list is sorted so that two runs of the same scenario return the
		// same order. Map iteration is not, and a caller that reports the
		// projects it resumed would otherwise report them differently each time.
		sortStrings(resumed)
		a.log.Info("control resumed after reconnect", "clientId", clientID)
	}
	return resumed
}

// Detach records that a client's connection closed, suspending its leases if
// that was the last one.
//
// It reports the projects it suspended, which is the caller's cue that every
// viewer's roster is now out of date.
func (a *Authority) Detach(clientID, connectionID string) []string {
	if clientID == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	presence := a.client[clientID]
	if presence == nil {
		return nil
	}
	delete(presence.conns, connectionID)
	if len(presence.conns) > 0 {
		// Another socket is still open and still this client. Nothing has
		// happened as far as any lease is concerned.
		return nil
	}
	delete(a.client, clientID)

	var suspended []string
	for projectID, state := range a.project {
		if state.controllerID != clientID || state.suspended {
			continue
		}
		a.suspendLocked(projectID, state, clientID)
		suspended = append(suspended, projectID)
	}
	if len(suspended) > 0 {
		sortStrings(suspended)
		a.log.Info("control suspended while the controller is away", "clientId", clientID)
	}
	a.pruneLocked()
	return suspended
}

// expire releases a suspended lease whose grace period ran out.
func (a *Authority) expire(projectID, clientID string, generation uint64) {
	a.mu.Lock()
	state := a.project[projectID]
	if state == nil || state.controllerID != clientID ||
		!state.suspended || state.generation != generation {
		// Either the lease moved on - the controller came back, or somebody
		// else has it - or it is already gone. A timer that outlived its lease
		// must not release the one that replaced it.
		a.mu.Unlock()
		return
	}
	state.timer = nil
	a.clearLocked(state)
	a.pruneLocked()
	a.mu.Unlock()

	a.log.Info("control expired", "projectId", projectID, "clientId", clientID)
	if a.expired != nil {
		a.expired(projectID)
	}
}

// Close stops every grace timer.
//
// It exists for tests and for a server shutting down. A timer that outlives the
// authority would call back into a hub that no longer has connections.
func (a *Authority) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, state := range a.project {
		if state.timer != nil {
			state.timer.Stop()
			state.timer = nil
		}
	}
}

// ---------------------------------------------------------------------------
// Authority

// MayInput reports whether a client may write to a project's terminal.
func (a *Authority) MayInput(projectID, clientID string) Decision {
	return a.may(projectID, clientID,
		CodeNotController,
		"you are watching this project; request control to type into it")
}

// MayResize reports whether a client may set a project's terminal size.
//
// It is a separate question from MayInput on purpose; see the file comment.
func (a *Authority) MayResize(projectID, clientID string) Decision {
	return a.may(projectID, clientID,
		CodeNotController,
		"you are watching this project; only the controller can resize its terminal")
}

// may is the one rule both questions are answered by today.
//
// It is a single function rather than two copies because the two must not drift
// while they are the same answer; the day they differ, this is the function
// that gets split.
func (a *Authority) may(projectID, clientID string, code, message string) Decision {
	a.mu.Lock()
	defer a.mu.Unlock()

	state := a.project[projectID]
	if state == nil || state.controllerID == "" {
		// Nobody holds this project's lease. That is a refusal: control is
		// asked for and granted, never assumed. A client that has not asked is
		// a viewer whatever else is true.
		return Decision{Code: code, Message: message}
	}
	if state.controllerID != clientID {
		return Decision{Code: code, Message: message}
	}
	if state.suspended {
		// The controller is this client, but its connection is gone - so this
		// request cannot be coming from it. It is a forged or stale identifier,
		// and the answer is the same as for a stranger.
		return Decision{Code: code, Message: message}
	}
	return allow()
}

// ---------------------------------------------------------------------------
// Lease lifecycle

// Request asks for a project's lease on behalf of a client.
//
// It reports the outcome and the reason to put on the wire. The reason is
// returned rather than derived by the caller because the authority is what knows
// which branch it took, and a caller re-deriving it from the outcome would be
// writing a second copy of these rules.
//
// A refusal here is never a refusal by a person. Nobody is asked; the answer is
// either "it is free" or "somebody has it", and transferring it is a separate
// act that only the current controller can perform - see Accept.
func (a *Authority) Request(projectID, clientID, connectionID, device string) (RequestOutcome, string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	state := a.project[projectID]
	if state == nil {
		state = &controlState{pending: make(map[string]PendingRequest)}
		a.project[projectID] = state
	}

	if state.controllerID == clientID && !state.suspended {
		// Already the controller. Asking twice is not an error, and it must not
		// be answered with a second grant: the client is re-affirming, and the
		// roster it already has is the answer.
		delete(state.pending, clientID)
		return RequestHeld, ReasonHeld
	}

	if state.controllerID == "" {
		a.grantLocked(projectID, state, clientID, connectionID, device)
		a.log.Info("control granted", "projectId", projectID, "clientId", clientID)
		return RequestGranted, ReasonAvailable
	}

	if state.suspended {
		// A suspended controller is still the controller. Its client is very
		// likely coming back - that is what the grace period is for - and
		// handing the terminal to the first asker would make the grace period
		// mean nothing. The asker is told the truth: somebody has it.
		return RequestDenied, ReasonControllerExists
	}

	if _, asked := state.pending[clientID]; asked {
		return RequestQueued, ""
	}
	if len(state.pending) >= MaxPendingRequests {
		// The queue is full. The request is not recorded, so the asker is not
		// left believing it is waiting for an answer that will never come.
		return RequestDenied, ReasonTooManyRequests
	}
	state.pending[clientID] = PendingRequest{
		ClientID: clientID, Device: device, Since: a.now(),
	}
	a.log.Info("control requested", "projectId", projectID, "clientId", clientID)
	return RequestQueued, ""
}

// Release gives up a lease deliberately.
//
// It reports whether the client held it. A project that is released is left with
// no controller and no queue: see clearLocked for why nobody waiting is promoted.
// Releasing is not disconnecting: it takes effect at once and there is no grace
// period, because the client that released is still there to be told so.
func (a *Authority) Release(projectID, clientID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	state := a.project[projectID]
	if state == nil || state.controllerID != clientID || state.suspended {
		return false
	}
	a.clearLocked(state)
	// A release is the ordinary way a project stops being controlled, and it
	// leaves no controller - so this is the path that has to prune, or the map
	// keeps an entry for every project anybody ever took and gave back.
	a.pruneLocked()
	a.log.Info("control released", "projectId", projectID, "clientId", clientID)
	return true
}

// Accept hands a project's lease from its controller to a client that asked.
//
// The new holder's connection is looked up rather than passed in: the controller
// performing the transfer has no idea what socket the other client is on, and a
// caller that had to name one would be guessing. The connection recorded on a
// lease is informational - what a lease is held by is a client.
func (a *Authority) Accept(projectID, controllerID, targetID string) TransferOutcome {
	a.mu.Lock()
	defer a.mu.Unlock()

	state := a.project[projectID]
	if state == nil || state.controllerID != controllerID || state.suspended {
		return TransferNotController
	}
	request, asked := state.pending[targetID]
	if !asked {
		return TransferNotPending
	}
	a.grantLocked(projectID, state, targetID, a.connectionLocked(targetID), request.Device)
	a.log.Info("control transferred", "projectId", projectID, "clientId", controllerID)
	return TransferDone
}

// connectionLocked picks one of a client's open connections.
//
// Any of them will do - the field is a record, not a route - but the choice is
// made deterministically rather than by map order, so that two runs of the same
// scenario produce the same lease and a test can assert on it.
func (a *Authority) connectionLocked(clientID string) string {
	presence := a.client[clientID]
	if presence == nil || len(presence.conns) == 0 {
		return ""
	}
	ids := make([]string, 0, len(presence.conns))
	for id := range presence.conns {
		ids = append(ids, id)
	}
	sortStrings(ids)
	return ids[0]
}

// Reject declines a pending request.
func (a *Authority) Reject(projectID, controllerID, targetID string) TransferOutcome {
	a.mu.Lock()
	defer a.mu.Unlock()

	state := a.project[projectID]
	if state == nil || state.controllerID != controllerID || state.suspended {
		return TransferNotController
	}
	if _, asked := state.pending[targetID]; !asked {
		return TransferNotPending
	}
	delete(state.pending, targetID)
	a.log.Info("control request refused", "projectId", projectID, "clientId", controllerID)
	return TransferDone
}

// Withdraw removes a client's pending request.
//
// It is what an unsubscribe does: a client that stops watching a project is no
// longer waiting for its terminal, and leaving the request behind would put a
// name in every other viewer's roster for a client that has gone.
func (a *Authority) Withdraw(projectID, clientID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.project[projectID]
	if state == nil {
		return
	}
	delete(state.pending, clientID)
	a.pruneLocked()
}

// Lease returns a snapshot of a project's control state.
func (a *Authority) Lease(projectID string) Lease {
	a.mu.Lock()
	defer a.mu.Unlock()

	state := a.project[projectID]
	if state == nil {
		return Lease{}
	}
	lease := Lease{
		ControllerID: state.controllerID,
		ConnectionID: state.connectionID,
		Device:       state.device,
		GrantedAt:    state.grantedAt,
		Suspended:    state.suspended,
		ExpiresAt:    state.expiresAt,
	}
	for _, request := range state.pending {
		lease.Pending = append(lease.Pending, request)
	}
	sortPending(lease.Pending)
	return lease
}

// ---------------------------------------------------------------------------
// Internals

// grantLocked gives a project's lease to a client and clears the queue.
//
// Every pending request is cleared, not just the one that was accepted: the
// question they were all waiting on - "who gets this terminal" - has just been
// answered, and leaving the others queued would show a controller a request
// from somebody whose turn has passed.
func (a *Authority) grantLocked(projectID string, state *controlState, clientID, connectionID, device string) {
	if state.timer != nil {
		state.timer.Stop()
		state.timer = nil
	}
	state.controllerID = clientID
	state.connectionID = connectionID
	state.device = device
	state.grantedAt = a.now()
	state.suspended = false
	state.expiresAt = time.Time{}
	state.generation++
	clear(state.pending)

	if connectionID == "" {
		// The new holder has no connection. That happens through a transfer to
		// a client that asked and then closed its browser: the request is still
		// in the queue and the controller may well accept it without knowing.
		//
		// The grant stands - the client asked, and it can come back to it - but
		// the lease is suspended from the outset so that a grace timer is
		// already running. Without this the lease would be live, held by a
		// client nobody can reach, and carrying no timer at all: the project
		// would be locked out for good rather than for thirty seconds.
		a.suspendLocked(projectID, state, clientID)
	}
}

// suspendLocked holds a lease for an absent client and arms the timer that
// releases it if the client does not come back.
//
// The client is passed in rather than read from the state at fire time: the
// timer runs long after the state may have moved on, and it has to name the
// client it was started for to be able to tell that it is no longer relevant.
func (a *Authority) suspendLocked(projectID string, state *controlState, clientID string) {
	state.suspended = true
	state.expiresAt = a.now().Add(a.grace)
	state.generation++
	generation := state.generation
	state.timer = time.AfterFunc(a.grace, func() {
		a.expire(projectID, clientID, generation)
	})
}

// clearLocked removes a project's controller without granting it to anybody.
func (a *Authority) clearLocked(state *controlState) {
	if state.timer != nil {
		state.timer.Stop()
		state.timer = nil
	}
	state.controllerID = ""
	state.connectionID = ""
	state.device = ""
	state.grantedAt = time.Time{}
	state.suspended = false
	state.expiresAt = time.Time{}
	state.generation++

	// The queue goes with the controller, because a pending request is a question
	// put to whoever is holding the lease and this leaves nobody holding it. An
	// entry left behind would be a client shown as waiting for a decision that
	// nobody is in a position to make, in a roster that says the project is free.
	//
	// Nobody is promoted by it either. Stepping back is not the same as choosing
	// a successor - that is what Accept is - and promoting the first in line
	// would make releasing a way to hand somebody the keyboard without saying so.
	// The project is free, and it is free for whoever asks next.
	clear(state.pending)
}

// pruneLocked drops a project's entry once nothing refers to it.
//
// Without this the map would grow by one entry per project ever controlled and
// never shrink. The condition is deliberately conservative - no controller, no
// pending requests - because an entry that still holds either is an entry some
// client is relying on.
func (a *Authority) pruneLocked() {
	for projectID, state := range a.project {
		if state.controllerID == "" && len(state.pending) == 0 {
			if state.timer != nil {
				state.timer.Stop()
			}
			delete(a.project, projectID)
		}
	}
}

// sortStrings sorts ascending. It is insertion sort, because the lists here are
// the number of projects one browser holds control of.
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// sortPending orders pending requests so that a roster reads the same way twice.
//
// Map iteration is random, and a list that reorders itself on every broadcast
// is a list that makes a UI flicker and a test flaky.
func sortPending(requests []PendingRequest) {
	for i := 1; i < len(requests); i++ {
		for j := i; j > 0 && requests[j].Since.Before(requests[j-1].Since); j-- {
			requests[j], requests[j-1] = requests[j-1], requests[j]
		}
	}
}
