package event

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
)

// Service is the event model's only writer and its only reader.
//
// Everything that records a fact goes through CreateEvent. The runtime bridge
// calls it now; a Claude hook, a user action and whatever a later phase adds
// call the same method, so the validation, the identifier, the timestamp and
// the payload bound are applied in one place and cannot be skipped by a caller
// that found a shorter route to the table.
//
// The service owns the rules, not the storage. Every SQL statement lives in the
// storage package, and nothing outside it ever sees a *sql.DB.
type Service struct {
	repo  Repository
	now   func() time.Time
	newID func() (string, error)
	log   *slog.Logger
}

// Options configures a Service. Only Repository is required.
type Options struct {
	// Repository is where events are stored. Required.
	Repository Repository

	// Now supplies the current time, which is the event's CreatedAt. Nil means
	// time.Now.
	//
	// It is injectable so that a test can produce events in a known order
	// without sleeping between them.
	Now func() time.Time

	// NewID generates event identifiers. Nil means NewID.
	NewID func() (string, error)

	// Logger receives one record per stored event. Nil means slog.Default.
	Logger *slog.Logger
}

// NewService builds a Service.
func NewService(o Options) (*Service, error) {
	if o.Repository == nil {
		return nil, errors.New("event: Repository is required")
	}
	s := &Service{
		repo:  o.Repository,
		now:   o.Now,
		newID: o.NewID,
		log:   o.Logger,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newID == nil {
		s.newID = NewID
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// maxIdentifierLen bounds the two identifiers an event carries.
//
// It is a guard and not a rule of the model: a project id is 23 characters and
// a session name is 27, and a caller passing something an order of magnitude
// longer than either has passed the wrong value. The bound exists so that a
// mistake becomes a rejection here rather than an oversized row.
const maxIdentifierLen = 128

// CreateEvent records that something happened, and returns the stored event.
//
// This is the single entry point to the event log. It is the only exported
// method that writes.
//
// It is deliberately *not* transactional with whatever produced the fact. The
// runtime bridge calls it after the runtime has already changed state, and a
// history write that could fail a start would make the event layer a dependency
// of the thing it is recording. A caller that cares whether the write succeeded
// gets the error; the runtime bridge logs it and carries on.
//
// An empty runtimeID means the event is about the project rather than about one
// of its runtimes. That is the ordinary case for anything an API request
// produces.
func (s *Service) CreateEvent(
	ctx context.Context,
	projectID, runtimeID, eventType, source string,
	payload json.RawMessage,
) (*AgentEvent, error) {
	if err := checkIdentifiers(projectID, runtimeID); err != nil {
		return nil, err
	}
	if !ValidType(eventType) {
		return nil, newError(CodeInvalidEvent,
			"%q is not an event type; a type is a dotted lowercase name such as %q",
			eventType, TypeRuntimeStarted).
			withDetail("field", "type")
	}
	if !ValidSource(source) {
		return nil, newError(CodeInvalidEvent,
			"%q is not an event source; expected one of %s",
			source, strings.Join(Sources(), ", ")).
			withDetail("field", "source")
	}
	if err := CheckPayload(payload); err != nil {
		return nil, err
	}

	id, err := s.newID()
	if err != nil {
		return nil, wrapError(err, CodeStorageFailure, "could not generate an event identifier")
	}
	if !ValidID(id) {
		// The identifier generator is injected, so this is reachable by a test
		// that supplies a bad one. In production it means NewID is broken.
		return nil, newError(CodeStorageFailure,
			"the event identifier generator produced %q, which is not an event id", id)
	}

	ev := &AgentEvent{
		ID:        id,
		ProjectID: projectID,
		RuntimeID: runtimeID,
		Type:      eventType,
		Source:    source,
		Payload:   payload,
		CreatedAt: s.now().UTC(),
	}
	if err := s.repo.Create(ctx, ev); err != nil {
		return nil, classify(err)
	}

	// What is logged is what identifies the event. The payload is left out on
	// purpose: it is the one field whose content a caller chose, and a log has
	// no key check in front of it - see AgentEvent.String.
	s.log.Info("event created",
		"eventId", ev.ID,
		"type", ev.Type,
		"source", ev.Source,
		"projectId", ev.ProjectID,
		"runtimeId", ev.RuntimeID,
	)
	return ev, nil
}

// checkIdentifiers refuses an event that does not name a project, and bounds
// both identifiers.
func checkIdentifiers(projectID, runtimeID string) error {
	if strings.TrimSpace(projectID) == "" {
		return newError(CodeInvalidEvent, "an event must name the project it happened to").
			withDetail("field", "projectId")
	}
	if len(projectID) > maxIdentifierLen || len(runtimeID) > maxIdentifierLen {
		return newError(CodeInvalidEvent,
			"an event identifier must be at most %d characters", maxIdentifierLen).
			withDetail("field", "projectId")
	}
	return nil
}

// ListOptions narrows a timeline read.
type ListOptions struct {
	// Limit is how many events to return. Zero means DefaultLimit; a value
	// above MaxLimit is clamped to it.
	Limit int

	// Before is an event id. When set, only events older than that one are
	// returned, which is how a caller reads the next page.
	Before string
}

// Page is one page of a timeline.
type Page struct {
	// Events are the events in this page, newest first.
	Events []*AgentEvent

	// NextBefore is the cursor for the following page, or empty when this page
	// is the last one.
	NextBefore string
}

// ListProject returns one project's timeline, newest first.
//
// The project is not resolved here. That belongs to the HTTP layer, which has
// the project service and can answer "no such project" with a message about the
// project rather than about events - and the answer matters, because an empty
// timeline and a project that does not exist are different things that would
// otherwise look identical.
func (s *Service) ListProject(ctx context.Context, projectID string, opts ListOptions) (Page, error) {
	if strings.TrimSpace(projectID) == "" {
		return Page{}, newError(CodeInvalidEvent, "a project timeline needs a project id")
	}
	return s.list(ctx, Query{ProjectID: projectID}, opts)
}

// ListRuntime returns one runtime's timeline, newest first.
//
// This is the timeline of a runtime rather than of a project: a runtime that
// was destroyed and started again keeps one id, so its events read as the
// history of that runtime across everything it has been through.
func (s *Service) ListRuntime(ctx context.Context, runtimeID string, opts ListOptions) (Page, error) {
	if strings.TrimSpace(runtimeID) == "" {
		return Page{}, newError(CodeInvalidEvent, "a runtime timeline needs a runtime id")
	}
	return s.list(ctx, Query{RuntimeID: runtimeID}, opts)
}

// list runs a timeline read, one row past the page so the caller can be told
// whether another page exists.
func (s *Service) list(ctx context.Context, query Query, opts ListOptions) (Page, error) {
	wanted := EffectiveLimit(opts.Limit)

	if strings.TrimSpace(opts.Before) != "" {
		cursor, err := s.repo.ResolveCursor(ctx, opts.Before)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return Page{}, newError(CodeNotFound,
					"no event with id %q, so there is nothing to page from", opts.Before).
					withDetail("before", opts.Before)
			}
			return Page{}, classify(err)
		}
		query.Before = &cursor
	}

	// One more row than the caller asked for, which is how "there is another
	// page" is answered without a second query. It is never returned.
	query.Limit = wanted + 1
	events, err := s.repo.List(ctx, query)
	if err != nil {
		return Page{}, classify(err)
	}

	page := Page{Events: events}
	if len(events) > wanted {
		page.Events = events[:wanted]
		// The cursor names the last event the caller is actually being given,
		// so the next page starts immediately after it.
		page.NextBefore = page.Events[wanted-1].ID
	}
	if page.Events == nil {
		// A timeline with nothing in it is an empty list and not a null, so a
		// client can render it without a special case.
		page.Events = []*AgentEvent{}
	}
	return page, nil
}

// classify gives an error from the repository a code.
//
// This package's contract is that every error it returns carries one of the
// Code* values, because the HTTP layer turns a code into a status and a message
// and has nothing to say about an unclassified error. A repository that returns
// a coded error has already decided what went wrong and is left alone; one that
// returns a bare error - a driver error, an error from a test double that does
// not know this package's vocabulary - is reported as the storage failure it
// is.
//
// Handling it here rather than at each call site means the guarantee is a
// property of the package, not of every implementation of Repository that
// anyone ever writes.
func classify(err error) error {
	if err == nil || CodeOf(err) != "" {
		return err
	}
	return wrapError(err, CodeStorageFailure, "the event store failed")
}

// EffectiveLimit clamps a requested page size to what a caller may ask for.
//
// It is exported because the HTTP layer reports the same number it applied, and
// a response that said one thing while the query did another would be the kind
// of disagreement that only shows up in a client written against the docs.
func EffectiveLimit(requested int) int {
	switch {
	case requested <= 0:
		return DefaultLimit
	case requested > MaxLimit:
		return MaxLimit
	default:
		return requested
	}
}
