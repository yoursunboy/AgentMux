package host

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestNativePathMapperValidatesRatherThanPassingThrough matters because a
// relative path accepted here would travel all the way to a terminal session
// before failing, in a place where the cause is no longer visible.
func TestNativePathMapperValidatesRatherThanPassingThrough(t *testing.T) {
	mapper := NewNativePathMapper()

	if got := mapper.Describe(); got != "native" {
		t.Errorf("Describe() = %q, want %q", got, "native")
	}

	absolute := t.TempDir()
	for _, in := range []string{absolute, absolute + string(filepath.Separator)} {
		got, err := mapper.ToRuntimePath(in)
		if err != nil {
			t.Fatalf("ToRuntimePath(%q) returned an error: %v", in, err)
		}
		if got != filepath.Clean(in) {
			t.Errorf("ToRuntimePath(%q) = %q, want %q", in, got, filepath.Clean(in))
		}
	}

	for _, in := range []string{"", "   ", "relative/path", "."} {
		got, err := mapper.ToRuntimePath(in)
		if err == nil {
			t.Errorf("ToRuntimePath(%q) = %q, want an error", in, got)
			continue
		}
		if !errors.Is(err, ErrEmptyPath) && !errors.Is(err, ErrNotAbsolute) {
			t.Errorf("ToRuntimePath(%q) error = %v, want ErrEmptyPath or ErrNotAbsolute", in, err)
		}
	}
}

// TestNativePathMapperIsSymmetric covers the case where the host and runtime are
// the same filesystem: translating in either direction is the same operation.
func TestNativePathMapperIsSymmetric(t *testing.T) {
	mapper := NewNativePathMapper()
	dir := t.TempDir()

	toRuntime, err := mapper.ToRuntimePath(dir)
	if err != nil {
		t.Fatalf("ToRuntimePath(%q): %v", dir, err)
	}
	toHost, err := mapper.ToHostPath(dir)
	if err != nil {
		t.Fatalf("ToHostPath(%q): %v", dir, err)
	}
	if toRuntime != toHost {
		t.Errorf("both directions must agree on a native host, got %q and %q", toRuntime, toHost)
	}
	if info, err := os.Stat(toRuntime); err != nil || !info.IsDir() {
		t.Errorf("the translated path %q must still refer to the directory", toRuntime)
	}
}
