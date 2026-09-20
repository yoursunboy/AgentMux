package event

import (
	"strings"
	"testing"
)

func TestNewIDShape(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		if !ValidID(id) {
			t.Fatalf("NewID produced %q, which ValidID refuses", id)
		}
		if !strings.HasPrefix(id, IDPrefix) {
			t.Fatalf("NewID produced %q, want the %q prefix", id, IDPrefix)
		}
		if len(id) != len(IDPrefix)+eventID.BodyLen() {
			t.Fatalf("NewID produced %q, want %d characters", id, len(IDPrefix)+eventID.BodyLen())
		}
		// Uniqueness before storage is the property the identifier exists for:
		// it is what lets an event be logged or handed to a client before the
		// database has seen it.
		if seen[id] {
			t.Fatalf("NewID repeated %q within a thousand draws", id)
		}
		seen[id] = true
	}
}

func TestValidID(t *testing.T) {
	valid := []string{
		"evt_0123456789abcdef0123456789abcdef",
		"evt_ffffffffffffffffffffffffffffffff",
		"evt_00000000000000000000000000000000",
	}
	for _, id := range valid {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false, want true", id)
		}
	}

	invalid := []string{
		"",
		"evt_",
		"evt_0123456789abcdef0123456789abcde",   // one character short
		"evt_0123456789abcdef0123456789abcdef0", // one character long
		"0123456789abcdef0123456789abcdef",      // no prefix
		"p_0123456789abcdef0123456789abcdef",    // another resource's prefix
		"evt_0123456789ABCDEF0123456789ABCDEF",  // uppercase is not the format
		"evt_0123456789abcdef0123456789abcdeg",  // not hex
		" evt_0123456789abcdef0123456789abcdef", // leading space
		"evt_0123456789abcdef0123456789abcdef ", // trailing space
	}
	for _, id := range invalid {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true, want false", id)
		}
	}
}

// TestValidTypeIsAShapeAndNotAVocabulary pins the design decision: a later
// phase can add a type without changing this package, and what is caught is the
// mistake somebody would actually make - passing a status, or a sentence.
func TestValidType(t *testing.T) {
	valid := []string{
		"runtime.started",
		"runtime.stopped",
		"runtime.destroyed",
		"agent.message",
		"a.b",
		"task.created",
		"runtime.state_changed",
		"git.commit",
		"v2.thing",
	}
	for _, name := range valid {
		if !ValidType(name) {
			t.Errorf("ValidType(%q) = false, want true", name)
		}
	}

	// This is the case §三 is about: a status is not an event.
	invalid := []string{
		"waiting",
		"running",
		"runtime_status = waiting",
		"runtime",
		"runtime.",
		".started",
		"runtime..started",
		"Runtime.Started",
		"runtime started",
		"runtime-started",
		"runtime.1started",
		"_runtime.started",
		"runtime.started ",
		"",
		strings.Repeat("a", 65) + ".b",
	}
	for _, name := range invalid {
		if ValidType(name) {
			t.Errorf("ValidType(%q) = true, want false", name)
		}
	}
}

// TestValidSourceIsAVocabulary pins the other half: sources are closed, because
// a misspelled source is an event no filter ever matches.
func TestValidSource(t *testing.T) {
	for _, source := range Sources() {
		if !ValidSource(source) {
			t.Errorf("Sources() lists %q but ValidSource refuses it", source)
		}
	}
	if len(Sources()) != 4 {
		t.Errorf("Sources() has %d entries, want 4", len(Sources()))
	}

	for _, source := range []string{"", "claude", "Runtime", "RUNTIME", "user ", "browser"} {
		if ValidSource(source) {
			t.Errorf("ValidSource(%q) = true, want false", source)
		}
	}
}

// TestDeclaredTypesAreWellFormed keeps the constants and the validator in
// agreement. A type constant the validator would refuse is a constant that
// cannot be written, which is a bug that only shows up at the first start.
func TestDeclaredTypesAreWellFormed(t *testing.T) {
	declared := []string{
		TypeRuntimeStarted,
		TypeRuntimeStopped,
		TypeRuntimeError,
		TypeRuntimeDestroyed,
	}
	for _, name := range declared {
		if !ValidType(name) {
			t.Errorf("the declared type %q is not well-formed", name)
		}
	}
}
