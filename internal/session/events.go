package session

import (
	"context"
	"encoding/json"

	"github.com/kutonlagos/agentmux/internal/event"
)

// This file is the runtime's half of the event bridge: the four facts a runtime
// produces, and the one place each of them is recorded.
//
// It is deliberately the smallest thing that can work. The runtime imports the
// event package for the vocabulary of what a runtime can report and for nothing
// else - it never reads an event, never queries one, and never decides anything
// from one. If the recorder were removed, every method in manager.go would
// behave exactly as it does now, which is the property that keeps a history
// layer from becoming a dependency of the thing it records.
//
// docs/AGENT_EVENTS.md §5 is the long form.

// EventRecorder receives the facts a runtime produces.
//
// It is one method wide, and it is the same one method the event service
// exposes, so *event.Service satisfies it with nothing in between - no adapter,
// and therefore no second place where the two vocabularies could drift.
//
// A nil recorder means nothing is recorded. That is a legitimate configuration
// rather than a degraded one: a manager built without one manages runtimes
// exactly as well, and every test in this package that predates events builds
// one that way.
type EventRecorder interface {
	CreateEvent(
		ctx context.Context,
		projectID, runtimeID, eventType, source string,
		payload json.RawMessage,
	) (*event.AgentEvent, error)
}

// noteRuntimeEvent records a fact about a runtime.
//
// It never returns an error, and that is the contract rather than an oversight.
// The event it writes is a record of something that has already happened: the
// runtime has started, or stopped, or failed, and it is in that state whatever
// the database does next. A caller that could fail because the history write
// failed would make the history a precondition of the runtime, which is the one
// thing this layer must not be. Failures are logged with what identifies the
// event and are otherwise dropped.
//
// The payload is built by the caller as a map so that it is an object by
// construction, and so that nothing in this package is tempted to put a string
// where structured detail belongs.
func (m *Manager) noteRuntimeEvent(
	ctx context.Context,
	projectID, runtimeID, eventType string,
	payload map[string]any,
) {
	if m.events == nil {
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		m.log.Warn("could not encode a runtime event payload; the event is not recorded",
			"type", eventType, "projectId", projectID, "error", err)
		return
	}
	if _, err := m.events.CreateEvent(ctx, projectID, runtimeID, eventType, event.SourceRuntime, encoded); err != nil {
		// The runtime is in the state the event describes regardless. What is
		// lost is the record of how it got there.
		m.log.Warn("could not record a runtime event",
			"type", eventType, "projectId", projectID, "runtimeId", runtimeID, "error", err)
	}
}

// Runtime operations, named in an error event's payload.
//
// They are the three things that can fail on a runtime, and they exist so that
// a timeline says which one did. A row that said only "something failed" would
// send a reader to the logs for the one fact the event could have carried.
const (
	operationStart   = "start"
	operationStop    = "stop"
	operationDestroy = "destroy"
)

// failRuntime moves a runtime into ERROR and records that it happened.
//
// The two are one function so that they cannot drift apart. A runtime that is
// in the ERROR state and a `runtime.error` event are produced together and
// nowhere else, which makes that a property of the code rather than a
// convention somebody has to remember when adding a failure path.
//
// One failure deliberately does not come through here: a Start whose runtime
// came up and whose *record* could not be written. The terminal is running, and
// an error event there would contradict the `runtime.started` written in the
// same instant. That failure is logged, is returned to the caller, and leaves
// the runtime RUNNING, which is what it is.
func (m *Manager) failRuntime(ctx context.Context, rt *runtime, operation, message string) {
	rt.setState(StateError, message, m.now())
	m.noteRuntimeEvent(ctx, rt.projectID, rt.session, event.TypeRuntimeError,
		map[string]any{"operation": operation, "state": string(StateError)})
}
