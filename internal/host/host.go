// Package host centralises every OS-specific decision in AgentMux.
//
// Two things live here and nowhere else:
//
//   - how a host filesystem path is spelled for the runtime that executes
//     tmux and the AI CLI (the PathMapper);
//   - which environment AgentMux is actually running on, and what that
//     environment is missing.
//
// Business logic never contains a drive letter, a mount point, or an OS check.
// If a project path needs translating, it goes through host.Adapter.
package host

import (
	"context"
	"errors"
)

// Errors reported by the host layer.
var (
	// ErrNotImplemented marks a capability that is declared by the interface
	// but deliberately not built yet.
	ErrNotImplemented = errors.New("not implemented in this phase")

	// ErrOutsideProjectsRoot means a path is not inside any configured
	// Projects Root, so AgentMux refuses to manage it.
	ErrOutsideProjectsRoot = errors.New("path is outside the configured projects roots")

	// ErrEmptyPath means an empty path was supplied.
	ErrEmptyPath = errors.New("path is empty")

	// ErrNotAbsolute means a path was not absolute.
	ErrNotAbsolute = errors.New("path must be absolute")

	// ErrUnsupportedPath means the path form cannot be translated, for
	// example a UNC path that has no WSL mount equivalent.
	ErrUnsupportedPath = errors.New("path form is not supported")

	// ErrNotUnderMount means a runtime path is not below the configured WSL
	// mount root, so it has no host path at all.
	ErrNotUnderMount = errors.New("path is not below the WSL mount root")

	// ErrNotUnderDrive means a runtime path is below the mount root but not
	// below a drive-letter mount.
	ErrNotUnderDrive = errors.New("path is not below a drive-letter mount")
)

// Kind is the host operating system AgentMux runs on.
type Kind string

// Host operating systems.
const (
	KindWindows Kind = "windows"
	KindLinux   Kind = "linux"
	KindDarwin  Kind = "darwin"
)

// RuntimeMode is where terminal sessions execute.
type RuntimeMode string

// Runtime modes.
const (
	// RuntimeNative executes on the same OS that runs the AgentMux server.
	RuntimeNative RuntimeMode = "native"

	// RuntimeWSL executes inside a WSL2 distribution while the server runs on
	// Windows. Host paths are Windows paths; runtime paths are WSL paths.
	RuntimeWSL RuntimeMode = "wsl"
)

// SystemInfo describes the environment AgentMux is running in. It is reported
// by GET /api/server and contains no secrets.
type SystemInfo struct {
	HostOS      Kind        `json:"hostOs"`
	HostArch    string      `json:"hostArch"`
	RuntimeMode RuntimeMode `json:"runtimeMode"`
	RuntimeOS   Kind        `json:"runtimeOs"`
	Distro      string      `json:"distro,omitempty"`
	PathMapper  string      `json:"pathMapper"`
}

// Dependency is the result of probing for one external program.
type Dependency struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Path      string `json:"path,omitempty"`

	// Required records whether AgentMux needs this program for the current
	// phase. Everything is optional in Phase 1; tmux becomes required in
	// Phase 2 and the AI CLI in Phase 3.
	Required bool `json:"required"`

	// Note explains what the program is used for.
	Note string `json:"note,omitempty"`
}

// Adapter is the single entry point for OS-specific behaviour.
//
// The interface is intentionally wider than Phase 1 needs: it fixes the shape
// the later phases depend on, so that adding tmux, Open VS Code, and WSL
// dependency probing does not require changing call sites.
type Adapter interface {
	// Kind is the host operating system.
	Kind() Kind

	// Mode is where terminal sessions will execute.
	Mode() RuntimeMode

	// Info describes the environment. Detection failures degrade to empty
	// fields rather than failing the call.
	Info(ctx context.Context) SystemInfo

	// PathMapper returns the mapper used for host <-> runtime translation.
	PathMapper() PathMapper

	// ToRuntimePath translates a host path into the path the runtime sees.
	ToRuntimePath(hostPath string) (string, error)

	// ToHostPath translates a runtime path back into a host path.
	ToHostPath(runtimePath string) (string, error)

	// ProjectsRoots returns the configured broad roots, in priority order.
	ProjectsRoots() []string

	// OwningRoot returns the configured root that contains hostPath, or "".
	// When roots are nested the most specific one wins.
	OwningRoot(hostPath string) string

	// ContainsPath reports whether hostPath lies inside a configured root.
	ContainsPath(hostPath string) bool

	// CollectionPathFor returns the collection/group folder that directly
	// contains hostPath, or "" when hostPath is a direct child of its root.
	// It returns ErrOutsideProjectsRoot for paths outside every root.
	CollectionPathFor(hostPath string) (string, error)

	// CheckDependencies probes for the external programs AgentMux uses.
	CheckDependencies(ctx context.Context) []Dependency

	// OpenInVSCode opens a registered project in VS Code. Declared for
	// Phase 9; returns ErrNotImplemented today.
	OpenInVSCode(ctx context.Context, hostPath string) error
}
