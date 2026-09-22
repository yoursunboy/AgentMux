package controller

import (
	"context"

	"github.com/kutonlagos/agentmux/internal/attention"
	"github.com/kutonlagos/agentmux/internal/project"
)

// ActionReader is the action queue as this package reads it.
//
// It is three reads and no writes. Nothing in this package can raise, resolve
// or expire an action, and that is not a gap: attention.Service.Project is the
// only thing that writes the queue, it is called only from inside the event
// service, and a console that could write it would be a console that could
// answer for a person. docs/ACTION_CENTER.md is the long form.
type ActionReader interface {
	ListActions(ctx context.Context, limit int) ([]attention.Action, error)
	Action(ctx context.Context, id string) (attention.Action, error)
	PendingCountsByType(ctx context.Context) (map[attention.ActionType]int, error)
}

// Actions returns the queue across every project, with the two counts.
//
// # How many queries it takes
//
// Three, whatever the number of projects and however long the queue:
//
//	actions.ListActions            one
//	actions.PendingCountsByType    one
//	projects.List                  one
//
// A name resolved per action would be the N+1 §13 forbids, and it would be the
// worst kind of it: a queue is the one listing in this API that grows without
// bound, because nothing resolves a VIEW_FAILURE.
//
// # What degrades, and what does not
//
// The listing is the request: if it fails there is no queue to show and the
// error is returned. The counts and the names are sections, and degrade the way
// Projects' do - a failure is logged, the counts are left at zero, and an
// unresolvable name is left empty so the client falls back to the id. A queue
// that showed the actions without their project names is worth more than an
// error page.
func (s *Service) Actions(ctx context.Context, limit int) (ActionsView, error) {
	if s.actions == nil {
		return ActionsView{}, newError(CodeUnavailable,
			"the action queue is not available on this server")
	}

	list, err := s.actions.ListActions(ctx, limit)
	if err != nil {
		return ActionsView{}, wrapError(err, CodeUnavailable, "could not list the action queue")
	}

	names, namesOK := s.projectNames(ctx)
	if !namesOK {
		names = nil
	}

	items := make([]ActionItem, 0, len(list))
	for _, a := range list {
		items = append(items, item(a, names))
	}

	view := ActionsView{Actions: items, Count: len(items)}
	needsYou, notices, countsOK := s.queueCounts(ctx)
	if countsOK {
		view.NeedsYou, view.Notices = needsYou, notices
	}
	return view, nil
}

// Action returns one action, with its project named.
//
// A missing action returns the attention package's own not-found error
// unwrapped, so that writeServiceError answers 404 with the code every other
// not-found in this API uses. Wrapping it in a controller code would give one
// absence two spellings, and a client would have to know which endpoint it had
// called to know which one to expect.
func (s *Service) Action(ctx context.Context, id string) (ActionItem, error) {
	if s.actions == nil {
		return ActionItem{}, newError(CodeUnavailable,
			"the action queue is not available on this server")
	}

	a, err := s.actions.Action(ctx, id)
	if err != nil {
		return ActionItem{}, err
	}

	names, namesOK := s.projectNames(ctx)
	if !namesOK {
		names = nil
	}
	return item(a, names), nil
}

// Queue returns the two counts alone, for a console header.
//
// It is separate from Actions because the dashboard response carries the counts
// and not the queue: asking Actions for its listing to throw it away would be a
// query per dashboard request that could not affect the answer. It never
// returns an error, for the reason the dashboard's other sections do not - a
// header that could not count what is waiting should say nothing rather than
// take the page down.
func (s *Service) Queue(ctx context.Context) QueueSummary {
	needsYou, notices, ok := s.queueCounts(ctx)
	if !ok {
		return QueueSummary{}
	}
	return QueueSummary{NeedsYou: needsYou, Notices: notices}
}

// queueCounts reads how much is waiting and folds it into the two lists.
//
// # Why the fold happens here and not in internal/attention
//
// The queue layer knows a type, and LevelOfAction reads a type as a level.
// "Which of these stops work and which can be read later" is a product
// decision about a screen, and a projection that knew it would be a projection
// with an opinion about a dashboard. So the layer that reads a type stays the
// layer that reads a type, and the layer that builds a console's shapes does
// the folding.
//
// A type this build does not know is counted as a notice: it is something
// waiting to be read, and it is certainly not a claim that work has stopped.
func (s *Service) queueCounts(ctx context.Context) (needsYou, notices int, ok bool) {
	if s.actions == nil {
		return 0, 0, false
	}
	byType, err := s.actions.PendingCountsByType(ctx)
	if err != nil {
		s.log.Warn("could not count pending actions for the controller queue", "error", err)
		return 0, 0, false
	}
	for typ, n := range byType {
		if attention.LevelOfAction(typ) == attention.LevelActionRequired {
			needsYou += n
			continue
		}
		notices += n
	}
	return needsYou, notices, true
}

// projectNames returns every project's id and display name, in one query.
//
// The filter excludes archived projects, deliberately. A project that has been
// archived can still have actions waiting - nothing resolves them - and an
// action is not dropped for it: it is listed with an empty name, and the client
// falls back to the id, which is still an identifier the rest of this API uses.
func (s *Service) projectNames(ctx context.Context) (map[string]string, bool) {
	if s.projects == nil {
		return nil, false
	}
	projects, err := s.projects.List(ctx, project.ListFilter{})
	if err != nil {
		s.log.Warn("could not list projects to name the controller queue", "error", err)
		return nil, false
	}
	names := make(map[string]string, len(projects))
	for _, p := range projects {
		names[p.ID] = p.Name
	}
	return names, true
}

// item builds one queue row from an action and the names it was given.
func item(a attention.Action, names map[string]string) ActionItem {
	return ActionItem{
		ID:             a.ID,
		ProjectID:      a.ProjectID,
		ProjectName:    names[a.ProjectID],
		AgentSessionID: a.AgentSessionID,
		Type:           string(a.Type),
		Level:          string(attention.LevelOfAction(a.Type)),
		Status:         string(a.Status),
		Reason:         a.Reason,
		CreatedAt:      a.CreatedAt,
		ResolvedAt:     a.ResolvedAt,
	}
}
