// Package controller aggregates what a dashboard needs into one response.
//
// # What it is
//
// A read model, and nothing else. It owns no table, writes nothing, and holds no
// state of its own: it asks the services that already exist, arranges what they
// say, and hands it to a client. Delete the package and nothing is lost but the
// convenience.
//
//	existing services ──▶ aggregation ──▶ one JSON response
//
// # Why it exists
//
// Without it, a console has to call seven endpoints and join them itself:
//
//	GET /api/server
//	GET /api/projects
//	GET /api/projects/{id}/runtime
//	GET /api/projects/{id}/tasks
//	GET /api/projects/{id}/agent-states
//	GET /api/projects/{id}/attention
//	GET /api/projects/{id}/actions
//
// That is a client reimplementing a join, once per client, in whatever language
// it happens to be written in - and each of those calls is a request per project
// for the last four. The join belongs on the server, where it can be done in a
// handful of queries rather than a handful per project.
//
// # What it must never become
//
// A source. There is no controller table, no controller cache, and no state that
// survives a request. The moment this package stored something, it would be a
// second place the truth lives and the two would disagree - docs/CONTROLLER_API.md
// §1.
//
// It is also not a new API surface for anything but reading. There is no POST
// and no PATCH here: every route this package serves is a GET.
package controller

import (
	"sort"
	"time"
)

// Dashboard is the whole console in one response.
type Dashboard struct {
	// Server describes the machine and the build, which is what a console
	// header shows.
	Server ServerSummary `json:"server"`

	// Projects are the cards, most in need of attention first.
	Projects []ProjectCard `json:"projects"`

	// Count is how many cards there are, so a client does not have to count.
	Count int `json:"count"`
}

// ServerSummary is the part of the server's own report a console header needs.
//
// It is deliberately a handful of fields rather than the whole of
// `GET /api/server`. That endpoint is the diagnostics surface - tmux paths,
// dependency checks, the filesystem layout - and a dashboard showing all of it
// would be showing an operator's screen to every client. A client that wants
// the detail still has the endpoint it has always had.
type ServerSummary struct {
	// Status is "online". The server answering is the whole of the evidence.
	Status string `json:"status"`

	// RuntimeAvailable reports whether a terminal can run on this machine.
	RuntimeAvailable bool `json:"runtimeAvailable"`

	// RuntimeUnavailableReason explains the above when it is false, and is
	// empty when it is true.
	//
	// It is here rather than left to `GET /api/server` because a console that
	// shows a project as stopped has to be able to say whether that is the
	// project's fault or the machine's.
	RuntimeUnavailableReason string `json:"runtimeUnavailableReason,omitempty"`

	// Version is the build, for a console footer.
	Version string `json:"version"`

	// UptimeSeconds is how long this process has been up.
	UptimeSeconds int64 `json:"uptimeSeconds"`
}

// ProjectCard is one project, as a console shows it.
//
// Every field is a status, an identifier or a count. There is no room in it for
// a prompt, a transcript, terminal output, a command, a tool input, a token
// count or a credential, and their absence is the design: a dashboard says what
// is happening, and the things that would make it say more are the things that
// must not leave the machine. docs/CONTROLLER_API.md §6.
type ProjectCard struct {
	// ID and Name identify the project. The name is what a person chose or the
	// directory's own, and the id is what every other endpoint takes.
	ID   string `json:"id"`
	Name string `json:"name"`

	// Runtime is whether the project's terminal is up.
	Runtime RuntimeSummary `json:"runtime"`

	// Agent is what the agent is doing, or null.
	//
	// It describes the project's most recent attempt. A project no agent has
	// ever run in reports null rather than a status it does not have - see
	// docs/CONTROLLER_API.md §3.
	Agent *AgentSummary `json:"agent"`

	// Attention is whether anybody needs to look, or null.
	Attention *AttentionSummary `json:"attention"`

	// Actions is how much is waiting.
	Actions ActionsSummary `json:"actions"`

	// UpdatedAt is when anything about this card last changed.
	//
	// It is the most recent of the project's own update, its agent state's and
	// its attention's, so a client can sort or badge on one field rather than
	// three.
	UpdatedAt time.Time `json:"updatedAt"`
}

// RuntimeSummary is whether a project's terminal is up.
//
// The status is the project model's own vocabulary, spelled exactly as that
// model spells it - "running", "stopped", "error" and the rest. It is passed
// through rather than translated: a second spelling of the same value would be
// a second thing to keep in step, and a client that knows one endpoint's
// vocabulary should not have to learn this one's.
type RuntimeSummary struct {
	Status string `json:"status"`
}

// AgentSummary is what the agent in a project is doing.
//
// The status and the event are the agent state projection's own vocabulary.
// `status` is one of CREATED, RUNNING, WAITING_INPUT, WAITING_PERMISSION,
// COMPLETED, FAILED or STOPPED; `lastEvent` is the type of the event that last
// moved it, and never a payload.
type AgentSummary struct {
	// Available reports whether the server can answer this at all.
	//
	// It is false when the build has no agent state projection - which is a
	// deployment fact rather than a project one, and the reason a card says
	// "unavailable" rather than "nothing is running".
	Available bool `json:"available"`

	// SessionID is the attempt this describes.
	SessionID string `json:"sessionId,omitempty"`

	// Status is what the agent is doing.
	Status string `json:"status,omitempty"`

	// LastEvent is the type of the event that last moved it.
	LastEvent string `json:"lastEvent,omitempty"`

	// UpdatedAt is when that event happened.
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
}

// AttentionSummary is whether anybody needs to look at a project.
type AttentionSummary struct {
	// Available reports whether the server can answer this at all, for the
	// reason AgentSummary.Available gives.
	Available bool `json:"available"`

	// Level is one of NONE, ACTION_REQUIRED, WARNING or INFO.
	Level string `json:"level"`

	// Reason is a short fixed phrase saying why, never a quotation.
	Reason string `json:"reason,omitempty"`
}

// ActionsSummary is how much is waiting in a project.
//
// It is a count and not a list. A console shows a badge; the queue itself is
// read from `GET /api/projects/{id}/actions`, and putting it here would make
// every dashboard request carry every pending action of every project.
type ActionsSummary struct {
	// Available reports whether the server can answer this at all.
	Available bool `json:"available"`

	// Pending is how many actions are still waiting.
	//
	// It counts the whole project rather than its most recent attempt: an
	// action is a backlog, and a backlog is something a project has.
	Pending int `json:"pending"`
}

// attentionRank orders the attention levels by how much they demand of a
// person.
//
// It is the sort key §6 of the phase brief asks for, and it is stated here
// rather than derived from the level's spelling because the two would disagree:
// alphabetically ACTION_REQUIRED, INFO, NONE, WARNING, which is not an order
// anybody wants a console in.
func attentionRank(level string) int {
	switch level {
	case "ACTION_REQUIRED":
		return 3
	case "WARNING":
		return 2
	case "INFO":
		return 1
	default:
		return 0
	}
}

// agentRank orders the agent statuses by how much they are doing.
//
// It is the second half of the sort: when nothing needs a person, the console
// still puts what is working above what has finished and what has finished
// above what is idle.
func agentRank(status string) int {
	switch status {
	case "RUNNING", "WAITING_PERMISSION", "WAITING_INPUT":
		return 2
	case "COMPLETED":
		return 1
	default:
		return 0
	}
}

// cardRank is where a project belongs in the console.
//
// Lower sorts first, and the order is §6 of the phase brief: what needs doing,
// then what went wrong, then what is working, then what has finished, then
// everything else.
func cardRank(c ProjectCard) int {
	// A section that is unavailable ranks as though it were absent, so a
	// server without the attention projection still sorts its working projects
	// above its idle ones rather than flattening every card together.
	if c.Attention != nil && c.Attention.Available {
		switch rank := attentionRank(c.Attention.Level); rank {
		case 3:
			return 0 // ACTION_REQUIRED
		case 2:
			return 1 // WARNING
		}
	}
	if c.Agent != nil && c.Agent.Available {
		switch agentRank(c.Agent.Status) {
		case 2:
			return 2 // working
		case 1:
			return 3 // finished
		}
	}
	return 4 // idle, or nothing to say
}

// sortCards orders the cards the way a console shows them.
//
// The order is total and stable: rank first, then the most recently changed,
// then the name, then the id. The id is the last resort and is never the reason
// two cards are in the order they are - §6 forbids sorting by it - but it is
// there so that two projects sharing a name and a change time still come back in
// the same order twice.
func sortCards(cards []ProjectCard) {
	sort.SliceStable(cards, func(i, j int) bool {
		a, b := cards[i], cards[j]
		if ra, rb := cardRank(a), cardRank(b); ra != rb {
			return ra < rb
		}
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
}
