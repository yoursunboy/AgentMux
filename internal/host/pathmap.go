package host

import (
	"fmt"
	"path/filepath"
	"strings"
)

// PathMapper converts between the path a user sees on the host and the path
// the runtime sees.
//
// This is the only abstraction allowed to know that a Windows drive appears
// under a mount point inside WSL. Every other package stores both forms and
// asks the mapper to produce them.
type PathMapper interface {
	// ToRuntimePath translates a host path into a runtime path.
	ToRuntimePath(hostPath string) (string, error)

	// ToHostPath translates a runtime path back into a host path.
	ToHostPath(runtimePath string) (string, error)

	// Describe names the mapper for diagnostics, for example "native" or
	// "wsl:/mnt".
	Describe() string
}

// NativePathMapper is used when the host and the runtime are the same
// filesystem: Linux running AgentMux directly, or Windows running without WSL.
//
// Translation is still validated rather than passed through, so that a
// relative path is rejected here instead of surfacing much later as a
// confusing tmux failure.
type NativePathMapper struct{}

// NewNativePathMapper returns a mapper for a host that equals its runtime.
func NewNativePathMapper() NativePathMapper { return NativePathMapper{} }

// ToRuntimePath implements PathMapper.
func (NativePathMapper) ToRuntimePath(hostPath string) (string, error) {
	return cleanHostAbsolute(hostPath)
}

// ToHostPath implements PathMapper.
func (NativePathMapper) ToHostPath(runtimePath string) (string, error) {
	return cleanHostAbsolute(runtimePath)
}

// Describe implements PathMapper.
func (NativePathMapper) Describe() string { return "native" }

// cleanHostAbsolute validates and canonicalises a host-native absolute path.
func cleanHostAbsolute(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", ErrEmptyPath
	}
	p = strings.TrimSpace(p)
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%w: %q", ErrNotAbsolute, p)
	}
	return filepath.Clean(p), nil
}
