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
// Phase 0 (foundation), Phase 1 (project model and server foundation), and
// Phase 2 (persistent session runtime) are implemented. There is still no Web
// Terminal: sessions are real and persistent, and the UI can start and stop
// them, but nothing streams their output to a browser yet.
const Phase = "Phase 2 - Persistent session runtime"

// TerminalRuntimeImplemented reports whether this build can run persistent
// terminal sessions.
//
// It is a statement about the software, not about the machine it is running on.
// A build with this true, started on a Windows host outside WSL, still has no
// usable runtime, and GET /api/server reports that separately as
// runtimeAvailable. A client needs both to decide whether to offer a terminal.
const TerminalRuntimeImplemented = true
