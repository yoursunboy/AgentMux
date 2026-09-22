package usage

import (
	"context"
	"log/slog"
	"time"
)

// Options configures a Service.
type Options struct {
	// Repository is where events are kept. Required.
	Repository Repository

	// Logger receives diagnostics. Nil means slog.Default.
	Logger *slog.Logger

	// Now supplies the clock. Nil means time.Now.
	Now func() time.Time

	// NewID generates an event identifier. Nil means this package's own spec.
	NewID func() (string, error)
}

// Service records beta usage events.
//
// It is built whether or not the beta is on, and it is handed to a producer
// only when it is. A server that is not in the beta therefore has no recorder
// wired anywhere, and the five call sites behave exactly as they did before
// this package existed - which is what makes the switch a wiring decision
// rather than a branch at each of them.
type Service struct {
	repo  Repository
	log   *slog.Logger
	now   func() time.Time
	newID func() (string, error)
}

// NewService builds a usage recorder.
func NewService(o Options) (*Service, error) {
	if o.Repository == nil {
		return nil, newError(CodeInvalidEvent, "usage: a repository is required")
	}
	s := &Service{
		repo:  o.Repository,
		log:   o.Logger,
		now:   o.Now,
		newID: o.NewID,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newID == nil {
		s.newID = eventIDSpec.New
	}
	return s, nil
}

// Record writes one usage event.
//
// It never returns an error, and that is the contract rather than an oversight,
// for the reason internal/task's noteEvent gives: the thing being recorded has
// already happened - the page was served, the socket subscribed, the lease
// moved - and it is in that state whatever the database does next. A caller
// that could fail because the history write failed would make the history a
// precondition of the thing it records, which is the wrong way round for a
// count nobody acts on.
//
// It is synchronous. It is deliberately not a goroutine and not a buffered
// channel: a channel is a queue with a failure mode nobody reads, and a
// goroutine per event is an unbounded number of writers to one SQLite file.
// The write is one INSERT into a three-column table, on paths that already talk
// to the database.
//
// The nil receiver check is not defensive decoration. A nil *Service stored in
// a Recorder variable is not a nil interface, so a producer's own nil check
// does not catch it - the same trap the httpapi tests record for the
// projections. Guarding here means the mistake is a no-op rather than a panic
// on a path whose whole job is to not matter.
func (s *Service) Record(ctx context.Context, t EventType) {
	if s == nil || s.repo == nil {
		return
	}
	if !t.Valid() {
		s.log.Warn("refused to record an event that is not one of the five; nothing is recorded",
			"type", string(t))
		return
	}
	id, err := s.newID()
	if err != nil {
		s.log.Warn("could not name a usage event; the event is not recorded",
			"type", string(t), "error", err)
		return
	}
	event := Event{ID: id, Type: t, CreatedAt: s.now().UTC()}
	if err := s.repo.Insert(ctx, event); err != nil {
		s.log.Warn("could not record a usage event", "type", string(t), "error", err)
	}
}

// Count reports how many usage events are stored.
//
// It is a read for tests and for an operator looking at the table directly.
// Unlike Record it returns its error, because a caller asking a question is
// entitled to be told the answer could not be found - the difference being that
// nobody's page depends on this one.
func (s *Service) Count(ctx context.Context) (int, error) {
	if s == nil || s.repo == nil {
		return 0, nil
	}
	return s.repo.Count(ctx)
}
