package task

import (
	"context"
	"encoding/json"

	"github.com/kutonlagos/agentmux/internal/event"
)

// This file is the task model's half of the event bridge: the facts a task and
// a session produce, and the one place each of them is recorded.
//
// It follows the shape internal/session/events.go established in Phase 7.1, for
// the same reason and with the same boundary. This package imports the event
// package for the vocabulary of what it can report and for nothing else: it
// never reads an event, never queries one, and never decides anything from one.
// If the recorder were removed, every method in service.go would behave exactly
// as it does now.
//
// docs/TASK_MODEL.md §6 and §7 are the long form.

// Event types this package emits.
//
// There are four, and not one per status. A status change is one kind of fact
// with a variable - which status, and from which - and the variable belongs in
// the payload. A type per status would mean six task types and five session
// types for two facts, and adding a status in a later phase would then require
// adding an event type to a package that is not supposed to know the status
// vocabulary.
//
// The brief for this phase lists task.completed, task.failed, session.started
// and so on as examples. Every one of them is recoverable from the two rows
// below; docs/TASK_MODEL.md §6 records the departure and why.
const (
	// TypeTaskCreated means a task came into existence.
	TypeTaskCreated = "task.created"

	// TypeTaskStatusChanged means a task moved from one status to another.
	TypeTaskStatusChanged = "task.status_changed"

	// TypeSessionCreated means an attempt at a task was created.
	TypeSessionCreated = "session.created"

	// TypeSessionStatusChanged means an attempt moved from one status to
	// another.
	TypeSessionStatusChanged = "session.status_changed"
)

// EventRecorder receives the facts a task or a session produces.
//
// It is one method wide, and it is the same one method the event service
// exposes, so *event.Service satisfies it with nothing in between - the same
// interface internal/session declares, spelled the same way, so that there is
// no adapter and therefore no second place where the vocabularies could drift.
//
// A nil recorder means nothing is recorded. That is a legitimate configuration
// rather than a degraded one: a service built without one manages tasks exactly
// as well, and the task API works with no event log attached at all. That is
// not a convenience for tests - it is the rule in docs/TASK_MODEL.md §7, that
// status is the authority and events are a record, holding by construction.
type EventRecorder interface {
	CreateEvent(
		ctx context.Context,
		projectID, runtimeID, eventType, source string,
		payload json.RawMessage,
	) (*event.AgentEvent, error)
}

// noteEvent records a fact about a task or a session.
//
// It never returns an error, and that is the contract rather than an oversight,
// for the reason internal/session/events.go gives: the thing being recorded has
// already happened - the task exists, the status has changed - and it is in
// that state whatever the database does next. A caller that could fail because
// the history write failed would make the history a precondition of the thing
// it records.
//
// It is deliberately *not* transactional with the status write either. A task
// whose status changed and whose event was not written is a task in the right
// state with an incomplete history, which is strictly better than a task whose
// status change was rolled back because a log write failed.
func (s *Service) noteEvent(
	ctx context.Context,
	projectID, runtimeID, eventType string,
	payload map[string]any,
) {
	if s.events == nil {
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		s.log.Warn("could not encode a task event payload; the event is not recorded",
			"type", eventType, "projectId", projectID, "error", err)
		return
	}
	if _, err := s.events.CreateEvent(ctx, projectID, runtimeID, eventType, event.SourceUser, encoded); err != nil {
		s.log.Warn("could not record a task event",
			"type", eventType, "projectId", projectID, "error", err)
	}
}

// taskEventPayload is the payload of a task event.
//
// It carries the identifier and nothing else. A title is deliberately absent:
// it is the one field in this model a person wrote, and a row that can never be
// edited can never be redacted either - the rule docs/AGENT_EVENTS.md §6
// applies to error strings, applied here to user text.
//
// It is a function rather than a map literal at each call site so that the
// shape of a task payload is stated once, and so that adding a field to it is a
// decision made here rather than in three places.
func taskEventPayload(taskID string) map[string]any {
	return map[string]any{"taskId": taskID}
}

// sessionEventPayload is the payload of a session event. It names both the
// session and the task it is an attempt at, because a reader of the project
// timeline has no other way to place it.
func sessionEventPayload(taskID, sessionID string) map[string]any {
	return map[string]any{"taskId": taskID, "sessionId": sessionID}
}
