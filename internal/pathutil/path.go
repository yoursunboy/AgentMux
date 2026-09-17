// Package pathutil holds the platform path semantics shared by configuration
// resolution and host adaptation.
//
// It exists so that "is this path inside that root" and "are these two paths
// the same location" are answered in exactly one place. Both questions depend
// on whether the host filesystem is case-insensitive, and getting that wrong
// in a containment check is a security problem, not a cosmetic one.
package pathutil

import (
	"path/filepath"
	"runtime"
	"strings"
)

// IsCaseInsensitive reports whether the host filesystem treats paths
// case-insensitively. Windows and macOS do; Linux does not.
func IsCaseInsensitive() bool {
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

// Key normalises a path for comparison, de-duplication, and map lookups.
func Key(p string) string {
	k := filepath.Clean(p)
	if IsCaseInsensitive() {
		return strings.ToLower(k)
	}
	return k
}

// Same reports whether two paths refer to the same location.
func Same(a, b string) bool {
	return Key(a) == Key(b)
}

// Contains reports whether child is parent itself or lies below parent.
//
// The comparison is component-aware: "/data/projects2" is not contained in
// "/data/projects". A trailing separator on parent is tolerated.
func Contains(parent, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if Same(parent, child) {
		return true
	}
	prefix := parent
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return HasPrefixFold(child, prefix)
}

// Relative returns child's path relative to parent when child lies below
// parent. The second result is false when it does not.
func Relative(parent, child string) (string, bool) {
	if !Contains(parent, child) {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return rel, true
}

// HasPrefixFold reports whether s begins with prefix, honouring the host's
// case sensitivity.
func HasPrefixFold(s, prefix string) bool {
	if IsCaseInsensitive() {
		return strings.HasPrefix(strings.ToLower(s), strings.ToLower(prefix))
	}
	return strings.HasPrefix(s, prefix)
}

// EqualFold reports whether two strings are equal under the host's case
// sensitivity rules.
func EqualFold(a, b string) bool {
	if IsCaseInsensitive() {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// IsRoot reports whether p is a filesystem root such as "/" or "D:\".
func IsRoot(p string) bool {
	cleaned := filepath.Clean(p)
	if cleaned == string(filepath.Separator) {
		return true
	}
	vol := filepath.VolumeName(cleaned)
	return vol != "" && cleaned == vol+string(filepath.Separator)
}
