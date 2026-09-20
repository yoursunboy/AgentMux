package task

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/kutonlagos/agentmux/internal/project"
)

// Service is the task model's only writer and its only reader.
//
// Every rule about a task or a session lives here: what a title may contain,
// which status changes are legal, when a completion time is set, and what a
// conflict means. The HTTP layer decodes a request and encodes a result, and
// the storage layer runs statements; neither decides anything.
//
// Two properties of this type are worth stating because the rest of the phase
// is arranged around them:
//
//   - Status is the authority and events are a record. Nothing here reads an
//     event to work out a status, and a service built with no event recorder at
//     all is fully functional. See docs/TASK_MODEL.md §7.
//   - Every status change is a conditional update. The status this service read
//     is the status the write requires, so two requests arriving at once cannot
//     produce a transition neither of them asked for. See §10.
type Service struct {
	repo     Repository
	projects ProjectLookup
	events   EventRecorder
	now      func() time.Time
	newTask  func() (string, error)
	newSess  func() (string, error)
	log      *slog.Logger
}

// ProjectLookup answers whether a project exists.
//
// It is one method wide and it is satisfied by *project.Service directly, with
// no adapter, so there is no second place where "which projects exist" could be
// answered differently. Declaring it here rather than importing a concrete
// service keeps the dependency pointing the way it does everywhere else in this
// codebase: the consumer states what it needs.
type ProjectLookup interface {
	// Get returns a project, or an error carrying project_not_found.
	Get(ctx context.Context, id string) (*project.Project, error)
}

// Options configures a Service. Repository and Projects are required.
type Options struct {
	// Repository is where tasks and sessions are stored. Required.
	Repository Repository

	// Projects answers whether the project a task names exists. Required: a
	// task under a project that was never registered is unreachable through
	// every endpoint this API has, which makes it a row rather than a record.
	Projects ProjectLookup

	// Events receives one record per task and session fact. Nil means nothing
	// is recorded, which is a legitimate configuration and not a degraded one.
	Events EventRecorder

	// Now supplies the current time. Nil means time.Now.
	//
	// It is injectable so that a test can produce a known completion time
	// without sleeping, and so that a test of the concurrency rule can control
	// the window it is racing in.
	Now func() time.Time

	// NewTaskID generates task identifiers. Nil means NewID.
	NewTaskID func() (string, error)

	// NewSessionID generates session identifiers. Nil means NewSessionID.
	NewSessionID func() (string, error)

	// Logger receives task lifecycle records. Nil means slog.Default.
	Logger *slog.Logger
}

// NewService builds a Service.
func NewService(o Options) (*Service, error) {
	if o.Repository == nil {
		return nil, errors.New("task: Repository is required")
	}
	if o.Projects == nil {
		return nil, errors.New("task: Projects is required")
	}
	s := &Service{
		repo:     o.Repository,
		projects: o.Projects,
		events:   o.Events,
		now:      o.Now,
		newTask:  o.NewTaskID,
		newSess:  o.NewSessionID,
		log:      o.Logger,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newTask == nil {
		s.newTask = NewID
	}
	if s.newSess == nil {
		s.newSess = NewSessionID
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// maxIdentifierLen bounds the identifiers a caller supplies.
//
// It is a guard and not a rule of the model: a task id is 29 characters and a
// runtime id is 27, and a caller passing something an order of magnitude longer
// has passed the wrong value. The bound exists so that a mistake becomes a
// rejection here rather than an oversized row.
const maxIdentifierLen = 128

// CreateTaskInput describes a new task.
type CreateTaskInput struct {
	// ProjectID is the project the work belongs to. Required, and the project
	// must exist.
	ProjectID string

	// Title is what somebody wants done. Required.
	Title string
}

// CreateTask records a piece of work somebody wants an agent to do.
//
// The task begins in CREATED, with no session and no runtime. Nothing is
// started: this phase has no link between creating a task and launching
// anything, and the absence is deliberate - see docs/TASK_MODEL.md §8.
func (s *Service) CreateTask(ctx context.Context, in CreateTaskInput) (*Task, error) {
	projectID := strings.TrimSpace(in.ProjectID)
	if err := checkProjectID(projectID); err != nil {
		return nil, err
	}
	if err := ValidateTitle(in.Title); err != nil {
		return nil, err
	}
	if _, err := s.requireProject(ctx, projectID); err != nil {
		return nil, err
	}

	id, err := s.newTask()
	if err != nil {
		return nil, wrapError(err, CodeStorageFailure, "could not generate a task identifier")
	}
	if !ValidID(id) {
		// The generator is injected, so this is reachable by a test that
		// supplies a bad one. In production it means NewID is broken.
		return nil, newError(CodeStorageFailure,
			"the task identifier generator produced %q, which is not a task id", id)
	}

	now := s.now().UTC()
	t := &Task{
		ID:        id,
		ProjectID: projectID,
		Title:     in.Title,
		Status:    StatusCreated,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.CreateTask(ctx, t); err != nil {
		return nil, classify(err)
	}

	s.log.Info("task created", "taskId", t.ID, "projectId", t.ProjectID, "status", t.Status)
	s.noteEvent(ctx, t.ProjectID, "", TypeTaskCreated, taskEventPayload(t.ID))
	return t, nil
}

// GetTask returns one task by identifier.
func (s *Service) GetTask(ctx context.Context, id string) (*Task, error) {
	if !ValidID(id) {
		// An id outside the namespace cannot be one of ours, and saying "not
		// found" is the honest answer rather than a bad-request: a caller
		// looking a task up by an id it was given should be told the task is
		// not here.
		return nil, newError(CodeTaskNotFound, "no task with id %q", id)
	}
	t, err := s.repo.GetTask(ctx, id)
	if err != nil {
		return nil, mapTaskNotFound(err, id)
	}
	return t, nil
}

// ListTasksInput narrows a task listing.
type ListTasksInput struct {
	// ProjectID selects one project's tasks. Required.
	ProjectID string

	// Status, when set, selects only tasks in that status.
	Status string

	// Limit is how many to return. Zero means DefaultLimit; a value above
	// MaxLimit is clamped to it.
	Limit int
}

// ListTasks returns a project's tasks, newest first.
//
// The project is resolved before the list is read, so that an unknown project
// is answered as a missing project rather than as an empty list. Those two
// answers mean different things and an empty list is the one a client cannot
// tell apart from a project with nothing in it.
func (s *Service) ListTasks(ctx context.Context, in ListTasksInput) ([]*Task, error) {
	projectID := strings.TrimSpace(in.ProjectID)
	if err := checkProjectID(projectID); err != nil {
		return nil, err
	}
	if in.Status != "" && !ValidTaskStatus(in.Status) {
		return nil, newError(CodeInvalidInput,
			"%q is not a task status; expected one of %s",
			in.Status, strings.Join(TaskStatuses(), ", ")).
			withDetail("field", "status")
	}
	if _, err := s.requireProject(ctx, projectID); err != nil {
		return nil, err
	}

	tasks, err := s.repo.ListTasks(ctx, TaskQuery{
		ProjectID: projectID,
		Status:    in.Status,
		Limit:     in.Limit,
	})
	if err != nil {
		return nil, classify(err)
	}
	if tasks == nil {
		// A project with no tasks is an empty list and not a null, so a client
		// can render it without a special case.
		tasks = []*Task{}
	}
	return tasks, nil
}

// UpdateTaskInput describes a change to a task.
//
// Both fields are pointers because both are optional and "absent" is different
// from "set to the empty string": a caller that changes only the status must
// not have to repeat the title, and a caller that clears a title has made a
// mistake rather than a request.
type UpdateTaskInput struct {
	// Title, when set, replaces the task's title.
	Title *string

	// Status, when set, is the status to move to. It must be a status the
	// lifecycle allows from the current one.
	Status *string
}

// UpdateTask applies a change to a task.
//
// Every part of the request is validated before anything is written, so a
// request carrying a legal status and an illegal title changes nothing at all
// rather than half of it.
func (s *Service) UpdateTask(ctx context.Context, id string, in UpdateTaskInput) (*Task, error) {
	if in.Title == nil && in.Status == nil {
		return nil, newError(CodeInvalidInput,
			"a task update must change the title, the status, or both").
			withDetail("field", "status")
	}

	current, err := s.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Title != nil {
		if err := ValidateTitle(*in.Title); err != nil {
			return nil, err
		}
	}
	if in.Status != nil {
		if err := checkTransition(current, *in.Status); err != nil {
			return nil, err
		}
	}

	// The status first, because it is the write that can lose a race. A caller
	// that is told it lost one must not be left holding a title it did change.
	if in.Status != nil && *in.Status != current.Status {
		if current, err = s.writeStatus(ctx, current, *in.Status); err != nil {
			return nil, err
		}
	}
	if in.Title != nil && *in.Title != current.Title {
		if err := s.repo.UpdateTaskTitle(ctx, current.ID, *in.Title, s.now().UTC()); err != nil {
			return nil, classify(err)
		}
		s.log.Info("task title changed", "taskId", current.ID, "projectId", current.ProjectID)
		if current, err = s.GetTask(ctx, id); err != nil {
			return nil, err
		}
	}
	return current, nil
}

// UpdateTaskStatus moves a task to another status.
//
// This is the whole of the lifecycle rule and it is the only method that
// changes a task's status. The HTTP layer has no path to the table, so a
// transition that the lifecycle does not allow cannot be written by any route.
func (s *Service) UpdateTaskStatus(ctx context.Context, id, to string) (*Task, error) {
	current, err := s.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkTransition(current, to); err != nil {
		return nil, err
	}
	if to == current.Status {
		// Asking for the status a task already has is not a transition and
		// writes nothing. It is answered with the task as it stands rather than
		// with a refusal, because a client retrying a request it is not sure
		// landed should get the state it asked for.
		return current, nil
	}
	return s.writeStatus(ctx, current, to)
}

// writeStatus applies a validated transition, conditionally, and records it.
func (s *Service) writeStatus(ctx context.Context, current *Task, to string) (*Task, error) {
	now := s.now().UTC()
	change := TaskStatusChange{From: current.Status, To: to, At: now}
	if to == StatusCompleted {
		change.CompletedAt = &now
	}

	if err := s.repo.UpdateTaskStatus(ctx, current.ID, change); err != nil {
		if errors.Is(err, ErrStatusConflict) {
			// Somebody changed the task between the read that validated this
			// transition and the write that would have applied it. The
			// transition was never legal against the state the row is actually
			// in, so it was not applied, and the caller is told it lost rather
			// than handed a status it did not ask for.
			return nil, newError(CodeConflict,
				"task %s changed while this request was being applied; it is no longer %s",
				current.ID, current.Status).
				withDetail("expectedStatus", current.Status).
				withDetail("field", "status")
		}
		return nil, classify(err)
	}

	s.log.Info("task status changed",
		"taskId", current.ID, "projectId", current.ProjectID, "from", current.Status, "to", to)
	s.noteEvent(ctx, current.ProjectID, "", TypeTaskStatusChanged,
		map[string]any{"taskId": current.ID, "from": current.Status, "to": to})

	// Read back rather than patching the in-memory copy: the row is the
	// authority, and a concurrent change to the title or the completion time
	// would otherwise be reported as though it had not happened.
	return s.GetTask(ctx, current.ID)
}

// checkTransition refuses a status change the lifecycle does not allow.
//
// The refusal names what would have worked, because "invalid transition" alone
// sends the caller to the documentation to find out which statuses were
// available - and the answer is a property of the current status, which the
// caller may not have.
func checkTransition(current *Task, to string) error {
	if !ValidTaskStatus(to) {
		return newError(CodeInvalidInput,
			"%q is not a task status; expected one of %s",
			to, strings.Join(TaskStatuses(), ", ")).
			withDetail("field", "status")
	}
	if to == current.Status {
		return nil
	}
	if !CanTransitionTask(current.Status, to) {
		return newError(CodeInvalidTransition,
			"a task cannot move from %s to %s; from %s it may move to %s",
			current.Status, to, current.Status,
			strings.Join(AllowedTaskTransitions(current.Status), ", ")).
			withDetail("from", current.Status).
			withDetail("to", to).
			withDetail("allowed", AllowedTaskTransitions(current.Status))
	}
	return nil
}

// CreateSessionInput describes a new attempt at a task.
type CreateSessionInput struct {
	// TaskID is the task this is an attempt at. Required, and the task must
	// exist.
	TaskID string
}

// CreateSession starts a new attempt at a task.
//
// The session begins in CREATED with **no runtime**. Attaching one is a
// separate request, and the window between the two is a real state the model
// names rather than hides: creating a session and starting a process are two
// different acts, and a transaction spanning them would hold a write lock open
// across a process launch. See docs/TASK_MODEL.md §4.
func (s *Service) CreateSession(ctx context.Context, in CreateSessionInput) (*AgentSession, error) {
	taskID := strings.TrimSpace(in.TaskID)
	if taskID == "" || len(taskID) > maxIdentifierLen {
		return nil, newError(CodeInvalidInput, "a session must name the task it is an attempt at").
			withDetail("field", "taskId")
	}
	parent, err := s.requireTask(ctx, taskID)
	if err != nil {
		return nil, err
	}

	id, err := s.newSess()
	if err != nil {
		return nil, wrapError(err, CodeStorageFailure, "could not generate a session identifier")
	}
	if !ValidSessionID(id) {
		return nil, newError(CodeStorageFailure,
			"the session identifier generator produced %q, which is not a session id", id)
	}

	sess := &AgentSession{
		ID:        id,
		TaskID:    parent.ID,
		Status:    StatusSessionCreated,
		CreatedAt: s.now().UTC(),
	}
	if err := s.repo.CreateSession(ctx, sess); err != nil {
		return nil, classify(err)
	}

	s.log.Info("session created", "sessionId", sess.ID, "taskId", sess.TaskID, "projectId", parent.ProjectID)
	s.noteEvent(ctx, parent.ProjectID, "", TypeSessionCreated,
		sessionEventPayload(sess.TaskID, sess.ID))
	return sess, nil
}

// GetSession returns one agent session by identifier.
func (s *Service) GetSession(ctx context.Context, id string) (*AgentSession, error) {
	if !ValidSessionID(id) {
		return nil, newError(CodeSessionNotFound, "no agent session with id %q", id)
	}
	sess, err := s.repo.GetSession(ctx, id)
	if err != nil {
		return nil, mapSessionNotFound(err, id)
	}
	return sess, nil
}

// ListSessionsInput narrows a session listing.
type ListSessionsInput struct {
	// TaskID selects one task's sessions. Required.
	TaskID string

	// Limit is how many to return. Zero means DefaultLimit; a value above
	// MaxLimit is clamped to it.
	Limit int
}

// ListSessions returns one task's attempts, newest first.
//
// The task is resolved first, so an unknown task is answered as a missing task
// rather than as a task with no attempts.
func (s *Service) ListSessions(ctx context.Context, in ListSessionsInput) ([]*AgentSession, error) {
	taskID := strings.TrimSpace(in.TaskID)
	if taskID == "" || len(taskID) > maxIdentifierLen {
		return nil, newError(CodeInvalidInput, "a session listing must name a task").
			withDetail("field", "taskId")
	}
	if _, err := s.requireTask(ctx, taskID); err != nil {
		return nil, err
	}

	sessions, err := s.repo.ListSessions(ctx, SessionQuery{TaskID: taskID, Limit: in.Limit})
	if err != nil {
		return nil, classify(err)
	}
	if sessions == nil {
		sessions = []*AgentSession{}
	}
	return sessions, nil
}

// AttachSessionRuntime binds a runtime to a session.
//
// The runtime id must be the session name of a project - the prefix AgentMux
// names its runtimes with, followed by a project identifier - and it is
// deliberately **not** required to exist. A runtime can be destroyed while the
// session that used it remains, so "does this runtime exist right now" is the
// wrong question to ask about the runtime an attempt ran in.
//
// The shape is checked against the project model's own identifier rule rather
// than against the prefix alone, because the runtime id *is* a project id with
// a prefix: asking project.ValidID about the body is asking the package that
// spells the namespace, so there is one spelling of it and not two.
func (s *Service) AttachSessionRuntime(ctx context.Context, sessionID, runtimeID string) (*AgentSession, error) {
	runtimeID = strings.TrimSpace(runtimeID)
	if !project.ValidRuntimeID(runtimeID) {
		return nil, newError(CodeInvalidInput,
			"%q is not a runtime identifier; a runtime id is its session name, "+
				"which is %q followed by a project id",
			runtimeID, project.SessionPrefix).
			withDetail("field", "runtimeId")
	}
	// No separate length check: a runtime identifier has one shape, and that
	// shape is a fixed number of characters. A bound written out again here
	// would be a second place for it to be wrong and could never fire.

	current, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if current.RuntimeID != "" {
		return nil, newError(CodeConflict,
			"session %s already ran in runtime %s", current.ID, current.RuntimeID).
			withDetail("runtimeId", current.RuntimeID)
	}

	if err := s.repo.AttachSessionRuntime(ctx, current.ID, runtimeID); err != nil {
		if errors.Is(err, ErrStatusConflict) {
			// Lost the race to another attach between the read and the write.
			return nil, newError(CodeConflict, "session %s already has a runtime", current.ID)
		}
		return nil, classify(err)
	}
	s.log.Info("session runtime attached",
		"sessionId", current.ID, "taskId", current.TaskID, "runtimeId", runtimeID)

	// No event is written for this. Attaching is a bookkeeping link rather than
	// a change in what is happening: what a reader of the timeline wants to
	// know is that the attempt started, and that is the status change to
	// RUNNING. docs/TASK_MODEL.md §6 records the decision.
	return s.GetSession(ctx, sessionID)
}

// UpdateSessionStatus moves an attempt to another status.
//
// Entering RUNNING sets StartedAt; entering a terminal status sets EndedAt. A
// session that failed before it ever ran therefore has an end and no start,
// which is a true description of a runtime that never came up.
func (s *Service) UpdateSessionStatus(ctx context.Context, id, to string) (*AgentSession, error) {
	current, err := s.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ValidSessionStatus(to) {
		return nil, newError(CodeInvalidInput,
			"%q is not an agent session status; expected one of %s",
			to, strings.Join(AgentSessionStatuses(), ", ")).
			withDetail("field", "status")
	}
	if to == current.Status {
		return current, nil
	}
	if !CanTransitionSession(current.Status, to) {
		return nil, newError(CodeInvalidTransition,
			"a session cannot move from %s to %s; from %s it may move to %s",
			current.Status, to, current.Status,
			strings.Join(AllowedSessionTransitions(current.Status), ", ")).
			withDetail("from", current.Status).
			withDetail("to", to).
			withDetail("allowed", AllowedSessionTransitions(current.Status))
	}

	now := s.now().UTC()
	change := SessionStatusChange{From: current.Status, To: to}
	switch {
	case to == StatusSessionRunning:
		change.StartedAt = &now
	case SessionTerminal(to):
		change.EndedAt = &now
	}

	if err := s.repo.UpdateSessionStatus(ctx, current.ID, change); err != nil {
		if errors.Is(err, ErrStatusConflict) {
			return nil, newError(CodeConflict,
				"session %s changed while this request was being applied; it is no longer %s",
				current.ID, current.Status).
				withDetail("expectedStatus", current.Status).
				withDetail("field", "status")
		}
		return nil, classify(err)
	}

	s.log.Info("session status changed",
		"sessionId", current.ID, "taskId", current.TaskID, "from", current.Status, "to", to)
	if projectID := s.projectIDOf(ctx, current.TaskID); projectID != "" {
		s.noteEvent(ctx, projectID, current.RuntimeID, TypeSessionStatusChanged,
			map[string]any{
				"taskId": current.TaskID, "sessionId": current.ID,
				"from": current.Status, "to": to,
			})
	}

	return s.GetSession(ctx, id)
}

// requireProject returns the project a task would belong to, or an error.
//
// A project that is missing is reported with this package's
// CodeProjectNotFound, which is spelled the same as the project model's own
// code: a client switching on it does not need to know which layer answered.
func (s *Service) requireProject(ctx context.Context, projectID string) (*project.Project, error) {
	p, err := s.projects.Get(ctx, projectID)
	if err != nil {
		if project.IsCode(err, project.CodeNotFound) {
			return nil, newError(CodeProjectNotFound, "no project with id %q", projectID).
				withDetail("projectId", projectID)
		}
		return nil, classify(err)
	}
	return p, nil
}

// requireTask returns the task a session would belong to, or an error.
func (s *Service) requireTask(ctx context.Context, taskID string) (*Task, error) {
	t, err := s.repo.GetTask(ctx, taskID)
	if err != nil {
		return nil, mapTaskNotFound(err, taskID)
	}
	return t, nil
}

// projectIDOf returns the project a task belongs to, or "" when it cannot be
// read.
//
// It exists so that a status change is never failed by the lookup an event
// needs. The change has already been written by the time this runs; losing the
// event is a gap in the history, and failing the request would be a lie about
// the state.
func (s *Service) projectIDOf(ctx context.Context, taskID string) string {
	t, err := s.repo.GetTask(ctx, taskID)
	if err != nil {
		s.log.Warn("could not resolve the project for a session event; the event is not recorded",
			"taskId", taskID, "error", err)
		return ""
	}
	return t.ProjectID
}

// checkProjectID bounds a project identifier supplied by a caller.
func checkProjectID(projectID string) error {
	if projectID == "" {
		return newError(CodeInvalidInput, "a task must name the project it belongs to").
			withDetail("field", "projectId")
	}
	if len(projectID) > maxIdentifierLen {
		return newError(CodeInvalidInput,
			"a project identifier must be at most %d characters", maxIdentifierLen).
			withDetail("field", "projectId")
	}
	return nil
}

// mapTaskNotFound converts a repository miss into a task-model error.
func mapTaskNotFound(err error, id string) error {
	if errors.Is(err, ErrNotFound) {
		return newError(CodeTaskNotFound, "no task with id %q", id)
	}
	return classify(err)
}

// mapSessionNotFound converts a repository miss into a session-model error.
func mapSessionNotFound(err error, id string) error {
	if errors.Is(err, ErrSessionNotFound) {
		return newError(CodeSessionNotFound, "no agent session with id %q", id)
	}
	return classify(err)
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
	return wrapError(err, CodeStorageFailure, "the task store failed")
}
