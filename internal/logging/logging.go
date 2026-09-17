// Package logging builds the structured logger used across AgentMux.
//
// Secret-handling rule: AgentMux never logs request or response bodies,
// request headers, provider credentials, API keys, or tokens. The HTTP
// middleware records only method, route, status, and duration. Anything that
// might carry a credential must be passed through Redact before it reaches a
// log record.
package logging

import (
	"io"
	"log/slog"
	"strings"
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
func New(level, format string, w io.Writer) *slog.Logger {
	h := NewHandler(level, format, w)
	return slog.New(h)
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
