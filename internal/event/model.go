// Package event implements the AgentMux agent event foundation.
//
// An event records that something happened. It is not a status and not a
// current value: the questions this package answers are "what has happened to
// this project" and "when", and every question of the form "what is true now"
// belongs to the layer that owns the thing being asked about.
//
//	RuntimeManager   owns  what is true now      session.Runtime, project_runtime
//	Event Service    owns  what happened         agent_events, append-only
//
// Neither replaces the other, and the boundary between them is the whole design
// of this package. `docs/AGENT_EVENTS.md` is the long form.
//
// Rows are append-only. Nothing here updates or deletes one and no API could: a
// history that can be edited is not a history. That is also why the payload is
// bounded and why no error string is ever copied into one - a row that can
// never be changed can never be redacted either.
package event

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// IDPrefix marks an event identifier in a log line or a URL.
const IDPrefix = "evt_"

// idRandomBytes is the entropy per identifier: 16 bytes, 128 bits, the same
// entropy a version-4 UUID carries.
//
// It is not a UUID. Every other identifier in AgentMux is "prefix_hex" - "p_"
// for a project, "inst_" for an installation - and a second identifier format
// in the same product is a second thing for a reader to recognise. The entropy
// is what makes the identifier unique before it is stored, which is what lets
// an event be logged or handed to a client before it is written; the spelling
// is not.
const idRandomBytes = 16

// idBodyLen is the number of hex characters in the random part of an ID.
const idBodyLen = idRandomBytes * 2

// NewID returns a fresh event identifier.
//
// It is generated rather than assigned by the database, because identity the
// storage engine hands out is identity only the storage engine knows: two
// installations writing to two databases cannot agree on an integer.
func NewID() (string, error) {
	buf := make([]byte, idRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("event: generate id: %w", err)
	}
	return IDPrefix + hex.EncodeToString(buf), nil
}

// ValidID reports whether id has the shape NewID produces.
func ValidID(id string) bool {
	if !strings.HasPrefix(id, IDPrefix) {
		return false
	}
	body := id[len(IDPrefix):]
	if len(body) != idBodyLen {
		return false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// Event types this build emits.
//
// The vocabulary is closed by what emits it, not by a list: a type that no code
// produces is not declared here, because a declared type that nothing writes
// reads as a feature that exists. A later phase adds its types beside these.
const (
	// TypeRuntimeStarted means a project's runtime reached RUNNING.
	TypeRuntimeStarted = "runtime.started"

	// TypeRuntimeStopped means a runtime was stopped, keeping its session and
	// its scrollback.
	TypeRuntimeStopped = "runtime.stopped"

	// TypeRuntimeError means a start, a stop or a destroy failed.
	TypeRuntimeError = "runtime.error"

	// TypeRuntimeDestroyed means a runtime and its session were removed.
	TypeRuntimeDestroyed = "runtime.destroyed"
)

// Event sources. This set is closed by the domain, and it is validated at the
// write: a misspelled source is an event no filter ever matches, which is worse
// than a rejection at the moment the mistake was made.
const (
	// SourceRuntime is the runtime manager observing something about a session.
	SourceRuntime = "runtime"

	// SourceUser is a person asking for something through the API.
	SourceUser = "user"

	// SourceSystem is AgentMux acting on nobody's behalf: a reconciliation, a
	// shutdown.
	SourceSystem = "system"

	// SourceAgent is the coding agent inside a runtime.
	SourceAgent = "agent"
)

// Sources lists every source, in the order they are declared.
func Sources() []string {
	return []string{SourceRuntime, SourceUser, SourceSystem, SourceAgent}
}

// ValidSource reports whether s is one of the declared sources.
func ValidSource(s string) bool {
	switch s {
	case SourceRuntime, SourceUser, SourceSystem, SourceAgent:
		return true
	default:
		return false
	}
}

// Type shape bounds.
//
// The shape is validated rather than the vocabulary, because the vocabulary of
// "what can happen" belongs to the layers that find out: an allowlist here would
// make adding a type in a later phase a change to this package. What the check
// does catch is the mistake that would actually be made - a caller passing a
// sentence, or a status value, where a type belongs.
const (
	// minTypeLen is "a.b", the shortest well-formed type.
	minTypeLen = 3

	// maxTypeLen bounds a type so that a stray document cannot become one.
	maxTypeLen = 64
)

// ValidType reports whether t is a well-formed event type: a dotted namespace
// of lowercase segments, each starting with a letter.
//
//	"runtime.started"      valid
//	"runtime"              not valid - a type names a namespace and an action
//	"Runtime.Started"      not valid - types are lowercase
//	"runtime..started"     not valid
//	"runtime started"      not valid
func ValidType(t string) bool {
	if len(t) < minTypeLen || len(t) > maxTypeLen {
		return false
	}
	segments := strings.Split(t, ".")
	if len(segments) < 2 {
		return false
	}
	for _, segment := range segments {
		if !validTypeSegment(segment) {
			return false
		}
	}
	return true
}

// validTypeSegment reports whether one dot-separated part of a type is
// well-formed: non-empty, starting with a lowercase letter, and otherwise
// lowercase letters, digits and underscores.
func validTypeSegment(segment string) bool {
	if segment == "" {
		return false
	}
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9', c == '_':
			// Digits and underscores are allowed after the first character,
			// which is checked below.
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// AgentEvent is one thing that happened.
//
// The struct is immutable in practice: a stored event is never updated, and
// nothing in this package writes to a field of one that has been read back.
type AgentEvent struct {
	// ID is the event's identity, "evt_" followed by 128 random bits in hex.
	//
	// It is unique before it is stored, so an event can be logged or handed to
	// a client without the database having seen it yet.
	ID string `json:"id"`

	// ProjectID is the project this happened to. Always present.
	ProjectID string `json:"projectId"`

	// RuntimeID is the runtime this happened to, or empty when the event is
	// about the project rather than about a runtime.
	//
	// In this build it holds the runtime's session name, which is the name the
	// runtime has on the host and the one the runtime manager reconciles and
	// destroys by. It is stored per event rather than derived from the project
	// id at read time: it is derivable today, and storing it means a change to
	// the naming rule cannot make a stored row misleading.
	RuntimeID string `json:"runtimeId,omitempty"`

	// Type is what happened, in a dotted namespace. See the Type* constants.
	Type string `json:"type"`

	// Source is who or what produced it. See the Source* constants.
	Source string `json:"source"`

	// Payload is structured detail, or nil. It is always a JSON object.
	//
	// It is deliberately small - a fact, not a document - and it is the one
	// field a caller gets to shape, which is why it is the one field that is
	// checked for the things an immutable row must never contain. See
	// CheckPayload.
	Payload json.RawMessage `json:"payload,omitempty"`

	// CreatedAt is when it happened, in UTC.
	//
	// Stored as RFC3339 with nanoseconds so the database file stays readable
	// with any SQLite tool. Converting to a local time zone is the display
	// layer's job and is not done here.
	CreatedAt time.Time `json:"createdAt"`
}

// String renders an event for a log line, without its payload.
//
// The payload is left out on purpose. It is the only field whose content a
// caller chose, and a log is the easiest place for something that should not
// have been stored to end up instead - and unlike the row, a log line has no
// key check in front of it. What is logged is what identifies the event: which
// event, what happened, to which project.
func (e *AgentEvent) String() string {
	if e == nil {
		return "event(nil)"
	}
	if e.RuntimeID != "" {
		return fmt.Sprintf("event(%s %s project=%s runtime=%s)",
			e.ID, e.Type, e.ProjectID, e.RuntimeID)
	}
	return fmt.Sprintf("event(%s %s project=%s)", e.ID, e.Type, e.ProjectID)
}
