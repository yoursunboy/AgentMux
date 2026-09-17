package host

import (
	"errors"
	"testing"
)

// TestToRuntimePath covers the direction that matters for starting a session:
// a Windows path the user chose must become the path WSL sees.
func TestToRuntimePath(t *testing.T) {
	mapper := NewWSLPathMapper(DefaultWSLMountRoot)

	tests := []struct {
		name string
		host string
		want string
	}{
		{
			name: "the real AgentMux checkout, which contains a space",
			host: `D:\AI\Projects\2026 AgentMux\AgentMux`,
			want: "/mnt/d/AI/Projects/2026 AgentMux/AgentMux",
		},
		{
			name: "lower-case drive is normalised, path case is preserved",
			host: `d:\ai\projects`,
			want: "/mnt/d/ai/projects",
		},
		{
			name: "drive root",
			host: `C:\`,
			want: "/mnt/c",
		},
		{
			name: "forward slashes are accepted",
			host: "D:/AI/Projects/App",
			want: "/mnt/d/AI/Projects/App",
		},
		{
			name: "redundant separators and dot segments collapse",
			host: `D:\AI\\Projects\.\App`,
			want: "/mnt/d/AI/Projects/App",
		},
		{
			name: "parent segments resolve",
			host: `D:\AI\Projects\Old\..\App`,
			want: "/mnt/d/AI/Projects/App",
		},
		{
			name: "parent segments cannot escape the drive root",
			host: `D:\..\..\App`,
			want: "/mnt/d/App",
		},
		{
			name: "a non-D drive maps just as well",
			host: `E:\work`,
			want: "/mnt/e/work",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mapper.ToRuntimePath(tt.host)
			if err != nil {
				t.Fatalf("ToRuntimePath(%q) returned an error: %v", tt.host, err)
			}
			if got != tt.want {
				t.Errorf("ToRuntimePath(%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}

// TestToRuntimePathRejectsUnmappablePaths proves the mapper fails rather than
// inventing a path. A guessed path would send a session to the wrong directory.
func TestToRuntimePathRejectsUnmappablePaths(t *testing.T) {
	mapper := NewWSLPathMapper(DefaultWSLMountRoot)

	tests := []struct {
		name string
		host string
		want error
	}{
		{"empty", "", ErrEmptyPath},
		{"blank", "   ", ErrEmptyPath},
		{"UNC path", `\\server\share\project`, ErrUnsupportedPath},
		{"forward-slash UNC path", `//server/share/project`, ErrUnsupportedPath},
		{"device path", `\\?\D:\project`, ErrUnsupportedPath},
		{"drive-relative", `D:project`, ErrNotAbsolute},
		{"purely relative", `AI\Projects\App`, ErrNotAbsolute},
		{"POSIX path handed to a Windows mapper", "/mnt/d/AI/Projects", ErrNotAbsolute},
		{"digit instead of a drive letter", `1:\project`, ErrNotAbsolute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mapper.ToRuntimePath(tt.host)
			if err == nil {
				t.Fatalf("ToRuntimePath(%q) = %q, want an error", tt.host, got)
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("ToRuntimePath(%q) error = %v, want it to wrap %v", tt.host, err, tt.want)
			}
			if got != "" {
				t.Errorf("ToRuntimePath(%q) returned %q alongside an error; it must return nothing", tt.host, got)
			}
		})
	}
}

// TestToHostPathFailsClosed is the security-relevant direction. A path inside
// the WSL virtual machine has no Windows equivalent, and must be refused rather
// than reinterpreted as one.
func TestToHostPathFailsClosed(t *testing.T) {
	mapper := NewWSLPathMapper(DefaultWSLMountRoot)

	valid := []struct {
		name    string
		runtime string
		want    string
	}{
		{"project path", "/mnt/d/AI/Projects/App", `D:\AI\Projects\App`},
		{"path with a space", "/mnt/d/AI/Projects/2026 AgentMux/AgentMux", `D:\AI\Projects\2026 AgentMux\AgentMux`},
		{"uppercase drive is accepted", "/mnt/D/AI", `D:\AI`},
		{"drive root without a trailing slash", "/mnt/c", `C:\`},
		{"drive root with a trailing slash", "/mnt/c/", `C:\`},
		{"trailing separator is dropped", "/mnt/d/AI/", `D:\AI`},
	}
	for _, tt := range valid {
		t.Run("valid/"+tt.name, func(t *testing.T) {
			got, err := mapper.ToHostPath(tt.runtime)
			if err != nil {
				t.Fatalf("ToHostPath(%q) returned an error: %v", tt.runtime, err)
			}
			if got != tt.want {
				t.Errorf("ToHostPath(%q) = %q, want %q", tt.runtime, got, tt.want)
			}
		})
	}

	invalid := []struct {
		name    string
		runtime string
		want    error
	}{
		{"empty", "", ErrEmptyPath},
		{"blank", "  ", ErrEmptyPath},
		{"relative", "mnt/d/AI", ErrNotAbsolute},
		{"home directory inside the VM", "/home/user/project", ErrNotUnderMount},
		{"the mount root itself", "/mnt", ErrNotUnderMount},
		{"a sibling of the mount root", "/mnt2/d/AI", ErrNotUnderMount},
		{"WSL's own mount, which has no drive", "/mnt/wsl/docker-desktop", ErrNotUnderDrive},
		{"a named mount with no drive letter", "/mnt/backup/projects", ErrNotUnderDrive},
	}
	for _, tt := range invalid {
		t.Run("invalid/"+tt.name, func(t *testing.T) {
			got, err := mapper.ToHostPath(tt.runtime)
			if err == nil {
				t.Fatalf("ToHostPath(%q) = %q, want an error", tt.runtime, got)
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("ToHostPath(%q) error = %v, want it to wrap %v", tt.runtime, err, tt.want)
			}
		})
	}
}

func TestPathMapperRoundTrip(t *testing.T) {
	mapper := NewWSLPathMapper(DefaultWSLMountRoot)

	// Only canonical host paths round-trip exactly: the runtime form carries a
	// lower-case drive, so the original spelling is not recoverable.
	canonical := []string{
		`D:\AI\Projects\2026 AgentMux\AgentMux`,
		`C:\Users\someone\Documents\App`,
		`E:\work`,
		`D:\`,
	}
	for _, hostPath := range canonical {
		runtimePath, err := mapper.ToRuntimePath(hostPath)
		if err != nil {
			t.Fatalf("ToRuntimePath(%q): %v", hostPath, err)
		}
		back, err := mapper.ToHostPath(runtimePath)
		if err != nil {
			t.Fatalf("ToHostPath(%q): %v", runtimePath, err)
		}
		if back != hostPath {
			t.Errorf("round trip of %q produced %q (via %q)", hostPath, back, runtimePath)
		}
	}
}

func TestWSLPathMapperHonoursACustomMountRoot(t *testing.T) {
	mapper := NewWSLPathMapper("windows")
	if mapper.MountRoot() != "/windows" {
		t.Errorf("MountRoot() = %q, want %q", mapper.MountRoot(), "/windows")
	}
	if got, err := mapper.ToRuntimePath(`D:\AI`); err != nil || got != "/windows/d/AI" {
		t.Errorf("ToRuntimePath with a custom mount root = (%q, %v), want (%q, nil)", got, err, "/windows/d/AI")
	}
	if _, err := mapper.ToHostPath("/mnt/d/AI"); !errors.Is(err, ErrNotUnderMount) {
		t.Errorf("ToHostPath should reject a path outside the configured mount root, got %v", err)
	}
}

func TestNewWSLPathMapperNormalisesTheMountRoot(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", DefaultWSLMountRoot},
		{"   ", DefaultWSLMountRoot},
		{"/mnt", "/mnt"},
		{"/mnt/", "/mnt"},
		{"mnt", "/mnt"},
		{"/mnt//", "/mnt"},
		{"/mnt/./", "/mnt"},
	}
	for _, tt := range tests {
		if got := NewWSLPathMapper(tt.in).MountRoot(); got != tt.want {
			t.Errorf("NewWSLPathMapper(%q).MountRoot() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCleanWindowsPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"drive letter is upper-cased", `d:\ai`, `D:\ai`},
		{"root", `D:\`, `D:\`},
		{"bare drive becomes a root", `D:`, `D:\`},
		{"separators are normalised", "D:/AI/Projects", `D:\AI\Projects`},
		{"duplicate separators collapse", `D:\AI\\Projects`, `D:\AI\Projects`},
		{"trailing separator is dropped", `D:\AI\Projects\`, `D:\AI\Projects`},
		{"dot segments are dropped", `D:\.\AI\.\Projects`, `D:\AI\Projects`},
		{"parent segments pop", `D:\AI\Projects\Old\..\App`, `D:\AI\Projects\App`},
		{"parent cannot rise above the root", `D:\..\..\App`, `D:\App`},
		{"parent at the root is a no-op", `D:\..`, `D:\`},
		{"a name that merely starts with dots is kept", `D:\AI\..hidden`, `D:\AI\..hidden`},
		{"surrounding whitespace is trimmed", "  D:\\AI  ", `D:\AI`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CleanWindowsPath(tt.in)
			if err != nil {
				t.Fatalf("CleanWindowsPath(%q) returned an error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("CleanWindowsPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
