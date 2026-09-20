// Package agent binds a coding agent running in a runtime to the AgentMux
// attempt it is, and to the events it produces.
//
// # What it is for
//
// The three parts this joins already exist and none of them knows about the
// others. The runtime manager owns a terminal and can start a program in it.
// The Claude adapter can observe a session and write what it sees to the event
// log. The task model records attempts. What none of them can do is decide that
// this attempt is that process in that terminal - and that decision is this
// package, and nothing else in the build makes it.
//
//	Project ──▶ Runtime ──▶ AgentSession ──▶ Claude session_id ──▶ agent.* events
//
// # The one thing it decides
//
// Which Claude session id a runtime is running under. AgentMux generates it,
// hands it to the CLI on the command line, and gives the same value to the
// adapter; from then on every hook payload names itself and nothing has to be
// matched on a directory, a process name or a clock. docs/AGENT_RUNTIME_BINDING.md
// §4 is the long form, and the reason it matters is that a correlation which is
// wrong is a correlation that can never be corrected.
//
// # What it deliberately does not do
//
// It does not own the process. It starts nothing itself: it asks the runtime to,
// through the runtime manager's own operations, and it decides only when to ask.
// It holds no database handle and writes no row directly - the attempt is
// created through the task service and the events through the event service,
// which remain the only doors into either table.
//
// It does not decide anything about a turn. A permission request is recorded and
// left alone; `agent.completed_candidate` is a candidate and is not treated as a
// completion; a task's status is never moved. Deciding belongs to a later phase
// with its own threat model.
package agent

import (
	"time"

	"github.com/kutonlagos/agentmux/internal/session"
	"github.com/kutonlagos/agentmux/internal/task"
)

// Run is one observed attempt: a Claude session in a runtime, bound to the
// AgentMux record of the work it is doing.
//
// It is the coordinator's binding, held in memory for as long as the adapter is
// attached. It is not stored, and docs/AGENT_RUNTIME_BINDING.md §6 says why and
// what a later phase would have to add to keep it across a restart.
type Run struct {
	// ProjectID is the project the runtime belongs to.
	ProjectID string

	// RuntimeID is the runtime the session runs in.
	RuntimeID string

	// AgentSessionID is the agent_sessions row this attempt is, or empty when
	// the caller named no task and only the observation was wanted.
	AgentSessionID string

	// SessionID is the Claude session id, chosen by AgentMux. It is deliberately
	// not serialised: it is the one value here that is not AgentMux's to
	// publish, and it is never written to the event log.
	SessionID string

	// StartedAt is when the launch was requested.
	StartedAt time.Time
}

// Outcome is how an attempt ended.
//
// It is an enumeration rather than a status because the statuses belong to the
// task model and this package must not restate them; the translation happens
// once, in the function that writes it.
type Outcome string

// The ways an attempt can end.
const (
	// OutcomeCompleted means the session ended by itself, which is what the
	// `SessionEnd` hook reports.
	//
	// It is not a claim that the work succeeded. Claude Code fires no hook that
	// means "the task is done", and this build cannot reach the `result`
	// envelope that would say so - docs/AGENT_RUNTIME_BINDING.md §5. A completed
	// attempt is one that ran to its own end.
	OutcomeCompleted Outcome = "completed"

	// OutcomeCancelled means somebody asked for it to stop: `agent/stop`, or a
	// runtime stopped with the agent still in it.
	OutcomeCancelled Outcome = "cancelled"

	// OutcomeFailed means it broke or its environment was taken away: a launch
	// that never came up, or a runtime that was destroyed.
	OutcomeFailed Outcome = "failed"
)

// StartInput is a request to start an agent and record the attempt.
type StartInput struct {
	// ProjectID is the project whose runtime the agent runs in. Required.
	ProjectID string

	// TaskID is the task this is an attempt at, when the caller wants the
	// attempt recorded.
	//
	// Empty is a legitimate request: it asks for an observed agent and no
	// record, which is what a caller that has no task wants. It is not a
	// default - nothing is inferred from a task that exists, because an attempt
	// is a statement that somebody started this work, and a server cannot make
	// that statement on a person's behalf.
	TaskID string
}

// StopInput is a request to stop an agent.
type StopInput struct {
	// ProjectID is the project whose agent should stop. Required.
	ProjectID string
}

// Result is what a start or stop produced.
//
// It carries the runtime manager's own report rather than a summary of it, so
// that a caller reads the same agent state it would have read from the runtime
// API and there is only one description of what an agent is doing.
type Result struct {
	// Agent is the runtime manager's report on the agent process.
	Agent session.AgentStatus

	// Run is the binding, when there is one. It is absent for an agent that was
	// adopted rather than started, and after a stop.
	Run *Run

	// Session is the attempt record, when the caller named a task.
	Session *task.AgentSession

	// Adopted reports that the agent was already running and nothing was
	// launched. The launch the caller asked for did not happen, so no session id
	// was dictated and nothing was bound.
	Adopted bool

	// RuntimeStarted reports that this call started the runtime, which it does
	// only when it found none running.
	RuntimeStarted bool
}
