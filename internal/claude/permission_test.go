package claude

import (
	"strings"
	"testing"
)

// These tests are about the permission vocabulary and the command line it is
// rendered onto. They install nothing and run nothing: every value here is a
// string this build either offers or refuses.

// ---------------------------------------------------------------------------
// The vocabulary
// ---------------------------------------------------------------------------

// TestTheOfferedModesAreExactlyThree pins the closed set.
//
// It is written as an exact list rather than a count so that a fourth mode
// added by a later phase is a failure here - the point being that offering a
// mode is a product decision that shows up in a test, not a line somebody adds
// while passing.
func TestTheOfferedModesAreExactlyThree(t *testing.T) {
	got := PermissionModes()
	want := []PermissionMode{PermissionManual, PermissionAcceptEdits, PermissionBypass}

	if len(got) != len(want) {
		t.Fatalf("PermissionModes() = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("PermissionModes()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if PermissionManual != "manual" || PermissionAcceptEdits != "acceptEdits" || PermissionBypass != "bypassPermissions" {
		t.Errorf("the constants are %q, %q, %q; want the CLI's own spellings",
			PermissionManual, PermissionAcceptEdits, PermissionBypass)
	}
}

// TestValidPermissionModeAcceptsOnlyTheOfferedModes is §十一's check at the
// level where the vocabulary lives.
//
// The rejected list is not a list of dangerous strings. That is the point: the
// check is a membership test against a closed set, so the question of what a
// value would do in a shell never has to be asked. A mode that is not one of
// three is not a mode.
func TestValidPermissionModeAcceptsOnlyTheOfferedModes(t *testing.T) {
	for _, mode := range PermissionModes() {
		if !ValidPermissionMode(mode) {
			t.Errorf("ValidPermissionMode(%q) = false; want true", mode)
		}
	}

	for _, mode := range []PermissionMode{
		"",
		"MANUAL",
		"Manual",
		" manual",
		"manual ",
		"accept-edits",
		"accept_edits",
		"bypass",
		"bypasspermissions",
		// The CLI's other three, which this build deliberately does not offer.
		"auto",
		"dontAsk",
		"plan",
		// Nothing here is escaped or sanitised anywhere in this build. They are
		// refused for being outside the set, which is why they can be in this
		// list at all.
		"manual; rm -rf /",
		"manual && curl example.invalid",
		"$(whoami)",
		"`id`",
		"manual\n--dangerously-skip-permissions",
		"--dangerously-skip-permissions",
		"manual'--dangerously-skip-permissions",
	} {
		if ValidPermissionMode(mode) {
			t.Errorf("ValidPermissionMode(%q) = true; want false", mode)
		}
	}
}

// TestPermissionModeNamesListsTheVocabulary keeps the phrase a 400 puts in
// front of a person in step with the set it describes.
func TestPermissionModeNamesListsTheVocabulary(t *testing.T) {
	names := PermissionModeNames()
	for _, mode := range PermissionModes() {
		if !strings.Contains(names, string(mode)) {
			t.Errorf("PermissionModeNames() = %q, which does not name %q", names, mode)
		}
	}
}

// ---------------------------------------------------------------------------
// The command line
// ---------------------------------------------------------------------------

// testInstallation is an installation with a known command, so a rendered line
// can be compared exactly.
func testInstallation() Installation {
	return Installation{
		Type:      Type,
		Available: true,
		Version:   "2.1.280",
		Binary:    "claude",
		Path:      "/usr/local/bin/claude",
		Command:   "'/usr/local/bin/claude'",
	}
}

// TestTheLaunchCommandCarriesThePermissionMode is §十's fifth case, and it is
// the one that ties the setting to something that actually happens: a mode
// chosen in a menu becomes an argument on the line a shell runs.
func TestTheLaunchCommandCarriesThePermissionMode(t *testing.T) {
	for _, mode := range PermissionModes() {
		t.Run(string(mode), func(t *testing.T) {
			line := LaunchCommand(testInstallation(), LaunchOptions{
				SessionID:      "0f2a4c6e-1111-2222-3333-444455556666",
				SettingsPath:   "/home/dbroot/.claude/amx-prj_0123/settings.json",
				PermissionMode: mode,
			})

			want := "--permission-mode " + Quote(string(mode))
			if !strings.Contains(line, want) {
				t.Errorf("the launch command does not carry %s:\n%s", want, line)
			}
			// Once, and with the value attached to it. A flag that appeared
			// twice would let the second occurrence be the one a CLI used,
			// which is not a thing anything here should be able to do.
			if n := strings.Count(line, "--permission-mode"); n != 1 {
				t.Errorf("--permission-mode appears %d times; want once:\n%s", n, line)
			}
			// The mode is quoted, so it is one word to the shell whatever it
			// holds. Nothing in this build constructs a value with a space in
			// it - the set is closed - but the command line is not where that
			// is allowed to matter.
			if strings.Contains(line, "--permission-mode "+string(mode)) {
				t.Errorf("the mode is not quoted:\n%s", line)
			}
		})
	}
}

// TestALaunchWithNoModeIsTheLineItAlwaysWas is the compatibility half.
//
// A caller that passes no mode - which is every caller before this option
// existed, and the e2e fixture's own resolution test - must get exactly the
// command line this build produced before the option existed. Rendering an
// empty mode as `--permission-mode ”` would be a flag with a value the CLI has
// no meaning for, passed on behalf of somebody who chose nothing.
func TestALaunchWithNoModeIsTheLineItAlwaysWas(t *testing.T) {
	line := LaunchCommand(testInstallation(), LaunchOptions{
		SessionID:    "0f2a4c6e-1111-2222-3333-444455556666",
		SettingsPath: "/home/dbroot/.claude/amx-prj_0123/settings.json",
	})

	if strings.Contains(line, "--permission-mode") {
		t.Errorf("a launch with no mode passed the flag anyway:\n%s", line)
	}
	want := "'/usr/local/bin/claude' --session-id '0f2a4c6e-1111-2222-3333-444455556666' " +
		"--settings '/home/dbroot/.claude/amx-prj_0123/settings.json'"
	if line != want {
		t.Errorf("the launch command is\n%s\nwant\n%s", line, want)
	}

	// And with nothing at all, which is how the resolver's own test calls it.
	if got := LaunchCommand(testInstallation(), LaunchOptions{}); got != testInstallation().Command {
		t.Errorf("LaunchCommand with no options = %q, want the bare command %q",
			got, testInstallation().Command)
	}
}

// TestAModeCannotBreakOutOfItsArgument is §十一's claim, checked on the
// rendering rather than argued.
//
// The values here are not ones this build can store - internal/project refuses
// every one of them - so this is a test of the second line of defence rather
// than the first. It is here because "the enum is enforced upstream" is a
// statement about code that could change, and the quoting rule is a statement
// about the command line: whatever arrives, it arrives as one inert word.
//
// # What this does not claim
//
// It does not claim a line break could not reach the command line. Quote makes a
// value one *shell* word; a line break is legal inside a single-quoted word, and
// would be caught by nothing here - which matters because the line is not only
// parsed by a shell, it is typed into a terminal, where a newline is Enter. That
// hazard is closed where the vocabulary is: `manual\nrm -rf /` is refused by
// ValidPermissionMode, so no value containing a line break can be stored, and
// this function cannot be handed one by the product. The case below records that
// it is the vocabulary doing that work, not the quoting.
func TestAModeCannotBreakOutOfItsArgument(t *testing.T) {
	for _, hostile := range []PermissionMode{
		"manual; rm -rf /",
		"manual && curl example.invalid",
		"$(whoami)",
		"`id`",
		"manual'; rm -rf /; echo '",
		"manual --dangerously-skip-permissions",
	} {
		line := LaunchCommand(testInstallation(), LaunchOptions{PermissionMode: hostile})

		// Every argument on the line is a single-quoted word, so the hostile
		// value is present, verbatim, inside one - and the shell would see one
		// argument where a reader sees a payload. The tell for an escape would
		// be a quote or a separator outside a quoted word, which is exactly
		// what the two assertions below would fail on.
		if !strings.Contains(line, Quote(string(hostile))) {
			t.Errorf("%q is not rendered as one quoted word:\n%s", hostile, line)
		}
		if strings.Count(line, "'")%2 != 0 {
			t.Errorf("%q left an unbalanced quote on the line:\n%s", hostile, line)
		}
		// The value ends the line. If it had ended its word early, whatever
		// followed the payload would be a second argument on the command line.
		if !strings.HasSuffix(line, Quote(string(hostile))) {
			t.Errorf("%q is not the last thing on the line:\n%s", hostile, line)
		}
	}

	// And the case the vocabulary catches rather than the quoting.
	if ValidPermissionMode("manual\n--dangerously-skip-permissions") {
		t.Error("a mode containing a line break is accepted; the vocabulary is what keeps one off the command line")
	}
}
