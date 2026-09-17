package project

import (
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kutonlagos/agentmux/internal/pathutil"
)

// Name length bounds. The upper bound keeps a name usable as a directory name
// on every supported platform, including inside a WSL path.
const (
	MinNameLength = 1
	MaxNameLength = 64
)

// invalidNameRunes are characters that cannot appear in a directory name on at
// least one supported platform. Rejecting them everywhere means a project
// created on Linux is still creatable on Windows.
const invalidNameRunes = `<>:"/\|?*`

// reservedNames are Windows device names. Windows reserves them with or
// without an extension, so "CON" and "CON.txt" are both unusable.
var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// ValidateName reports whether name can be used as a project directory name.
//
// The rules are deliberately strict, because this name is concatenated into a
// filesystem path. A name is rejected rather than sanitised: silently turning
// "my/project" into "my_project" would create a directory the user did not
// ask for.
func ValidateName(name string) error {
	if name == "" {
		return newError(CodeInvalidName, "project name must not be empty")
	}
	if name != strings.TrimSpace(name) {
		return newError(CodeInvalidName, "project name must not begin or end with whitespace")
	}
	switch name {
	case ".", "..":
		return newError(CodeInvalidName, "project name must not be %q", name)
	}
	if utf8.RuneCountInString(name) > MaxNameLength {
		return newError(CodeInvalidName, "project name must be at most %d characters", MaxNameLength)
	}
	// The character scan runs before the dot rules so that a traversal attempt
	// such as `..\..\Windows` is reported as an illegal character rather than
	// as a hidden file. Both reject it; only one says why it is dangerous.
	for _, r := range name {
		if r == utf8.RuneError {
			return newError(CodeInvalidName, "project name must be valid UTF-8")
		}
		if unicode.IsControl(r) {
			return newError(CodeInvalidName, "project name must not contain control characters")
		}
		if strings.ContainsRune(invalidNameRunes, r) {
			return newError(CodeInvalidName, "project name must not contain any of %s", invalidNameRunes)
		}
	}
	if strings.HasPrefix(name, ".") {
		return newError(CodeInvalidName, "project name must not begin with a dot")
	}
	if strings.HasSuffix(name, ".") {
		return newError(CodeInvalidName, "project name must not end with a dot")
	}
	// Windows reserves a device name regardless of any extension, so check
	// the part before the first dot.
	stem, _, _ := strings.Cut(name, ".")
	if reservedNames[strings.ToLower(stem)] {
		return newError(CodeInvalidName, "project name %q is a reserved device name", stem)
	}
	return nil
}

// SafeJoin returns parent/name after verifying that name is a single path
// component and that the result really is a direct child of parent.
//
// This is the only place a user-supplied name becomes a path. Validation
// happens first; the containment check afterwards is defence in depth, so that
// a later relaxation of ValidateName cannot silently introduce traversal.
func SafeJoin(parent, name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	if strings.TrimSpace(parent) == "" {
		return "", newError(CodeInvalidInput, "parent directory must not be empty")
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", newError(CodeInvalidName, "project name must not contain path separators or %q", "..")
	}
	cleanParent := filepath.Clean(parent)
	joined := filepath.Clean(filepath.Join(cleanParent, name))
	if !pathutil.Same(filepath.Dir(joined), cleanParent) {
		return "", newError(CodeInvalidName,
			"project name %q does not resolve to a directory directly inside %s", name, cleanParent)
	}
	return joined, nil
}
