package claude

import "strings"

// PermissionMode is how much Claude asks before it acts.
//
// # Why a closed set and not a string
//
// The value becomes an argument on a command line that a shell parses. It is a
// named type with three values for the reason every vocabulary in this build is
// closed: a caller that asks for a mode this build does not offer should be
// told the field is unknown, rather than have its string carried as far as a
// shell and discovered there.
//
// These three are the CLI's own values, checked against 2.1.280 on the beta
// host. It accepts three more - `auto`, `dontAsk` and `plan` - and none of them
// is offered here. Offering one would be a product decision, and this build has
// made exactly three of them.
type PermissionMode string

const (
	// PermissionManual is Claude's ordinary behaviour: it asks before it acts.
	//
	// It is the CLI's own name for that, and it is what Claude does when nothing
	// says otherwise. It is offered rather than assumed: a person who wants to
	// be asked chooses it, and AgentMux's own default is not this one -
	// see internal/project.DefaultPermissionMode for that decision and why.
	PermissionManual PermissionMode = "manual"

	// PermissionAcceptEdits lets Claude change files without asking, and keeps
	// asking about everything else.
	//
	// It is the middle of the three, and it is here because "stop asking me
	// about edits in my own project" and "stop asking me anything" are
	// different requests that a two-value setting would have to answer with one
	// value.
	PermissionAcceptEdits PermissionMode = "acceptEdits"

	// PermissionBypass lets Claude act without asking at all.
	//
	// It is the mode a project nobody has configured launches under, which is
	// AgentMux's own decision rather than the CLI's - internal/project's
	// DefaultPermissionMode is where it is made and argued. It is a default and
	// not a policy: the other two are still offered, and a project that has
	// chosen one keeps it.
	PermissionBypass PermissionMode = "bypassPermissions"
)

// PermissionModes lists every mode, in the order a menu should offer them: the
// one that asks most, first.
//
// It is the one list. An error message enumerates it, a client is told it is
// the vocabulary, and a test asserts that what it returns is exactly what this
// file declares - so a fourth mode cannot be added to one and forgotten in the
// other.
func PermissionModes() []PermissionMode {
	return []PermissionMode{PermissionManual, PermissionAcceptEdits, PermissionBypass}
}

// PermissionModeNames renders the vocabulary as one comma-separated list, for a
// message that has to say what the field accepts.
func PermissionModeNames() string {
	names := make([]string, 0, len(PermissionModes()))
	for _, mode := range PermissionModes() {
		names = append(names, string(mode))
	}
	return strings.Join(names, ", ")
}

// ValidPermissionMode reports whether mode is one of the modes this build
// offers.
func ValidPermissionMode(mode PermissionMode) bool {
	switch mode {
	case PermissionManual, PermissionAcceptEdits, PermissionBypass:
		return true
	default:
		return false
	}
}
