// Package usage records the beta usage events.
//
// Five things the server did are counted, and nothing else. The vocabulary is
// closed, the payload is absent, and both of those are properties of the types
// rather than rules a caller is asked to respect:
//
//	EventType   a named type whose Valid refuses anything not in the five
//	Event       three fields, one of which is that type, and no field for detail
//
// # Why the shape is the guarantee
//
// The rule this package exists to keep is that no terminal content, no prompt,
// no input and no Claude output is ever recorded. There are two ways to keep
// it: filter for the strings that would indicate a leak, or build somewhere a
// leak cannot go. This is the second. A filter is a list of the ways somebody
// thought of, kept correct by everybody who comes after remembering to extend
// it; a struct with three fields and a table with three columns cannot hold a
// transcript however the caller is written.
//
// internal/attention takes the same position for its own rows, where the
// longest reason is twenty characters and the column is bounded at 64, and
// agent_attention's migration says why. This package makes the same argument
// one step stronger: the reason there is a fixed phrase rather than a
// quotation, and the event here is a name rather than a phrase.
//
// # What this is not
//
// It is not analytics and it is not an audit trail. There is no actor, no
// project, no session and no device in the record, so no query can answer "who
// did what". It is a count of how often five code paths ran, and it is
// permitted to be wrong: nothing derives anything from it, and deleting every
// row loses a number and nothing else.
package usage

import (
	"context"
	"time"

	"github.com/kutonlagos/agentmux/internal/idgen"
)

// EventType is one of the five things the beta records.
//
// It is a named type rather than a string so that the vocabulary is closed as
// far as the compiler can close it, and Valid is the half the compiler cannot
// reach: a value converted from a string - from a request, from a log line,
// from a caller that built one - is refused by the service before a row is
// written. Without that, the named type would be a suggestion, and the day
// somebody passed a formatted string would be the day this table started
// holding one.
type EventType string

const (
	// EventDashboardOpen is a browser asking for the console page.
	//
	// It is recorded where the server serves the page, not by the page: a
	// beacon from the client would be a second write path, a second thing to
	// secure, and a claim from the far end of the connection that this server
	// has no way to check.
	EventDashboardOpen EventType = "dashboard.open"

	// EventTerminalConnect is a browser subscribing to a project's terminal.
	//
	// One per subscription, and a re-subscribe over a socket that already has
	// one is not a second: the transport returns the existing subscription, so
	// the event counts a browser attaching rather than a frame arriving.
	EventTerminalConnect EventType = "terminal.connect"

	// EventControllerRequest is a client asking for a project's control lease.
	//
	// Recorded on the attempt, including a refusal. A refusal is a person
	// pressing a button and finding the terminal busy, which is exactly the
	// thing a beta wants to know the frequency of.
	EventControllerRequest EventType = "controller.request"

	// EventControllerRelease is a client giving a project's control lease up.
	//
	// Recorded only on a release that happened. A refusal here is a client
	// asking to release something it never held, which is a bug in the client
	// rather than something a person did. The asymmetry with
	// EventControllerRequest is deliberate.
	EventControllerRelease EventType = "controller.release"

	// EventActionView is a browser asking for one action's page.
	//
	// One action's page, not the queue. The queue is the console's list and is
	// not counted: see usageEventForPath in internal/httpapi.
	EventActionView EventType = "action.view"
)

// EventTypes is the vocabulary, in the order this package declares it.
//
// It exists so that a test can assert the set is exactly what Valid accepts,
// and so that a reader has one place to see the whole of what is recorded. It
// is a copy for reading; Valid is the authority.
var EventTypes = []EventType{
	EventDashboardOpen,
	EventTerminalConnect,
	EventControllerRequest,
	EventControllerRelease,
	EventActionView,
}

// maxEventTypeLen bounds an event type in storage.
//
// It is not a formatting preference. The longest constant above is seventeen
// characters, and a value approaching this is not one of them. The bound is what
// keeps the column from becoming a place a formatted string could live even if
// a future bug reached the store without passing Valid - it is the second lock
// on the same door, and the first is that there is nothing to format.
const maxEventTypeLen = 64

// Valid reports whether t is one of the five events.
func (t EventType) Valid() bool {
	switch t {
	case EventDashboardOpen, EventTerminalConnect, EventControllerRequest,
		EventControllerRelease, EventActionView:
		return true
	default:
		return false
	}
}

// String is the event type as it is stored.
func (t EventType) String() string { return string(t) }

// eventIDSpec is the shape of a usage event's identifier.
//
// Eight bytes rather than sixteen, and the difference from every other
// identifier in this codebase is the point: nothing addresses a usage event. It
// has no URL, it is not a cursor, no endpoint reads it and no client ever sees
// it. Contracting with the shortest width this package allows is the honest
// choice for an identifier of record, and sixteen bytes would be a claim of
// reach that nothing makes.
var eventIDSpec = idgen.Spec{Prefix: "use_", Bytes: 8}

// Event is one recorded usage event.
//
// Three fields, because the table has three columns and the phase brief names
// them. There is no Payload, no Detail, no Context and no Metadata field, and
// the absence is the design rather than an omission: a struct with nowhere to
// put a prompt cannot record one. A test asserts this type has exactly three
// exported fields, so that a fourth added later fails the build's tests rather
// than the promise.
type Event struct {
	// ID is the event's identity, generated before the row exists.
	ID string `json:"id"`

	// Type is one of the five, and is the only thing that varies.
	Type EventType `json:"eventType"`

	// CreatedAt is when the server recorded it, in UTC.
	CreatedAt time.Time `json:"createdAt"`
}

// Repository is what the service needs from storage.
type Repository interface {
	// Insert records one event.
	Insert(ctx context.Context, e Event) error

	// Count reports how many are stored.
	//
	// It exists for tests and for whatever reads this table next. Nothing in
	// this phase reads it over HTTP, and there is deliberately no endpoint that
	// does.
	Count(ctx context.Context) (int, error)
}

// Recorder is the one-method view of this package that a producer holds.
//
// It is declared here rather than at each consumer because the producers live
// in two packages - internal/httpapi and internal/terminal - and a copy in each
// is two places the width of the interface could drift. It is one method wide
// so that a consumer's nil check is the whole of the opt-out: a producer wired
// without a recorder records nothing and behaves identically otherwise, which
// is the same rule internal/task's EventRecorder states for its own.
//
// Record returns nothing. A caller cannot be told the history write failed,
// because the thing being recorded has already happened - the page was served,
// the socket subscribed, the lease moved - and it is in that state whatever the
// database does next. See Service.Record.
type Recorder interface {
	Record(ctx context.Context, t EventType)
}
