package host

import (
	"os"
	"runtime"
	"strings"
)

// Environment is where the AgentMux server process itself is running.
//
// This is a different question from RuntimeMode, which asks where terminal
// sessions execute relative to the server. On Windows the two genuinely
// differ: the server is a Windows process and the runtime is a WSL
// distribution. Inside WSL they coincide, and the distinction stops being
// interesting - which is exactly why the server belongs there.
//
// Reporting both is what lets the server tell a user "you started me on the
// wrong side of the boundary" instead of quietly failing to find tmux.
type Environment string

// Server environments.
const (
	// EnvWindows is a native Windows process.
	EnvWindows Environment = "windows"

	// EnvWSL is a Linux process inside a WSL2 distribution.
	EnvWSL Environment = "wsl"

	// EnvLinux is a native Linux process.
	EnvLinux Environment = "linux"

	// EnvDarwin is a native macOS process.
	EnvDarwin Environment = "darwin"
)

// Posix reports whether this environment can host a tmux runtime at all.
//
// tmux, the PTY, and the coding agent it will host are POSIX processes. This
// is a property of the environment, not a probe result: a missing tmux binary
// on Linux is an install problem, while a Windows process is a structural one.
func (e Environment) Posix() bool { return e != EnvWindows }

// wslKernelMarkers are the strings the WSL2 kernel reports about itself. Both
// /proc/sys/kernel/osrelease and /proc/version carry one of them inside WSL
// and neither carries one on ordinary Linux.
var wslKernelMarkers = []string{"microsoft", "wsl"}

// wslKernelFiles are the files consulted to recognise WSL, in order.
var wslKernelFiles = []string{"/proc/sys/kernel/osrelease", "/proc/version"}

// DetectEnvironment reports where this process is running, plus the WSL
// distribution name when it can be determined.
//
// The distribution is never guessed from the kernel: it comes from the
// WSL_DISTRO_NAME that WSL itself exports, or from the distribution's own
// os-release, or it stays empty.
func DetectEnvironment() (Environment, string) {
	switch runtime.GOOS {
	case "windows":
		return EnvWindows, ""
	case "darwin":
		return EnvDarwin, ""
	}
	if distro := strings.TrimSpace(os.Getenv("WSL_DISTRO_NAME")); distro != "" {
		return EnvWSL, distro
	}
	if isWSLKernel() {
		return EnvWSL, linuxDistroName()
	}
	return EnvLinux, ""
}

// isWSLKernel reports whether the running kernel identifies itself as WSL.
func isWSLKernel() bool {
	for _, path := range wslKernelFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lower := strings.ToLower(string(data))
		for _, marker := range wslKernelMarkers {
			if strings.Contains(lower, marker) {
				return true
			}
		}
	}
	return false
}

// linuxDistroName reads the distribution's own name from os-release.
//
// It is the fallback for a WSL environment that does not export
// WSL_DISTRO_NAME. An unreadable or nameless os-release yields "", because a
// distribution name AgentMux invented would be worse than none.
func linuxDistroName() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key != "NAME" {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

// DefaultShell returns the shell a session should run when none is configured.
//
// It is injected into the configuration as a default rather than read inside
// it, so that the configuration package stays free of platform knowledge.
func DefaultShell() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		return shell
	}
	// bash is present on essentially every system AgentMux targets, and
	// nothing beyond starting a process depends on which shell it is.
	return "/bin/bash"
}

// runtimeUnsupportedReason explains, in terms a user can act on, why a
// terminal runtime cannot execute here. It returns "" when one can.
//
// The message is deliberately specific about the fix rather than about the
// symptom: "tmux not found" on a Windows PATH is true and useless, because no
// amount of installing tmux on Windows would help.
func runtimeUnsupportedReason(env Environment, mode RuntimeMode) string {
	if env.Posix() {
		return ""
	}
	if mode == RuntimeWSL {
		return "The terminal runtime needs the AgentMux server to run inside WSL, because " +
			"tmux and the coding agent it hosts are Linux processes. Start the server from " +
			"inside your distribution instead, for example: " +
			"wsl -d <distribution> -- ./agentmux-server. " +
			"Project management and diagnostics work either way."
	}
	return "The terminal runtime needs a POSIX host. Start the AgentMux server inside WSL " +
		"(recommended on Windows) or on a Linux host. Project management and diagnostics " +
		"work either way."
}
