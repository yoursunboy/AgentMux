package project

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateNameAccepts(t *testing.T) {
	names := []string{
		"AgentMux",
		"agentmux",
		"2026 AgentMux",
		"my-project",
		"my_project",
		"app.v2",
		"项目",
		"A",
		strings.Repeat("a", MaxNameLength),
	}
	for _, name := range names {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want it to be accepted", name, err)
		}
	}
}

func TestValidateNameRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"leading whitespace", " App"},
		{"trailing whitespace", "App "},
		{"only whitespace", "   "},
		{"a tab", "App\tName"},
		{"a newline", "App\nName"},
		{"single dot", "."},
		{"double dot", ".."},
		{"leading dot", ".hidden"},
		{"trailing dot", "App."},
		{"too long", strings.Repeat("a", MaxNameLength+1)},
		{"a forward slash", "my/project"},
		{"a backslash", `my\project`},
		{"traversal with a slash", "../escape"},
		{"traversal with a backslash", `..\escape`},
		{"a colon", "my:project"},
		{"an asterisk", "my*project"},
		{"a question mark", "my?project"},
		{"a double quote", `my"project`},
		{"an angle bracket", "my<project"},
		{"a pipe", "my|project"},
		{"a NUL byte", "my\x00project"},
		{"the reserved name CON", "CON"},
		{"the reserved name con in lower case", "con"},
		{"the reserved name CON with an extension", "CON.txt"},
		{"the reserved name LPT1", "LPT1"},
		{"the reserved name NUL with an extension", "nul.log"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateName(tt.in)
			if err == nil {
				t.Fatalf("ValidateName(%q) was accepted, want a rejection", tt.in)
			}
			if !IsCode(err, CodeInvalidName) {
				t.Errorf("ValidateName(%q) error code = %q, want %q", tt.in, CodeOf(err), CodeInvalidName)
			}
		})
	}
}

// TestValidateNameDoesNotSanitise records the deliberate choice: a name is
// rejected rather than repaired, because repairing it would create a directory
// the user did not ask for.
func TestValidateNameDoesNotSanitise(t *testing.T) {
	err := ValidateName("my/project")
	if err == nil {
		t.Fatal("a name containing a path separator must be rejected")
	}
	if strings.Contains(err.Error(), "my_project") {
		t.Error("the error must not suggest a repaired name; the caller decides what to do")
	}
}

// TestValidateNameIsAboutNamesNotPaths draws the line between the two rules.
// "my..project" is a legal display name, so ValidateName accepts it; SafeJoin
// is what refuses it, because a name containing ".." must never become a path.
func TestValidateNameIsAboutNamesNotPaths(t *testing.T) {
	if err := ValidateName("my..project"); err != nil {
		t.Errorf("ValidateName(\"my..project\") = %v, want it accepted as a display name", err)
	}
	parent := t.TempDir()
	if _, err := SafeJoin(parent, "my..project"); err == nil {
		t.Error("SafeJoin must refuse a name containing \"..\" even though it is a legal display name")
	}
}

func TestSafeJoin(t *testing.T) {
	parent := t.TempDir()

	got, err := SafeJoin(parent, "App")
	if err != nil {
		t.Fatalf("SafeJoin returned an error for a valid name: %v", err)
	}
	if want := filepath.Join(parent, "App"); got != want {
		t.Errorf("SafeJoin = %q, want %q", got, want)
	}
	if filepath.Dir(got) != filepath.Clean(parent) {
		t.Errorf("SafeJoin produced %q, which is not a direct child of %q", got, parent)
	}
}

// TestSafeJoinRefusesToEscape is the security test for the only place a
// user-supplied name becomes a path.
func TestSafeJoinRefusesToEscape(t *testing.T) {
	parent := t.TempDir()
	names := []string{
		"..",
		"../escape",
		`..\escape`,
		"../../etc/passwd",
		"a/../../escape",
		`a\..\..\escape`,
		"..\\..\\escape",
		"nested/child",
		`nested\child`,
		"/absolute",
		`C:\absolute`,
		"",
		"  ",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			got, err := SafeJoin(parent, name)
			if err == nil {
				t.Fatalf("SafeJoin(%q, %q) = %q, want a rejection", parent, name, got)
			}
			if got != "" {
				t.Errorf("SafeJoin returned %q alongside an error; it must return nothing", got)
			}
		})
	}
}

func TestSafeJoinRejectsAnEmptyParent(t *testing.T) {
	for _, parent := range []string{"", "   "} {
		if got, err := SafeJoin(parent, "App"); err == nil {
			t.Errorf("SafeJoin(%q, \"App\") = %q, want a rejection", parent, got)
		}
	}
}

// TestSafeJoinDoesNotTouchTheFilesystem proves the containment check is pure
// path arithmetic, so nothing is created by a rejected request.
func TestSafeJoinDoesNotTouchTheFilesystem(t *testing.T) {
	parent := t.TempDir()
	if _, err := SafeJoin(parent, "../escape"); err == nil {
		t.Fatal("expected a rejection")
	}

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("could not read the parent directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a rejected SafeJoin left %d entries behind", len(entries))
	}
}

// TestSafeJoinFollowsHostPathSemantics keeps the assertion honest on a
// case-insensitive filesystem, where a differently-cased name is the same
// location rather than a second project.
func TestSafeJoinFollowsHostPathSemantics(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("this host compares paths case-sensitively")
	}
	parent := t.TempDir()

	lower, err := SafeJoin(parent, "app")
	if err != nil {
		t.Fatalf("SafeJoin returned an error: %v", err)
	}
	upper, err := SafeJoin(parent, "APP")
	if err != nil {
		t.Fatalf("SafeJoin returned an error: %v", err)
	}
	if !strings.EqualFold(lower, upper) {
		t.Errorf("SafeJoin produced %q and %q, which differ only in case", lower, upper)
	}
}
