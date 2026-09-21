// Package logging builds the structured logger used across AgentMux.
//
// # The record shape
//
// Every record carries four things: a timestamp, a level, a component, and an
// event. The first two come from slog's handler; the component is attached with
// Component and names the subsystem the record came from; the event is the
// message, and it says what happened, in the present tense, without a subject:
//
//	time=2026-09-19T10:04:11.882Z level=INFO msg="runtime started" component=session projectId=prj_7f3a session=amx-prj_7f3a
//
// Structured attributes carry the detail - identifiers, counts, durations,
// outcomes. Identifiers rather than names, because a project may be renamed and
// a log line that quoted the old name would describe a project that no longer
// exists under it.
//
// # What is never logged
//
//   - Terminal output. A pane's bytes are the program's output, not AgentMux's
//     event, and they may contain anything the user's program printed. Where a
//     record needs to say something about a block of terminal bytes it says how
//     many there were, not what they were.
//   - Anything the user typed, and any prompt or conversation content.
//   - Credentials: API keys, tokens, passwords, provider configuration,
//     Authorization headers, request or response bodies.
//
// The HTTP middleware records only method, path, status, byte count and
// duration. The path is logged without its query string, which can carry a
// token. Anything that might carry a credential must be passed through Redact
// before it reaches a log record.
//
// # Rotation
//
// There is no rotation here, deliberately. AgentMux writes to stderr and the
// supervisor owns the file: under systemd that is the journal, which rotates on
// its own, and anywhere else it is logrotate. A logger that also rotated would
// be a second thing deciding when a file ends. docs/DEPLOYMENT.md §Logs has
// both configurations.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// ComponentField is the attribute name that names the subsystem a record came
// from. It is a constant because the field is part of the record's contract:
// an operator greps for it, and a query that spelled it "component" while the
// writer spelled it "Component" would silently match nothing.
const ComponentField = "component"

// AgentMux subsystem names, as they appear in the component field.
//
// They follow the package layout, so that a record can be traced back to the
// code that wrote it without a lookup table.
const (
	ComponentConfig  = "config"
	ComponentStorage = "storage"
	ComponentHost    = "host"
	ComponentProject = "project"
	ComponentRuntime = "session"
	ComponentEvent   = "event"
	ComponentTask    = "task"
	ComponentAgent   = "claude"
	// ComponentAgentState is the projection of the event log into what is true
	// about an agent now. It is named after its package rather than after the
	// agent, because a record about a projection is not a record about Claude:
	// it says what the log added up to, which is a different question and a
	// different component.
	ComponentAgentState = "agentstate"
	// ComponentAttention is the projection of the event log into whether
	// anybody needs to look at an agent. It is a component of its own rather
	// than part of agentstate, because a record about attention is a record
	// about the relationship between an agent and a person, not about the
	// agent.
	ComponentAttention = "attention"
	// ComponentController is the aggregation a console reads. It is a component
	// of its own because a record about it is about assembling an answer from
	// other components rather than about any of them.
	ComponentController = "controller"
	ComponentTerminal   = "terminal"
	ComponentAPI        = "httpapi"
	ComponentServer     = "server"
)

// Placeholder is written in place of any value that must not reach a log.
const Placeholder = "[redacted]"

// Supported level names, as accepted by config.LoggingConfig.Level.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Supported formats, as accepted by config.LoggingConfig.Format.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// New builds a slog.Logger from AgentMux logging configuration.
//
// Unrecognised level or format values fall back to info/text rather than
// failing, because configuration validation already reports them as errors;
// logging must still work while that error is being reported.
//
// The logger it returns carries no component. Every subsystem derives its own
// with Component, so that a record says where it came from without the writer
// having to remember to add the field.
func New(level, format string, w io.Writer) *slog.Logger {
	h := NewHandler(level, format, w)
	return slog.New(h)
}

// Component returns a logger that tags every record it writes with the
// subsystem it came from.
//
// It is a With rather than a per-record attribute, which means the tag is
// attached once, at the point the subsystem's logger is built, and cannot be
// forgotten at an individual call site.
//
// A nil logger is answered with slog.Default() with the tag applied rather than
// a panic: a component that was handed no logger should still produce records,
// and the alternative is a nil dereference in the one code path - error
// handling - that is hardest to reproduce.
func Component(logger *slog.Logger, name string) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return logger.With(ComponentField, strings.TrimSpace(name))
}

// NewHandler builds the slog.Handler for the given level and format.
func NewHandler(level, format string, w io.Writer) slog.Handler {
	opts := &slog.HandlerOptions{Level: ParseLevel(level)}
	if strings.EqualFold(strings.TrimSpace(format), FormatJSON) {
		return slog.NewJSONHandler(w, opts)
	}
	return slog.NewTextHandler(w, opts)
}

// ParseLevel maps a configuration string to a slog.Level. Unknown values map
// to slog.LevelInfo.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case LevelDebug:
		return slog.LevelDebug
	case LevelWarn, "warning":
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ValidLevel reports whether s is a level name AgentMux accepts.
func ValidLevel(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case LevelDebug, LevelInfo, LevelWarn, "warning", LevelError:
		return true
	default:
		return false
	}
}

// ValidFormat reports whether s is a log format AgentMux accepts.
func ValidFormat(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case FormatText, FormatJSON:
		return true
	default:
		return false
	}
}

// Redact returns the redaction placeholder when value is non-empty, and the
// empty string otherwise.
//
// Use it for any field whose presence matters but whose content must not be
// recorded.
func Redact(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return Placeholder
}
