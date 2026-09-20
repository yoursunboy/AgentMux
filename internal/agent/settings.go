package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// This file puts Claude's hook configuration where Claude will read it.
//
// # Why the coordinator writes it and the adapter does not
//
// The adapter renders the document - it is the only thing that knows what
// Claude's settings schema looks like - and deliberately does not write it.
// `Adapter.HookSettings` returns bytes and takes no view on where they go,
// because a component that chose a path would be editing a machine's
// environment as a side effect of starting, and a test of the adapter would be
// writing to a real disk.
//
// # Where it goes, and why not somewhere else
//
// Under the server's own data directory, beside the database and the tmux
// sockets. It is deliberately not inside the project: a project is somebody's
// repository, and CLAUDE.md rule 11 - runtime metadata must not be stored inside
// user repositories - is the rule this is obeying. A settings file that only
// AgentMux can interpret has no business appearing in a `git status`.
//
// One file per runtime rather than one for the machine: a runtime is what an
// adapter observes, the document names that adapter's port, and a single shared
// file would mean every runtime's hooks pointed at whichever adapter started
// last.

// settingsDirName is the directory under the data directory that holds them.
const settingsDirName = "claude"

// settingsFileName is the file Claude is pointed at with `--settings`.
const settingsFileName = "settings.json"

// SettingsWriter puts a hook configuration document somewhere Claude can read
// it, and takes it away again.
//
// It is an interface so that the lifecycle can be tested without a disk, and so
// that a caller that wanted the documents somewhere else - a tmpfs, a
// container's own volume - could have it without changing the chain that
// produces them.
type SettingsWriter interface {
	// Write stores the document for a runtime and returns the path Claude
	// should be told. The path is absolute: it ends up on a command line that is
	// typed into a shell in another working directory.
	Write(runtimeID string, document []byte) (string, error)

	// Remove deletes a runtime's document. Removing one that is not there is
	// not a failure: the caller is usually reacting to something ending, and
	// needing to know whether a file happened to exist would make every caller
	// responsible for a race it cannot see.
	Remove(runtimeID string) error
}

// FileSettings stores the documents on disk under a data directory.
type FileSettings struct {
	// Root is the data directory. The documents go in a subdirectory of it.
	Root string
}

// NewFileSettings builds a FileSettings under a data directory.
func NewFileSettings(dataDir string) *FileSettings {
	return &FileSettings{Root: dataDir}
}

// Dir is the directory the documents live in.
func (f *FileSettings) Dir() string {
	return filepath.Join(f.Root, settingsDirName)
}

// Write implements SettingsWriter.
//
// The write is atomic for the same reason the store's is: the file is handed to
// a process on a command line, and a reader that caught it half-written would
// see a settings document that is not JSON. It is written to a sibling
// temporary file and renamed over the target.
//
// The permissions are narrowed to the owner. The document is not a credential -
// it holds a loopback URL and a hook list - but it is a file that tells a
// program where to send what it observes, and a writable one would let anything
// on the machine redirect that.
func (f *FileSettings) Write(runtimeID string, document []byte) (string, error) {
	dir, err := f.dirFor(runtimeID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", wrapError(err, CodeSettingsFailure,
			"could not create the claude settings directory for runtime %s", runtimeID).
			withDetail("runtimeId", runtimeID)
	}

	path := filepath.Join(dir, settingsFileName)
	tmp, err := os.CreateTemp(dir, settingsFileName+".*")
	if err != nil {
		return "", wrapError(err, CodeSettingsFailure,
			"could not write the claude settings document for runtime %s", runtimeID).
			withDetail("runtimeId", runtimeID)
	}
	tmpName := tmp.Name()

	// Every exit from here removes the temporary file. A partial document left
	// behind is a file nothing reads and nobody cleans up.
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(document); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", wrapError(err, CodeSettingsFailure,
			"could not write the claude settings document for runtime %s", runtimeID).
			withDetail("runtimeId", runtimeID)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", wrapError(err, CodeSettingsFailure,
			"could not restrict the claude settings document for runtime %s", runtimeID).
			withDetail("runtimeId", runtimeID)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", wrapError(err, CodeSettingsFailure,
			"could not finish writing the claude settings document for runtime %s", runtimeID).
			withDetail("runtimeId", runtimeID)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return "", wrapError(err, CodeSettingsFailure,
			"could not put the claude settings document in place for runtime %s", runtimeID).
			withDetail("runtimeId", runtimeID)
	}
	return path, nil
}

// Remove implements SettingsWriter.
func (f *FileSettings) Remove(runtimeID string) error {
	dir, err := f.dirFor(runtimeID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return wrapError(err, CodeSettingsFailure,
			"could not remove the claude settings document for runtime %s", runtimeID).
			withDetail("runtimeId", runtimeID)
	}
	return nil
}

// dirFor is the directory a runtime's document lives in.
//
// The runtime id is checked rather than trusted. It is "amx-" followed by a
// project id, so it is not a value a caller can point anywhere - but it is a
// value that becomes a path component here, and the check is what keeps that
// true if the naming rule ever changes. A separator or a parent reference would
// turn a settings directory into a write anywhere on the filesystem.
func (f *FileSettings) dirFor(runtimeID string) (string, error) {
	id := strings.TrimSpace(runtimeID)
	if id == "" || id == "." || id == ".." {
		return "", newError(CodeInvalidInput,
			"a claude settings document must be named for a runtime, not %q", runtimeID)
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return "", newError(CodeInvalidInput,
			"the runtime id %q cannot be used as a path component", runtimeID).
			withDetail("runtimeId", runtimeID)
	}
	return filepath.Join(f.Dir(), id), nil
}
