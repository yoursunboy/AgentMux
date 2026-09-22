package attention

import (
	"testing"

	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/task"
)

// This file is in the package rather than beside it, because the thing it pins
// is a correspondence between two unexported tables: actionFor decides what a
// type an event raises, and attentionFor decides what level the same event
// asserts. LevelOfAction reads the type back as a level, and the whole reason it
// can is that the two agree exactly.
//
// A test in attention_test could only check the answers one at a time. This one
// walks the events and compares the two, which is what catches the drift that
// matters: somebody adding a fourth permission-like event, giving it a type, and
// forgetting that the queue would then read it as a notice.

// actionRaisingEvents is every event this build raises an action from, with the
// status an event of that kind carries.
//
// It is written out rather than derived, because deriving it would mean asking
// actionFor, and a test that asks the thing it is testing can only agree with
// it. This is the list a reader checks against projection.go.
var actionRaisingEvents = []struct {
	name          string
	eventType     string
	sessionStatus string
}{
	{"a permission request", claude.TypeAgentPermissionRequested, ""},
	{"a failed agent", claude.TypeAgentFailed, ""},
	{"a completed agent", claude.TypeAgentCompleted, ""},
	{"a failed attempt", task.TypeSessionStatusChanged, task.StatusSessionFailed},
	{"a completed attempt", task.TypeSessionStatusChanged, task.StatusSessionCompleted},
}

// TestLevelOfActionMatchesTheProjection is the pin.
//
// For every event that raises an action, the level the projection asserts at the
// moment it raises it and the level LevelOfAction reads back from the row must
// be the same. If they ever differ, the queue and the project card would be
// describing one action two ways, and the console would colour a card by one
// answer and list the same action under the other.
func TestLevelOfActionMatchesTheProjection(t *testing.T) {
	for _, c := range actionRaisingEvents {
		t.Run(c.name, func(t *testing.T) {
			actionType, _, raises := actionFor(c.eventType, c.sessionStatus)
			if !raises {
				t.Fatalf("actionFor(%q, %q) raises nothing; this case is in the wrong list",
					c.eventType, c.sessionStatus)
			}
			level, _, read := attentionFor(c.eventType, c.sessionStatus)
			if !read {
				t.Fatalf("attentionFor(%q, %q) reads nothing; the event raises an action it cannot level",
					c.eventType, c.sessionStatus)
			}

			if got := LevelOfAction(actionType); got != level {
				t.Errorf("LevelOfAction(%s) = %q; the projection asserts %q for the same event - "+
					"the queue and the card now disagree about one action",
					actionType, got, level)
			}
		})
	}
}

// TestEveryActionTypeHasALevel is the other direction.
//
// A type with no level would be a row the queue could not place: it would count
// as a notice by the fold's else branch, which is the right answer for a type
// from a newer build and the wrong one for a type this build defines. So every
// type this build knows must read as a level, and the only empty answer is for a
// type it does not.
func TestEveryActionTypeHasALevel(t *testing.T) {
	for _, typ := range ActionTypes() {
		if level := LevelOfAction(typ); level == "" {
			t.Errorf("LevelOfAction(%s) is empty; this build defines that type, so it must read as a level", typ)
		}
	}

	if level := LevelOfAction(ActionType("SOMETHING_FROM_A_NEWER_BUILD")); level != "" {
		t.Errorf("an unknown type reads as %q; want the empty level - this build cannot judge its urgency", level)
	}
}
