package claude

import (
	"context"
	"encoding/json"

	"github.com/kutonlagos/agentmux/internal/event"
)

// This file is the adapter's half of the event bridge: the one place a Claude
// observation becomes a row in the event log.
//
// It follows the shape internal/session/events.go established in Phase 7.1 and
// internal/task/events.go repeated in Phase 7.2, and it holds the same
// boundary. This package imports the event package for the vocabulary of what
// it may report and for nothing else: it never reads an event, never queries
// one, and never decides anything from one. If the recorder were removed, the
// adapter would observe exactly as much and record nothing, and no other
// behaviour would change.
//
// There is no path from here to a database. The adapter does not import
// internal/storage and does not know that SQLite exists; the only way a row is
// written is the one method below, which is the event service's. A second route
// to the table is the thing this boundary exists to prevent.

// EventRecorder receives the facts the adapter observes.
//
// It is one method wide, and it is the same one method the event service
// exposes, so *event.Service satisfies it with nothing in between - the same
// interface internal/session and internal/task declare, spelled the same way,
// so that there is no adapter and therefore no second place where the
// vocabularies could drift.
//
// A nil recorder means nothing is recorded, and that is a legitimate
// configuration rather than a degraded one: an adapter built without one
// observes sessions exactly as well, and a test can exercise the whole of the
// mapping without a database.
type EventRecorder interface {
	CreateEvent(
		ctx context.Context,
		projectID, runtimeID, eventType, source string,
		payload json.RawMessage,
	) (*event.AgentEvent, error)
}

// noteEvent records one observation, when it is one worth recording.
//
// It never returns an error, and that is the contract rather than an oversight,
// for the reason internal/session/events.go gives: the thing being recorded has
// already happened. Claude started a session, submitted a prompt, asked for a
// permission - it is in that state whatever the database does next, and a hook
// receiver that could fail because a history write failed would answer Claude
// with an error over something Claude is not asking about.
//
// It is the only caller of the recorder, so the source, the correlation and the
// decision of whether to record at all are applied in one place.
func (a *Adapter) noteEvent(ctx context.Context, ev Event) {
	if a.recorder == nil {
		return
	}
	eventType, record := agentEventType(ev.Kind, ev.Payload)
	if !record {
		// An observation that is not an event. It has been published to
		// subscribers already, which is where a caller who wants the raw
		// stream of observations reads it.
		return
	}
	if _, err := a.recorder.CreateEvent(
		ctx, ev.ProjectID, ev.RuntimeID, eventType, event.SourceAgent, ev.Payload,
	); err != nil {
		// The session is in the state the event describes regardless. What is
		// lost is the record of it, and the log line says which one.
		a.log.Warn("could not record a claude event",
			"type", eventType, "kind", string(ev.Kind),
			"projectId", ev.ProjectID, "runtimeId", ev.RuntimeID, "error", err)
	}
}
