// Package version exposes build identity for the AgentMux server.
//
// The values here are reported by GET /api/server so that a client can tell
// which roadmap phase a running server actually implements, instead of
// assuming capabilities from the client's own version.
package version

// AppName is the product name reported by GET /api/server.
const AppName = "AgentMux"

// Version is the AgentMux release version.
const Version = "0.1.0"

// Phase names the roadmap phase this build implements.
//
// Phase 0 (foundation) and Phase 1 (project model and server foundation) are
// the only phases implemented so far. No terminal runtime exists yet.
const Phase = "Phase 1 - Project model and server foundation"

// TerminalRuntimeImplemented reports whether this build can run persistent
// terminal sessions. It is false until Phase 2 lands, and exists so that the
// API can be explicit rather than letting a client guess.
const TerminalRuntimeImplemented = false
