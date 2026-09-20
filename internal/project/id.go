package project

import (
	"github.com/kutonlagos/agentmux/internal/idgen"
)

// IDPrefix marks a project identifier. It makes an ID recognisable in a log
// line or a tmux session list.
const IDPrefix = "p_"

// projectID is the shape of a project identifier.
//
// Ten bytes is 80 bits, which is far beyond what a single-user installation can
// collide on, while keeping the identifier short enough to read aloud. It is
// shorter than an event id because a project id is spoken about - it is part of
// the session name - and an event id is not.
var projectID = idgen.Spec{Prefix: IDPrefix, Bytes: 10}

// NewID returns a fresh, stable project identifier.
//
// The identifier is random rather than derived from the project name, because
// the runtime session name is built from it: a name-derived ID would silently
// move a project's session whenever the project was renamed.
func NewID() (string, error) { return projectID.New() }

// ValidID reports whether id has the shape NewID produces.
func ValidID(id string) bool { return projectID.Valid(id) }
