package pathutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestContainsIsComponentAware is the important one: a prefix comparison would
// report that "/data/projects2" sits inside "/data/projects", which would let a
// project outside the configured root be treated as inside it.
func TestContainsIsComponentAware(t *testing.T) {
	tests := []struct {
		name   string
		parent string
		child  string
		want   bool
	}{
		{"child below parent", "/data/projects", "/data/projects/app", true},
		{"deeply nested child", "/data/projects", "/data/projects/a/b/c", true},
		{"parent itself", "/data/projects", "/data/projects", true},
		{"sibling sharing a prefix", "/data/projects", "/data/projects2", false},
		{"sibling file sharing a prefix", "/data/projects", "/data/projects-old", false},
		{"parent of parent", "/data/projects", "/data", false},
		{"unrelated", "/data/projects", "/other/app", false},
		{"trailing separator on parent", "/data/projects/", "/data/projects/app", true},
		{"traversal escapes parent", "/data/projects", "/data/projects/../other", false},
		{"traversal that stays inside", "/data/projects", "/data/projects/a/../b", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Contains(tt.parent, tt.child); got != tt.want {
				t.Errorf("Contains(%q, %q) = %v, want %v", tt.parent, tt.child, got, tt.want)
			}
		})
	}
}

func TestRelative(t *testing.T) {
	rel, ok := Relative("/data/projects", "/data/projects/Collection/App")
	if !ok {
		t.Fatal("Relative reported the child is not below the parent")
	}
	want := filepath.Join("Collection", "App")
	if rel != want {
		t.Errorf("Relative = %q, want %q", rel, want)
	}

	if _, ok := Relative("/data/projects", "/data/other"); ok {
		t.Error("Relative reported an unrelated path as relative")
	}

	// A path relative to itself is the empty string, not ".".
	rel, ok = Relative("/data/projects", "/data/projects")
	if !ok || rel != "" {
		t.Errorf("Relative(path, path) = (%q, %v), want (\"\", true)", rel, ok)
	}
}

func TestSame(t *testing.T) {
	if !Same("/data/projects", "/data/projects/") {
		t.Error("Same should tolerate a trailing separator")
	}
	if Same("/data/projects", "/data/projects2") {
		t.Error("Same must not equate paths that differ")
	}
}

// TestKeyFollowsHostCaseSensitivity pins the behaviour the containment checks
// depend on, rather than asserting a fixed answer that would be wrong on one of
// the supported hosts.
func TestKeyFollowsHostCaseSensitivity(t *testing.T) {
	upper := Key("/Data/Projects")
	lower := Key("/data/projects")
	if IsCaseInsensitive() {
		if upper != lower {
			t.Errorf("on a case-insensitive host Key(%q) = %q and Key(%q) = %q; they must match",
				"/Data/Projects", upper, "/data/projects", lower)
		}
		return
	}
	if upper == lower {
		t.Errorf("on a case-sensitive host Key(%q) and Key(%q) must differ", "/Data/Projects", "/data/projects")
	}
}

func TestIsCaseInsensitiveMatchesHost(t *testing.T) {
	want := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	if got := IsCaseInsensitive(); got != want {
		t.Errorf("IsCaseInsensitive() = %v on %s, want %v", got, runtime.GOOS, want)
	}
}

func TestIsRoot(t *testing.T) {
	if !IsRoot(string(filepath.Separator)) {
		t.Errorf("IsRoot(%q) = false, want true", string(filepath.Separator))
	}
	if IsRoot(filepath.Join("data", "projects")) {
		t.Error("a relative path is not a filesystem root")
	}
	if IsRoot("") {
		t.Error("an empty path is not a filesystem root")
	}

	// A drive or volume root only exists on some hosts, so it is derived from
	// the working directory instead of being hard-coded.
	wd, err := os.Getwd()
	if err != nil {
		t.Skipf("cannot determine the working directory: %v", err)
	}
	volume := filepath.VolumeName(wd)
	if volume == "" {
		t.Skip("this host has no volume names to test")
	}
	if root := volume + string(filepath.Separator); !IsRoot(root) {
		t.Errorf("IsRoot(%q) = false, want true", root)
	}
	if IsRoot(filepath.Join(volume+string(filepath.Separator), "data")) {
		t.Error("a directory inside a volume root is not itself a root")
	}
}

func TestHasPrefixFoldAndEqualFold(t *testing.T) {
	// Both follow the host's case sensitivity, so the assertions are phrased
	// against it rather than against a fixed answer that would be wrong on one
	// of the supported hosts: a case-differing prefix matches exactly when the
	// host is case-insensitive.
	caseInsensitive := IsCaseInsensitive()

	if got := HasPrefixFold("/DATA/projects", "/data/"); got != caseInsensitive {
		t.Errorf("HasPrefixFold = %v, want %v (case-insensitive host: %v)",
			got, caseInsensitive, caseInsensitive)
	}
	if got := HasPrefixFold("/data/projects", "/data/"); !got {
		t.Error("HasPrefixFold must match an identical prefix on every host")
	}
	if got := EqualFold("A", "a"); got != caseInsensitive {
		t.Errorf("EqualFold = %v, want %v (case-insensitive host: %v)",
			got, caseInsensitive, caseInsensitive)
	}
	if !EqualFold("abc", "abc") {
		t.Error("EqualFold must match identical strings on every host")
	}
}
