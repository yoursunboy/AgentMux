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
// Phase 0 (foundation), Phase 1 (project model and server foundation), Phase 2
// (persistent session runtime), Phase 3 (real Claude Code runtime) and Phase 4
// (web terminal) are implemented. A project's runtime can host the real Claude
// Code CLI, and that terminal is now in a browser: it is resolved, launched in
// the project's own directory, observed through the process table, streamed
// over GET /api/ws, and it survives a server restart. What Phase 4 did not add
// is a controller, a viewer, or a lease - every subscription is equal - and the
// multi-project grid is Phase 5.
const Phase = "Phase 4 - Web terminal"

// TerminalRuntimeImplemented reports whether this build can run persistent
// terminal sessions.
//
// It is a statement about the software, not about the machine it is running on.
// A build with this true, started on a Windows host outside WSL, still has no
// usable runtime, and GET /api/server reports that separately as
// runtimeAvailable. A client needs both to decide whether to offer a terminal.
const TerminalRuntimeImplemented = true
