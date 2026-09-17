package host

import (
	"fmt"
	"path"
	"strings"
)

// DefaultWSLMountRoot is where WSL2 exposes Windows drives by default.
const DefaultWSLMountRoot = "/mnt"

// WSLPathMapper translates between Windows drive paths and the paths WSL2
// exposes for the same files.
//
//	D:\AI\Projects\2026 AgentMux\AgentMux
//	/mnt/d/AI/Projects/2026 AgentMux/AgentMux
//
// The mapping is general over drive letters: C: maps to /mnt/c, not just D:.
// Nothing here assumes a particular drive, and nothing here shells out.
type WSLPathMapper struct {
	mountRoot string
}

// NewWSLPathMapper returns a mapper using mountRoot (default /mnt). An empty
// value selects DefaultWSLMountRoot.
func NewWSLPathMapper(mountRoot string) *WSLPathMapper {
	root := strings.TrimSpace(mountRoot)
	if root == "" {
		root = DefaultWSLMountRoot
	}
	if !strings.HasPrefix(root, "/") {
		root = "/" + root
	}
	root = strings.TrimRight(path.Clean(root), "/")
	if root == "" {
		root = DefaultWSLMountRoot
	}
	return &WSLPathMapper{mountRoot: root}
}

// MountRoot is the mount point this mapper uses.
func (m *WSLPathMapper) MountRoot() string { return m.mountRoot }

// Describe implements PathMapper.
func (m *WSLPathMapper) Describe() string { return "wsl:" + m.mountRoot }

// ToRuntimePath converts a Windows host path to its WSL equivalent.
func (m *WSLPathMapper) ToRuntimePath(hostPath string) (string, error) {
	cleaned, err := CleanWindowsPath(hostPath)
	if err != nil {
		return "", err
	}
	drive := strings.ToLower(cleaned[0:1])
	// cleaned is always "X:\" or "X:\rest" at this point.
	rest := strings.ReplaceAll(cleaned[3:], `\`, "/")
	base := m.mountRoot + "/" + drive
	if rest == "" {
		return base, nil
	}
	return base + "/" + rest, nil
}

// ToHostPath converts a WSL runtime path back to a Windows host path.
//
// This direction fails closed. A path that is not below a drive-letter mount
// has no Windows equivalent at all - it lives inside the WSL virtual machine -
// so it is rejected rather than guessed at.
func (m *WSLPathMapper) ToHostPath(runtimePath string) (string, error) {
	if strings.TrimSpace(runtimePath) == "" {
		return "", ErrEmptyPath
	}
	cleaned := path.Clean(strings.TrimSpace(runtimePath))
	if !strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("%w: %q", ErrNotAbsolute, runtimePath)
	}
	prefix := m.mountRoot + "/"
	if !strings.HasPrefix(cleaned, prefix) {
		return "", fmt.Errorf("%w: %q is not below %q, so it has no host path",
			ErrNotUnderMount, runtimePath, m.mountRoot)
	}
	remainder := cleaned[len(prefix):]
	segment, rest, _ := strings.Cut(remainder, "/")
	if len(segment) != 1 || !isASCIILetter(segment[0]) {
		return "", fmt.Errorf("%w: %q is not below a drive-letter mount", ErrNotUnderDrive, runtimePath)
	}
	drive := strings.ToUpper(segment)
	if rest == "" {
		return drive + `:\`, nil
	}
	return drive + `:\` + strings.ReplaceAll(rest, "/", `\`), nil
}

// CleanWindowsPath returns the canonical absolute Windows form of p: an
// upper-case drive letter, backslash separators, and "." / ".." resolved
// using Windows semantics, where the parent of a drive root is the drive root
// itself and cannot escape it.
//
// It rejects UNC and device paths, drive-relative paths such as "D:foo", and
// anything that is not drive-qualified.
func CleanWindowsPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", ErrEmptyPath
	}
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return "", fmt.Errorf("%w: UNC and device paths cannot be mapped to WSL: %q", ErrUnsupportedPath, p)
	}
	if len(p) < 2 || p[1] != ':' {
		return "", fmt.Errorf("%w: %q is not a drive-qualified absolute path", ErrNotAbsolute, p)
	}
	drive := p[0]
	if !isASCIILetter(drive) {
		return "", fmt.Errorf("%w: %q does not begin with a drive letter", ErrNotAbsolute, p)
	}
	if len(p) == 2 {
		return strings.ToUpper(string(drive)) + `:\`, nil
	}
	if p[2] != '\\' && p[2] != '/' {
		return "", fmt.Errorf("%w: %q is drive-relative, not absolute", ErrNotAbsolute, p)
	}
	segments := strings.FieldsFunc(p[3:], func(r rune) bool { return r == '\\' || r == '/' })
	stack := make([]string, 0, len(segments))
	for _, segment := range segments {
		switch segment {
		case "", ".":
			continue
		case "..":
			// Windows cannot go above a drive root, so ".." at the root is a
			// no-op rather than an escape.
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		default:
			stack = append(stack, segment)
		}
	}
	out := strings.ToUpper(string(drive)) + `:\`
	if len(stack) == 0 {
		return out, nil
	}
	return out + strings.Join(stack, `\`), nil
}

func isASCIILetter(r byte) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}
