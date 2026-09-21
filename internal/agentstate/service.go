package agentstate

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/task"
)

// Service projects the event log into agent state.
//
// It implements event.Projector, and it is the only thing in the build that
// turns a fact about the past into a reading of the present.
//
// # How it is driven
//
// The event service calls Project once per stored event. Nothing else calls it:
// the adapter does not know this package exists, and there is no path from an
// observer of Claude to a state row that skips the event log. That is §10 of
// the phase brief, and it is also the only arrangement in which the states can
// be rebuilt - a projection that could be written to directly would be a
// projection whose contents the log could not reproduce.
//
// # Concurrency
//
// Nothing here locks around a projection. Two events arriving at once are two
// conditional writes, and the condition is the event's own timestamp, so the
// later one wins and re-applying the earlier one afterwards changes nothing.
// The database is the only coordination this needs, which is what §13 of the
// brief asked for and is simpler than any lock that would achieve less.
type Service struct {
	repo     Repository
	events   EventReader
	projects ProjectLister
	log      *slog.Logger
	now      func() time.Time

	// rebuildMu serialises rebuilds against each other. It is not held by
	// Project, and a projection running while a rebuild is in flight is
	// harmless: both write through the same conditional upsert, and the rebuild
	// simply writes what the projection already wrote.
	rebuildMu sync.Mutex
}

// EventReader is the event log as this package reads it.
//
// It is a consumer-declared interface, so the event service does not know this
// package exists in either direction: it declares the Projector it will call,
// and this declares the reader it will use.
type EventReader interface {
	ListProject(ctx context.Context, projectID string, opts event.ListOptions) (event.Page, error)
}

// ProjectLister is the project model as this package reads it.
//
// A rebuild has to visit every project, because the event log refuses an
// unscoped read - deliberately, so that "every event AgentMux has ever
// recorded" is never one query's answer. Reading a project at a time is the
// cost of that, and it is small: a runtime belongs to exactly one project, so
// the fold never needs to see two at once.
type ProjectLister interface {
	List(ctx context.Context, filter project.ListFilter) ([]*project.Project, error)
}

// Options configures a Service. Repository is required.
type Options struct {
	// Repository is where projected states are kept. Required.
	Repository Repository

	// Events is the event log, read by a rebuild. Without it this service
	// projects live events and cannot recompute from history.
	Events EventReader

	// Projects is the project model, read by a rebuild for the same reason.
	Projects ProjectLister

	// Logger receives diagnostics. Nil means slog.Default.
	Logger *slog.Logger

	// Now supplies the current time. Nil means time.Now.
	//
	// It is the clock the row is stamped with, and deliberately not the clock
	// the projection *decides* by: which event wins is settled by the event's
	// own time, so a state computed on one machine is the state any other
	// machine computes from the same log.
	Now func() time.Time
}

// NewService builds a Service.
func NewService(o Options) (*Service, error) {
	if o.Repository == nil {
		return nil, newError(CodeInvalidState, "agentstate: a repository is required")
	}
	s := &Service{
		repo:     o.Repository,
		events:   o.Events,
		projects: o.Projects,
		log:      o.Logger,
		now:      o.Now,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Project folds one event into the state it is about.
//
// It is what event.Service calls after an event is stored, and it never fails
// an event: by the time it runs the fact is already recorded, and a state that
// could not be derived does not un-happen the thing it was derived from. An
// error here is reported to the caller so that it can be logged, and the event
// stands either way.
//
// # Why the caller's cancellation is ignored
//
// The context is stripped of cancellation before anything is written. Every
// event is written on the context of whatever produced it - an HTTP request, a
// hook delivery, a process launch - and those end: a client hangs up, a timeout
// fires. If that cancellation reached this far it would abandon a projection of
// an event that had already been stored, and the state would simply be wrong
// until something else happened to the same attempt. The last event of an
// attempt is exactly the one that would be lost, and exactly the one that
// matters most.
//
// It is safe to be uncancellable here because there is nothing to wait for: the
// projection is one bounded local write, and the database's own busy timeout
// bounds it further. The deadline that is dropped is the request's, which the
// event has already outlived.
//
// # What it ignores, and why that is not a gap
//
// An event that neither names an attempt nor is one this package reads is not
// projected, and there are three kinds of those. `runtime.*` and `task.*` are
// about a terminal and about what somebody wants done, neither of which is the
// agent's state. An `agent.*` event with no attempt bound to its runtime is an
// agent AgentMux did not start, or one started without a task - there is no
// attempt for it to be the state of. docs/AGENT_STATE.md §6.
func (s *Service) Project(ctx context.Context, ev *event.AgentEvent) error {
	if ev == nil {
		return nil
	}

	// See the note above. This is the whole of what makes the state survive the
	// request that produced the event.
	ctx = context.WithoutCancel(ctx)

	if status, ok := statusFor(ev.Type); ok {
		return s.projectAgentEvent(ctx, ev, status)
	}
	if isSessionEvent(ev.Type) {
		return s.projectSessionEvent(ctx, ev)
	}
	return nil
}

// isSessionEvent reports whether an event is one an attempt's own lifecycle
// produces.
//
// Only two are read, and the list is short on purpose. `session.created` is
// what puts an attempt into the projection at all, and `session.status_changed`
// is the only event in the build that carries both an attempt and a runtime -
// which makes it both the binding and the attempt's own outcome.
func isSessionEvent(eventType string) bool {
	return eventType == task.TypeSessionCreated ||
		eventType == task.TypeSessionStatusChanged
}

// projectSessionEvent folds an attempt's own lifecycle event.
//
// These do two jobs. They carry the attempt id, which is what creates the row
// and what binds a runtime to it; and they carry the attempt's status, which is
// the same fact the agent's state is about - an attempt that was cancelled is
// an agent that stopped.
func (s *Service) projectSessionEvent(ctx context.Context, ev *event.AgentEvent) error {
	payload, ok := sessionPayloadOf(ev.Payload)
	if !ok {
		// A session event with no attempt named is not projectable. It is not
		// an error either: the event is stored, and a rebuild meeting the same
		// payload will make the same choice.
		return nil
	}

	status := StatusCreated
	if ev.Type == task.TypeSessionStatusChanged {
		mapped, ok := statusForSession(payload.To)
		if ok {
			status = mapped
		} else {
			// A status this build does not know. It is unreachable from the
			// task service, which validates a transition before it emits one -
			// and it is handled rather than skipped because skipping would drop
			// the binding with it: the runtime would stop finding its attempt,
			// and every agent event after that would go unprojected.
			//
			// What it holds is the status the row already has, which is what an
			// event that asserts no status does. A row that does not exist yet
			// has no status to hold, so there is nothing to write and the event
			// is reported instead.
			current, err := s.repo.Get(ctx, payload.AgentSession)
			if err != nil {
				if IsCode(err, CodeNotFound) {
					s.log.Warn("a session event carries a status this build does not know",
						"eventId", ev.ID, "to", payload.To,
						"agentSessionId", payload.AgentSession)
					return nil
				}
				return err
			}
			status = current.Status
		}
	}

	return s.write(ctx, AgentState{
		AgentSessionID: payload.AgentSession,
		ProjectID:      ev.ProjectID,
		// Empty for `session.created`, which happens before a runtime is
		// attached. The store keeps whatever a previous event already bound.
		RuntimeID:   ev.RuntimeID,
		Status:      status,
		LastEvent:   ev.Type,
		LastEventAt: ev.CreatedAt,
		UpdatedAt:   s.now().UTC(),
	})
}

// projectAgentEvent folds something Claude reported.
//
// An `agent.*` event names a project and a runtime and no attempt. The attempt
// is the one currently bound to that runtime, which the log established when
// the attempt's own lifecycle events arrived.
func (s *Service) projectAgentEvent(ctx context.Context, ev *event.AgentEvent, status Status) error {
	if strings.TrimSpace(ev.RuntimeID) == "" {
		return nil
	}

	current, err := s.repo.ByRuntime(ctx, ev.RuntimeID)
	if err != nil {
		if IsCode(err, CodeNotFound) {
			// Nothing is bound to this runtime, so there is no attempt for the
			// event to be the state of. This is the ordinary case for an agent
			// started without a task, and it is not an error.
			s.log.Debug("an agent event names a runtime no attempt is bound to",
				"eventId", ev.ID, "type", ev.Type, "runtimeId", ev.RuntimeID)
			return nil
		}
		return err
	}

	// An event that asserts no status - `agent.completed_candidate` - is
	// recorded without moving the state. The row then says what the last thing
	// seen was and what the state still is, which are two different facts and
	// both true.
	next := current.Status
	if status != "" {
		next = status
	}

	return s.write(ctx, AgentState{
		AgentSessionID: current.AgentSessionID,
		ProjectID:      current.ProjectID,
		RuntimeID:      ev.RuntimeID,
		Status:         next,
		LastEvent:      ev.Type,
		LastEventAt:    ev.CreatedAt,
		UpdatedAt:      s.now().UTC(),
	})
}

// write validates and stores a projected state.
func (s *Service) write(ctx context.Context, state AgentState) error {
	if err := state.Validate(); err != nil {
		return err
	}
	written, err := s.repo.Upsert(ctx, state)
	if err != nil {
		return err
	}
	if written {
		s.log.Debug("agent state projected",
			"agentSessionId", state.AgentSessionID,
			"status", state.Status,
			"lastEvent", state.LastEvent)
	}
	return nil
}

// State returns one attempt's state.
func (s *Service) State(ctx context.Context, agentSessionID string) (AgentState, error) {
	id := strings.TrimSpace(agentSessionID)
	if id == "" {
		return AgentState{}, newError(CodeInvalidState, "an agent state must be asked for by attempt").
			withDetail("field", "agentSessionId")
	}
	return s.repo.Get(ctx, id)
}

// StateByRuntime returns the state of the attempt currently bound to a runtime.
//
// It is how a package that holds an event naming only a runtime - an `agent.*`
// event does - finds the attempt it is about. internal/attention is the caller,
// and it reads this rather than repeating the binding rule: the log establishes
// the binding once, in one projection, and a second place that worked it out
// would be a second place for it to be wrong.
//
// It returns an error carrying CodeNotFound when no attempt is bound, which is
// what an agent started without a task produces.
func (s *Service) StateByRuntime(ctx context.Context, runtimeID string) (AgentState, error) {
	id := strings.TrimSpace(runtimeID)
	if id == "" {
		return AgentState{}, newError(CodeInvalidState,
			"an agent state must be asked for by runtime").withDetail("field", "runtimeId")
	}
	return s.repo.ByRuntime(ctx, id)
}

// ListByProject returns a project's agent states, most recently updated first.
func (s *Service) ListByProject(ctx context.Context, projectID string, limit int) ([]AgentState, error) {
	id := strings.TrimSpace(projectID)
	if id == "" {
		return nil, newError(CodeInvalidState, "agent states must be listed for a project").
			withDetail("field", "projectId")
	}
	return s.repo.ListByProject(ctx, id, LimitOr(limit))
}

// RebuildReport says what a rebuild did.
type RebuildReport struct {
	// Projects is how many projects were visited.
	Projects int

	// Events is how many events were read.
	Events int

	// States is how many rows the projection holds afterwards.
	States int
}

// RebuildIfNeeded rebuilds when the projection is empty, and reports whether it
// ran.
//
// It is what a server calls at start-up. An empty projection over a log that
// already has events in it is what a first run after this phase leaves behind -
// the table is new and nothing has ever projected into it - and it is the only
// condition under which the server recomputes by itself.
//
// A projection that already holds something is left exactly as it is. A server
// that rebuilt on every start would pay the whole history on every start, and
// there would be nothing to repair: the live projection is kept current as
// events are written, so a non-empty table is a current one.
//
// The empty case recurs on an installation whose log holds no agent or session
// events at all, because such a log rebuilds to nothing. That costs a handful of
// queries at start-up and is left alone rather than guessed at with a second
// condition that would have to be right.
func (s *Service) RebuildIfNeeded(ctx context.Context) (RebuildReport, bool, error) {
	count, err := s.repo.Count(ctx)
	if err != nil {
		return RebuildReport{}, false, err
	}
	if count > 0 {
		return RebuildReport{}, false, nil
	}
	report, err := s.Rebuild(ctx)
	return report, true, err
}

// rebuildPageSize is how many events one page of a rebuild reads.
//
// It is the event service's own default page, restated here so that a rebuild
// does not have to reason about a limit it did not choose. The value is not a
// contract; it only decides how many round trips a long log takes.
const rebuildPageSize = 200

// Rebuild recomputes every state from the event log.
//
// # What it is for
//
// The projection is derived, so it can always be thrown away and recomputed.
// That is what makes it safe to add this table to an installation that already
// has a history - the first start after this phase rebuilds and the states
// appear - and it is the recovery path for a projection that was interrupted,
// or for rows written by a version of the table that computed them differently.
//
// It is also the test of the design: a projection whose rebuild produced
// something other than its live writes would be a projection that had started
// depending on something other than the log.
//
// # What it does
//
// It reads every project's timeline, then empties the table and folds what it
// read back in, oldest event first. It does not touch `agent_events`, does not
// write an event and does not read anything but the log and the project list.
//
// **The read happens before the clear, and the order is the point.** Clearing
// first would mean that a log which could not be read left an empty projection
// behind - a server reporting no agent states at all because a rebuild failed,
// when the table was correct a moment earlier. Reading first means a failure
// leaves the projection exactly as it was, which is the only outcome that is
// not worse than not having tried.
//
// The fold itself cannot fail it: an event that will not project is reported
// and skipped, for the reason projectAgentEvent gives.
//
// It is not cheap and it is not meant to be run in a request. A rebuild reads
// the whole history into memory; the cost is the price of the guarantee, and it
// is paid at start-up rather than per read.
func (s *Service) Rebuild(ctx context.Context) (RebuildReport, error) {
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()

	if s.events == nil || s.projects == nil {
		return RebuildReport{}, newError(CodeProjectionFailed,
			"agentstate: a rebuild needs the event log and the project list")
	}

	projects, err := s.projects.List(ctx, project.ListFilter{})
	if err != nil {
		return RebuildReport{}, wrapError(err, CodeStorageFailure,
			"could not list the projects to rebuild agent states from")
	}

	// Read everything first. Nothing is cleared until the whole log has been
	// read, so a log that cannot be read cannot empty the projection.
	var report RebuildReport
	report.Projects = len(projects)
	logs := make([][]*event.AgentEvent, 0, len(projects))
	for _, p := range projects {
		events, err := s.allEvents(ctx, p.ID)
		if err != nil {
			return RebuildReport{}, err
		}
		logs = append(logs, events)
		report.Events += len(events)
	}

	if err := s.repo.DeleteAll(ctx); err != nil {
		return RebuildReport{}, err
	}

	for _, events := range logs {
		for _, ev := range events {
			if err := s.Project(ctx, ev); err != nil {
				// One event that cannot be folded is reported and skipped
				// rather than ending the rebuild. The projection is a reading;
				// an event it cannot read leaves the row where the last
				// readable event put it, which is the same answer a live
				// projection gives.
				s.log.Warn("could not project an event during a rebuild",
					"eventId", ev.ID, "type", ev.Type, "error", err)
			}
		}
	}

	// The count is of the whole table rather than of a listing, so it is not
	// bounded by how many rows a page happens to hold.
	states, err := s.repo.Count(ctx)
	if err != nil {
		return report, err
	}
	report.States = states

	// Reported at debug when there was nothing to fold. An installation with no
	// agent activity rebuilds to nothing on every start, because the emptiness
	// that triggers a rebuild is also the emptiness it produces, and a line
	// saying "rebuilt" each time would describe work that did not happen.
	if report.Events == 0 {
		s.log.Debug("agent states rebuilt from an empty log",
			"projects", report.Projects, "states", report.States)
	} else {
		s.log.Info("agent states rebuilt",
			"projects", report.Projects, "events", report.Events, "states", report.States)
	}
	return report, nil
}

// allEvents reads a project's timeline in the order it happened.
//
// The store returns newest first, because that is what a reader of a timeline
// wants. A projection wants the opposite, so the pages are collected and
// reversed. Holding a project's history in memory is the cost of a rebuild and
// is bounded by the history itself - which is why this runs at start-up and
// never in a request.
func (s *Service) allEvents(ctx context.Context, projectID string) ([]*event.AgentEvent, error) {
	var collected []*event.AgentEvent
	opts := event.ListOptions{Limit: rebuildPageSize}

	for {
		page, err := s.events.ListProject(ctx, projectID, opts)
		if err != nil {
			return nil, wrapError(err, CodeStorageFailure,
				"could not read the timeline of project %s to rebuild its agent states", projectID).
				withDetail("projectId", projectID)
		}
		collected = append(collected, page.Events...)
		if page.NextBefore == "" {
			break
		}
		opts.Before = page.NextBefore
	}

	// Newest first to oldest first.
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return collected, nil
}
