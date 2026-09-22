package attention

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/kutonlagos/agentmux/internal/agentstate"
	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/task"
)

// Service projects the event log into attention and a queue of actions.
//
// It implements event.Projector, and it runs after the agent state projection -
// which is a real dependency rather than a preference, because an `agent.*`
// event names a runtime and no attempt and the state is what says which attempt
// a runtime belongs to.
//
// # What it is driven by, and what it must not be
//
// The event service calls Project once per stored event. Nothing else calls it:
// there is no path from an observer of Claude to a row here, and no path from a
// person's click either - this phase defines no way to answer an action, and
// docs/AGENT_ATTENTION.md §6 is why.
//
// # Concurrency
//
// Nothing here locks around a projection, for the reason internal/agentstate
// gives: the attention write is one conditional statement and the action write
// is an idempotent insert, so the database is the only coordination either
// needs.
type Service struct {
	repo     Repository
	states   StateReader
	events   EventReader
	projects ProjectLister
	log      *slog.Logger

	// rebuildMu serialises rebuilds against each other. It is not held by
	// Project, and a projection running while a rebuild is in flight is
	// harmless: both write through the same conditional and idempotent writes.
	rebuildMu sync.Mutex
}

// StateReader is the agent state projection as this package reads it.
//
// It is a consumer-declared interface, so neither projection knows the other
// exists: this one states what it needs, and the other satisfies it. What this
// needs is one thing - given a runtime, which attempt is running in it.
type StateReader interface {
	StateByRuntime(ctx context.Context, runtimeID string) (agentstate.AgentState, error)
}

// EventReader is the event log as this package reads it, for a rebuild.
type EventReader interface {
	ListProject(ctx context.Context, projectID string, opts event.ListOptions) (event.Page, error)
}

// ProjectLister is the project model as this package reads it, for a rebuild.
//
// A rebuild visits every project because the event log refuses an unscoped
// read, deliberately: "every event AgentMux has ever recorded" is never one
// query's answer.
type ProjectLister interface {
	List(ctx context.Context, filter project.ListFilter) ([]*project.Project, error)
}

// Options configures a Service. Repository is required.
type Options struct {
	// Repository is where attention and actions are kept. Required.
	Repository Repository

	// States is the agent state projection, read to resolve the attempt an
	// `agent.*` event is about. Without it, only session events can be
	// projected.
	States StateReader

	// Events and Projects are the log and the project list, read by a rebuild.
	Events   EventReader
	Projects ProjectLister

	// Logger receives diagnostics. Nil means slog.Default.
	Logger *slog.Logger
}

// NewService builds a Service.
func NewService(o Options) (*Service, error) {
	if o.Repository == nil {
		return nil, newError(CodeInvalidAttention, "attention: a repository is required")
	}
	s := &Service{
		repo:     o.Repository,
		states:   o.States,
		events:   o.Events,
		projects: o.Projects,
		log:      o.Logger,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// subject is the attempt an event is about.
//
// It is what both halves of the projection need and neither can work out alone:
// an `agent.*` event names a runtime, and a `session.*` event names an attempt
// but no runtime, so the two arrive by different routes and end up the same.
type subject struct {
	AgentSessionID string
	ProjectID      string

	// SessionStatus is the status an attempt's own event moved to, and empty
	// for every other event. It is what a `session.status_changed` says, and
	// it is read rather than inferred - the projection never asks the task
	// model what a status is now.
	SessionStatus string
}

// Project folds one event into the attention and the actions it is about.
//
// It never fails an event, for the reason the state projection gives: by the
// time it runs the fact is already recorded, and a level that could not be
// derived does not un-happen the thing it was derived from. An error is
// reported to the caller so that it can be logged, and the event stands.
//
// The caller's cancellation is ignored, also for the state projection's reason:
// every event is written on the context of whatever produced it, and the one
// that would be lost to a client hanging up is the last event of an attempt -
// which is exactly the one that decides whether anybody needs to look.
func (s *Service) Project(ctx context.Context, ev *event.AgentEvent) error {
	if ev == nil {
		return nil
	}
	ctx = context.WithoutCancel(ctx)

	sub, ok := s.subjectOf(ctx, ev)
	if !ok {
		return nil
	}

	// The settle comes first, so that an action raised by this same event can
	// never be resolved by it. A permission request asserts ACTION_REQUIRED and
	// so does not settle anything anyway; the ordering means that stays true if
	// the rule ever changes.
	if settles(ev.Type, sub.SessionStatus) {
		s.settlePending(ctx, ev, sub)
	}

	if level, reason, read := attentionFor(ev.Type, sub.SessionStatus); read && level != "" {
		if err := s.writeAttention(ctx, ev, sub, level, reason); err != nil {
			return err
		}
	}

	if actionType, reason, raises := actionFor(ev.Type, sub.SessionStatus); raises {
		if err := s.raiseAction(ctx, ev, sub, actionType, reason); err != nil {
			return err
		}
	}
	return nil
}

// subjectOf works out which attempt an event is about, and for which project.
//
// The two kinds of event arrive differently. A `session.*` event carries the
// attempt in its payload - it is the only event in the build that does - and a
// project in its column. An `agent.*` event carries a runtime and no attempt,
// so the attempt is the one the agent state projection has bound to that
// runtime. An event that is neither is not this package's.
func (s *Service) subjectOf(ctx context.Context, ev *event.AgentEvent) (subject, bool) {
	if isSessionEvent(ev.Type) {
		facts, ok := sessionFactsOf(ev.Payload)
		if !ok {
			return subject{}, false
		}
		return subject{
			AgentSessionID: facts.AgentSession,
			ProjectID:      ev.ProjectID,
			SessionStatus:  facts.To,
		}, true
	}

	if !isAgentEvent(ev.Type) {
		return subject{}, false
	}
	if strings.TrimSpace(ev.RuntimeID) == "" {
		return subject{}, false
	}
	if s.states == nil {
		// A server built without the state projection cannot resolve an agent
		// event to an attempt. It is reported rather than guessed at: an
		// attention row attributed to the wrong attempt is worse than none.
		s.log.Warn("an agent event cannot be attributed without the state projection",
			"eventId", ev.ID, "type", ev.Type, "runtimeId", ev.RuntimeID)
		return subject{}, false
	}

	state, err := s.states.StateByRuntime(ctx, ev.RuntimeID)
	if err != nil {
		if agentstate.IsCode(err, agentstate.CodeNotFound) {
			// Nothing is bound to this runtime, so there is no attempt for the
			// event to be about. This is the ordinary case for an agent started
			// without a task.
			s.log.Debug("an agent event names a runtime no attempt is bound to",
				"eventId", ev.ID, "type", ev.Type, "runtimeId", ev.RuntimeID)
			return subject{}, false
		}
		s.log.Warn("could not resolve the attempt an agent event is about",
			"eventId", ev.ID, "type", ev.Type, "error", err)
		return subject{}, false
	}
	return subject{
		AgentSessionID: state.AgentSessionID,
		ProjectID:      state.ProjectID,
	}, true
}

// isSessionEvent reports whether an event is one an attempt's own lifecycle
// produces - the only kind that names an attempt directly.
func isSessionEvent(eventType string) bool {
	return eventType == task.TypeSessionCreated ||
		eventType == task.TypeSessionStatusChanged
}

// isAgentEvent reports whether an event is one Claude's observer produces.
func isAgentEvent(eventType string) bool { return strings.HasPrefix(eventType, "agent.") }

// writeAttention records a level.
func (s *Service) writeAttention(
	ctx context.Context,
	ev *event.AgentEvent,
	sub subject,
	level Level,
	reason string,
) error {
	a := Attention{
		AgentSessionID: sub.AgentSessionID,
		ProjectID:      sub.ProjectID,
		Level:          level,
		Reason:         reason,
		// The event's own time, not this server's clock. It is what decides
		// which event wins and what makes a rebuild produce what the live
		// projection produced.
		UpdatedAt: ev.CreatedAt,
	}
	if err := a.Validate(); err != nil {
		return err
	}
	written, err := s.repo.UpsertAttention(ctx, a)
	if err != nil {
		return err
	}
	if written {
		s.log.Debug("attention projected",
			"agentSessionId", a.AgentSessionID, "level", a.Level, "reason", a.Reason)
	}
	return nil
}

// raiseAction records a pending action.
func (s *Service) raiseAction(
	ctx context.Context,
	ev *event.AgentEvent,
	sub subject,
	actionType ActionType,
	reason string,
) error {
	id := ActionIDForEvent(ev.ID)
	if id == "" {
		// Unreachable: the event service validates its identifiers before
		// storing. Reported rather than given a made-up identity, because an
		// action whose id is not derived from its event is an action that can
		// be raised twice.
		return newError(CodeInvalidAction,
			"event %s cannot raise an action: its identifier is not an event id", ev.ID).
			withDetail("eventId", ev.ID)
	}

	a := Action{
		ID:             id,
		AgentSessionID: sub.AgentSessionID,
		ProjectID:      sub.ProjectID,
		Type:           actionType,
		Status:         ActionPending,
		Reason:         reason,
		CreatedAt:      ev.CreatedAt,
	}
	if err := a.Validate(); err != nil {
		return err
	}
	raised, err := s.repo.RaiseAction(ctx, a)
	if err != nil {
		return err
	}
	if raised {
		s.log.Debug("action raised",
			"actionId", a.ID, "type", a.Type, "agentSessionId", a.AgentSessionID)
	}
	return nil
}

// settlePending resolves whatever an event shows has moved on.
//
// It is reported and swallowed: a queue that could not be tidied does not make
// the event wrong, and the next rebuild would tidy it anyway.
func (s *Service) settlePending(ctx context.Context, ev *event.AgentEvent, sub subject) {
	n, err := s.repo.ResolveActions(ctx, sub.AgentSessionID,
		[]ActionType{ActionPermissionRequest}, ev.CreatedAt)
	if err != nil {
		s.log.Warn("could not resolve a pending action",
			"agentSessionId", sub.AgentSessionID, "eventId", ev.ID, "error", err)
		return
	}
	if n > 0 {
		s.log.Debug("pending actions resolved",
			"agentSessionId", sub.AgentSessionID, "count", n, "by", ev.Type)
	}
}

// Attention returns one attempt's level.
func (s *Service) Attention(ctx context.Context, agentSessionID string) (Attention, error) {
	id := strings.TrimSpace(agentSessionID)
	if id == "" {
		return Attention{}, newError(CodeInvalidAttention,
			"attention must be asked for by attempt").withDetail("field", "agentSessionId")
	}
	return s.repo.Attention(ctx, id)
}

// ListAttentionByProject returns a project's levels, most recently updated
// first.
func (s *Service) ListAttentionByProject(ctx context.Context, projectID string, limit int) ([]Attention, error) {
	id := strings.TrimSpace(projectID)
	if id == "" {
		return nil, newError(CodeInvalidAttention,
			"attention must be listed for a project").withDetail("field", "projectId")
	}
	return s.repo.ListAttentionByProject(ctx, id, LimitOr(limit))
}

// AttentionForProjects returns the most recent level of each of the given
// projects, keyed by project id.
//
// A project with no level is absent from the map rather than present with one,
// for the reason the state lookup gives.
func (s *Service) AttentionForProjects(ctx context.Context, projectIDs []string) (map[string]Attention, error) {
	if len(projectIDs) == 0 {
		return map[string]Attention{}, nil
	}
	list, err := s.repo.NewestAttentionByProjects(ctx, projectIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Attention, len(list))
	for _, a := range list {
		out[a.ProjectID] = a
	}
	return out, nil
}

// PendingCountsForProjects returns how many actions are waiting, per project.
//
// It is the one number a dashboard wants from the queue. The actions themselves
// are read from the per-project endpoint, which is where somebody who wants to
// look at them goes.
func (s *Service) PendingCountsForProjects(ctx context.Context, projectIDs []string) (map[string]int, error) {
	return s.repo.PendingActionCounts(ctx, projectIDs)
}

// Actions returns one attempt's actions, newest first.
func (s *Service) Actions(ctx context.Context, agentSessionID string) ([]Action, error) {
	id := strings.TrimSpace(agentSessionID)
	if id == "" {
		return nil, newError(CodeInvalidAction,
			"actions must be asked for by attempt").withDetail("field", "agentSessionId")
	}
	return s.repo.Actions(ctx, id)
}

// ListActionsByProject returns a project's actions, pending first and then
// newest first.
func (s *Service) ListActionsByProject(ctx context.Context, projectID string, limit int) ([]Action, error) {
	id := strings.TrimSpace(projectID)
	if id == "" {
		return nil, newError(CodeInvalidAction,
			"actions must be listed for a project").withDetail("field", "projectId")
	}
	return s.repo.ListActionsByProject(ctx, id, LimitOr(limit))
}

// ListActions returns actions across every project, pending first and then
// newest first.
//
// The ceiling is this package's, so the clamp is here rather than left to a
// caller that would each have to remember it.
func (s *Service) ListActions(ctx context.Context, limit int) ([]Action, error) {
	return s.repo.ListActions(ctx, LimitOr(limit))
}

// Action returns one action by id.
//
// An id that no event raised comes back as CodeNotFound from the repository; an
// id that is not shaped like one at all is refused here, so that a caller
// cannot send this layer looking for a row that could not exist.
func (s *Service) Action(ctx context.Context, id string) (Action, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return Action{}, newError(CodeInvalidAction,
			"an action must be asked for by id").withDetail("field", "id")
	}
	return s.repo.ActionByID(ctx, trimmed)
}

// PendingCountsByType returns how many actions are waiting, per type, across
// every project.
func (s *Service) PendingCountsByType(ctx context.Context) (map[ActionType]int, error) {
	return s.repo.PendingActionCountsByType(ctx)
}

// RebuildReport says what a rebuild did.
type RebuildReport struct {
	// Projects is how many projects were visited.
	Projects int

	// Events is how many events were read.
	Events int

	// Attention is how many attention rows the projection holds afterwards.
	Attention int

	// Actions is how many action rows it holds afterwards.
	Actions int
}

// rebuildPageSize is how many events one page of a rebuild reads.
const rebuildPageSize = 200

// Rebuild recomputes attention and actions from the event log.
//
// # Why it is one method and not two
//
// The brief sketched `RebuildAttention` and `RebuildActions`. They are one pass
// here because they are folded from the same events in the same order, and two
// methods would either replay the log twice or let one table be recomputed
// without the other - and a queue rebuilt against an attention that was not
// would be a queue nobody could reason about. The report says what each holds
// afterwards, so nothing a caller wanted from the split is lost.
//
// # What it does
//
// It reads every project's timeline, then empties both tables and folds what it
// read back in, oldest event first. The read happens before the clear for the
// reason the state projection gives: a log that cannot be read must leave the
// projection as it was, not empty it.
//
// # What it reads while folding
//
// An `agent.*` event is attributed through the agent state projection, so the
// states must be current before this runs. At start-up they are: the state
// projection rebuilds first. A caller that rebuilds this one alone against an
// empty state table would project only the session events, which is why
// main.go does them in that order and docs/AGENT_ATTENTION.md §5 says so.
func (s *Service) Rebuild(ctx context.Context) (RebuildReport, error) {
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()

	if s.events == nil || s.projects == nil {
		return RebuildReport{}, newError(CodeProjectionFailed,
			"attention: a rebuild needs the event log and the project list")
	}

	projects, err := s.projects.List(ctx, project.ListFilter{})
	if err != nil {
		return RebuildReport{}, wrapError(err, CodeStorageFailure,
			"could not list the projects to rebuild attention from")
	}

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

	if err := s.repo.DeleteAllAttention(ctx); err != nil {
		return RebuildReport{}, err
	}
	if err := s.repo.DeleteAllActions(ctx); err != nil {
		return RebuildReport{}, err
	}

	for _, events := range logs {
		for _, ev := range events {
			if err := s.Project(ctx, ev); err != nil {
				s.log.Warn("could not project an event during a rebuild",
					"eventId", ev.ID, "type", ev.Type, "error", err)
			}
		}
	}

	if report.Attention, err = s.repo.CountAttention(ctx); err != nil {
		return report, err
	}
	if report.Actions, err = s.repo.CountActions(ctx); err != nil {
		return report, err
	}

	if report.Events == 0 {
		s.log.Debug("attention rebuilt from an empty log",
			"projects", report.Projects, "attention", report.Attention, "actions", report.Actions)
	} else {
		s.log.Info("attention and actions rebuilt",
			"projects", report.Projects, "events", report.Events,
			"attention", report.Attention, "actions", report.Actions)
	}
	return report, nil
}

// RebuildIfNeeded rebuilds when either projection is empty, and reports whether
// it ran.
//
// It is what a server calls at start-up, for the reason the state projection
// gives: an empty table over a log that has events in it is what a first run
// after this phase leaves behind, and a projection that already holds something
// is kept current as events are written.
//
// Either table being empty triggers it, because they are folded in one pass and
// recomputing one without the other would be recomputing half a projection.
func (s *Service) RebuildIfNeeded(ctx context.Context) (RebuildReport, bool, error) {
	attentionCount, err := s.repo.CountAttention(ctx)
	if err != nil {
		return RebuildReport{}, false, err
	}
	actionCount, err := s.repo.CountActions(ctx)
	if err != nil {
		return RebuildReport{}, false, err
	}
	if attentionCount > 0 && actionCount > 0 {
		return RebuildReport{}, false, nil
	}

	// An installation with agent activity but no failures and no completions
	// has attention rows and no actions, and would rebuild on every start. The
	// condition above cannot tell "no actions yet" from "actions were lost", so
	// it rebuilds rather than guessing - a handful of queries against a table
	// that would otherwise stay wrong.
	report, err := s.Rebuild(ctx)
	return report, true, err
}

// allEvents reads a project's timeline in the order it happened.
//
// The store returns newest first, because that is what a reader of a timeline
// wants; a projection wants the opposite, so the pages are collected and
// reversed.
func (s *Service) allEvents(ctx context.Context, projectID string) ([]*event.AgentEvent, error) {
	var collected []*event.AgentEvent
	opts := event.ListOptions{Limit: rebuildPageSize}

	for {
		page, err := s.events.ListProject(ctx, projectID, opts)
		if err != nil {
			return nil, wrapError(err, CodeStorageFailure,
				"could not read the timeline of project %s to rebuild its attention", projectID).
				withDetail("projectId", projectID)
		}
		collected = append(collected, page.Events...)
		if page.NextBefore == "" {
			break
		}
		opts.Before = page.NextBefore
	}

	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return collected, nil
}

// sessionFacts is what this package reads out of a session event's payload.
//
// It is a decoding edge with two fields, and the rest of the payload - a task
// id, the status the attempt came from - is not decoded, held or stored.
type sessionFacts struct {
	// AgentSession is the attempt's id.
	AgentSession string `json:"agentSession"`

	// To is the status a status change moved to.
	To string `json:"to"`
}

// sessionFactsOf reads them.
//
// A payload that cannot be decoded, or that names no attempt, yields nothing
// rather than an error: the event is already stored, and a rebuild meeting the
// same payload makes the same choice.
func sessionFactsOf(payload json.RawMessage) (sessionFacts, bool) {
	if len(payload) == 0 {
		return sessionFacts{}, false
	}
	var decoded sessionFacts
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return sessionFacts{}, false
	}
	if strings.TrimSpace(decoded.AgentSession) == "" {
		return sessionFacts{}, false
	}
	return decoded, true
}
