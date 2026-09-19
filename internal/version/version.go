// Package version exposes build identity for the AgentMux server.
//
// The values here are reported by GET /api/server and by GET /health so that a
// client can tell which roadmap phase a running server actually implements,
// instead of assuming capabilities from the client's own version.
package version

import "strings"

// AppName is the product name reported by GET /api/server.
const AppName = "AgentMux"

// Version is the AgentMux release version.
//
// The scheme is major.minor.patch, where the minor tracks the roadmap phase:
// 0.6.5 is the sixth phase, shipped as a stabilization release. It is bumped by
// hand when a phase is tagged, not derived from the commit, because the number
// answers "which phase is this" and a commit count answers "how many commits".
const Version = "0.6.5"

// Commit is the git commit this binary was built from, set at link time:
//
//	go build -ldflags "-X github.com/kutonlagos/agentmux/internal/version.Commit=$(git rev-parse HEAD)"
//
// It is a variable rather than a constant because the value is not knowable
// until the link step, and it is empty in a plain `go build` or `go run` -
// which is the honest answer for a binary built from a working tree rather
// than from a commit.
var Commit = ""

// BuildDate is when the binary was linked, in RFC 3339, set at link time the
// same way Commit is. Empty for a binary that was not built by the release
// script, which sets it.
var BuildDate = ""

// Phase names the roadmap phase this build implements.
//
// Phase 0 (foundation), Phase 1 (project model and server foundation), Phase 2
// (persistent session runtime), Phase 3 (real Claude Code runtime), Phase 4
// (web terminal), Phase 5 (multi-project workspace) and Phase 6 (controller and
// viewer ownership) are implemented. A project's runtime can host the real
// Claude Code CLI, that terminal is in a browser, several of them are on screen
// at once, and exactly one client holds a project's lease at a time.
//
// Phase 6.5 adds no product capability. It is the deployment phase: a checked-in
// configuration example, a systemd unit, a health endpoint, a logging rule, a
// backup and upgrade procedure, and the documentation that makes them usable by
// somebody who did not write the code.
const Phase = "Phase 6.5 - Stabilization"

// TerminalRuntimeImplemented reports whether this build can run persistent
// terminal sessions.
//
// It is a statement about the software, not about the machine it is running on.
// A build with this true, started on a Windows host outside WSL, still has no
// usable runtime, and GET /api/server reports that separately as
// runtimeAvailable. A client needs both to decide whether to offer a terminal.
const TerminalRuntimeImplemented = true

// ShortCommit is the first twelve characters of Commit, or "" when the binary
// carries no commit.
//
// Twelve is what `git log --oneline` prints and what a person can compare by
// eye against a repository; the full hash is available in Commit for anything
// that needs to match it exactly.
func ShortCommit() string {
	c := strings.TrimSpace(Commit)
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// String is the version, for a log line or a page footer: "AgentMux v0.6.5".
func String() string {
	return AppName + " v" + Version
}

// StringWithCommit is String plus the commit when the build has one, and String
// alone when it does not: "AgentMux v0.6.5 (a1b2c3d4e5f6)".
//
// The commit is included rather than always shown as a placeholder, because a
// build with no commit is a real state - `go build` from a working tree - and a
// printed "(unknown)" would suggest a build step had failed.
func StringWithCommit() string {
	s := String()
	if c := ShortCommit(); c != "" {
		s += " (" + c + ")"
	}
	return s
}
