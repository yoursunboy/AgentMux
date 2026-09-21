package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/kutonlagos/agentmux/internal/agentstate"
	"github.com/kutonlagos/agentmux/internal/attention"
	"github.com/kutonlagos/agentmux/internal/project"
)

// Service aggregates what a console needs into one response.
//
// It holds no state, writes nothing and caches nothing. Every call asks the
// services underneath it and arranges the answers.
type Service struct {
	projects  ProjectLister
	agents    StateReader
	attention AttentionReader
	log       *slog.Logger
}

// ProjectLister is the project model as this package reads it.
//
// A listing is all it needs: a project's own status is derived from its runtime
// by the project service, in memory, which is what makes reading a hundred
// projects one query rather than a hundred.
type ProjectLister interface {
	List(ctx context.Context, filter project.ListFilter) ([]*project.Project, error)
}

// StateReader is the agent state projection as this package reads it.
type StateReader interface {
	StatesForProjects(ctx context.Context, projectIDs []string) (map[string]agentstate.AgentState, error)
}

// AttentionReader is the attention projection as this package reads it.
type AttentionReader interface {
	AttentionForProjects(ctx context.Context, projectIDs []string) (map[string]attention.Attention, error)
	PendingCountsForProjects(ctx context.Context, projectIDs []string) (map[string]int, error)
}

// Options configures a Service. Projects is required.
type Options struct {
	// Projects is the project model. Required: without it there is nothing to
	// aggregate.
	Projects ProjectLister

	// Agents and Attention are the two projections this package reads.
	//
	// Either may be nil, and a server built without one reports that section as
	// unavailable rather than failing. That is §12 of the phase brief: a
	// dashboard shows what it can, and a missing projection is a deployment
	// fact rather than a reason to show nothing at all.
	//
	// A caller that has no projection must pass an untyped nil. A nil
	// *agentstate.Service stored in a StateReader is not a nil interface, and
	// the check below would not see it - the aggregation would then call a
	// method on a nil receiver. It is Go's oldest interface trap and the only
	// defence is to say so here.
	Agents    StateReader
	Attention AttentionReader

	// Logger receives diagnostics. Nil means slog.Default.
	Logger *slog.Logger
}

// NewService builds a Service.
func NewService(o Options) (*Service, error) {
	if o.Projects == nil {
		return nil, newError(CodeUnavailable, "controller: a project list is required")
	}
	s := &Service{
		projects:  o.Projects,
		agents:    o.Agents,
		attention: o.Attention,
		log:       o.Logger,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// Dashboard returns the whole console.
//
// The server block is supplied by the caller rather than assembled here. It
// describes the machine - which host adapter answered, whether tmux is
// installed, where this server keeps its files - and none of that is anything
// this package has an opinion about. §5 of the phase brief asks that the
// existing report be reused rather than re-derived, and taking it as an
// argument is the most direct way to make that true: there is no second place
// it could come from.
func (s *Service) Dashboard(ctx context.Context, server ServerSummary) (Dashboard, error) {
	cards, err := s.Projects(ctx)
	if err != nil {
		return Dashboard{}, err
	}
	return Dashboard{Server: server, Projects: cards, Count: len(cards)}, nil
}

// Projects returns one card per project, most in need of attention first.
//
// # How many queries it takes
//
// Three, plus the project listing itself, whatever the number of projects:
//
//	projects.List                  one
//	agents.StatesForProjects       one
//	attention.AttentionForProjects one
//	attention.PendingCounts        one
//
// That is the whole of what §13 of the phase brief asks for. The obvious
// implementation - a loop over the projects asking each one four questions -
// would be four hundred queries for a hundred projects, and the cost would grow
// with the number of projects rather than with the size of the answer.
//
// # What degrades, and what does not
//
// The project listing is the request. If it fails there is no console to show
// and the error is returned. The three projections are sections: a missing one,
// or one that could not be read, marks that section `available: false` and the
// rest of the card is still built. A dashboard that shows five of seven things
// is worth more than an error page, and the failure is logged where an operator
// can find it.
func (s *Service) Projects(ctx context.Context) ([]ProjectCard, error) {
	projects, err := s.projects.List(ctx, project.ListFilter{})
	if err != nil {
		return nil, wrapError(err, CodeUnavailable, "could not list the projects")
	}

	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		ids = append(ids, p.ID)
	}

	// A console with no projects asks nothing. The projections answer an empty
	// list correctly - each returns early - but a batch query for no rows is a
	// query that cannot match anything, and three of them per dashboard request
	// on an installation with nothing registered is three too many.
	if len(ids) == 0 {
		return []ProjectCard{}, nil
	}

	states, statesOK := s.statesFor(ctx, ids)
	levels, attentionOK := s.attentionFor(ctx, ids)
	pending, actionsOK := s.pendingFor(ctx, ids)

	cards := make([]ProjectCard, 0, len(projects))
	for _, p := range projects {
		cards = append(cards, s.card(p, states, statesOK, levels, attentionOK, pending, actionsOK))
	}
	sortCards(cards)
	return cards, nil
}

// statesFor reads the agent state of every project, degrading to "unavailable".
func (s *Service) statesFor(ctx context.Context, ids []string) (map[string]agentstate.AgentState, bool) {
	if s.agents == nil {
		return nil, false
	}
	states, err := s.agents.StatesForProjects(ctx, ids)
	if err != nil {
		s.log.Warn("could not read the agent states for the controller dashboard", "error", err)
		return nil, false
	}
	return states, true
}

// attentionFor reads the attention of every project, degrading the same way.
func (s *Service) attentionFor(ctx context.Context, ids []string) (map[string]attention.Attention, bool) {
	if s.attention == nil {
		return nil, false
	}
	levels, err := s.attention.AttentionForProjects(ctx, ids)
	if err != nil {
		s.log.Warn("could not read attention for the controller dashboard", "error", err)
		return nil, false
	}
	return levels, true
}

// pendingFor reads how much is waiting in every project.
func (s *Service) pendingFor(ctx context.Context, ids []string) (map[string]int, bool) {
	if s.attention == nil {
		return nil, false
	}
	pending, err := s.attention.PendingCountsForProjects(ctx, ids)
	if err != nil {
		s.log.Warn("could not count pending actions for the controller dashboard", "error", err)
		return nil, false
	}
	return pending, true
}

// card builds one project's card from what the four reads produced.
func (s *Service) card(
	p *project.Project,
	states map[string]agentstate.AgentState,
	statesOK bool,
	levels map[string]attention.Attention,
	attentionOK bool,
	pending map[string]int,
	actionsOK bool,
) ProjectCard {
	card := ProjectCard{
		ID:        p.ID,
		Name:      p.Name,
		Runtime:   RuntimeSummary{Status: p.Status},
		UpdatedAt: p.UpdatedAt,
	}

	if !statesOK {
		// The projection is not wired, or could not be read. A section that
		// says so is more useful than one that says nothing is running.
		card.Agent = &AgentSummary{Available: false}
	} else if state, ok := states[p.ID]; ok {
		card.Agent = &AgentSummary{
			Available: true,
			SessionID: state.AgentSessionID,
			Status:    string(state.Status),
			LastEvent: state.LastEvent,
			UpdatedAt: state.LastEventAt,
		}
		card.UpdatedAt = latest(card.UpdatedAt, state.LastEventAt)
	}
	// No state at all leaves Agent nil, which is what §8 asks for: an agent
	// that has never run is reported as absent rather than invented.

	if !attentionOK {
		card.Attention = &AttentionSummary{Available: false}
	} else if a, ok := levels[p.ID]; ok {
		card.Attention = &AttentionSummary{
			Available: true,
			Level:     string(a.Level),
			Reason:    a.Reason,
		}
		card.UpdatedAt = latest(card.UpdatedAt, a.UpdatedAt)
	}

	card.Actions = ActionsSummary{Available: actionsOK, Pending: pending[p.ID]}
	return card
}

// latest returns the later of two times.
func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
