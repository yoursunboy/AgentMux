package idgen

import (
	"strings"
	"testing"
)

// testSpec is an ordinary spec: a prefix and enough entropy to be unique.
var testSpec = Spec{Prefix: "x_", Bytes: 8}

func TestNewHasTheDeclaredShape(t *testing.T) {
	id, err := testSpec.New()
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	if !strings.HasPrefix(id, "x_") {
		t.Errorf("New() = %q, want the %q prefix", id, "x_")
	}
	if got, want := len(id), len("x_")+16; got != want {
		t.Errorf("New() produced %d characters, want %d", got, want)
	}
	if !testSpec.Valid(id) {
		t.Errorf("New() produced %q, which Valid rejects", id)
	}
}

// TestNewIsUnique is the only property that actually matters, and it is checked
// over enough identifiers that a collision would mean the entropy is not being
// drawn from where it is supposed to be.
func TestNewIsUnique(t *testing.T) {
	const n = 5000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id, err := testSpec.New()
		if err != nil {
			t.Fatalf("New returned an error: %v", err)
		}
		if seen[id] {
			t.Fatalf("New returned %q twice in %d identifiers", id, n)
		}
		seen[id] = true
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"a generated id", "x_0123456789abcdef", true},
		{"all digits", "x_0000000000000000", true},

		{"empty", "", false},
		{"no prefix", "0123456789abcdef", false},
		{"the wrong prefix", "y_0123456789abcdef", false},
		{"a prefix with no body", "x_", false},
		{"a body one character short", "x_0123456789abcde", false},
		{"a body one character long", "x_0123456789abcdef0", false},
		{"uppercase hex", "x_0123456789ABCDEF", false},
		{"non-hex letters", "x_0123456789abcdeg", false},
		{"a prefix inside the body", "x_x_123456789abcd", false},
		{"whitespace", "x_0123456789abcdef ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := testSpec.Valid(tt.id); got != tt.want {
				t.Errorf("Valid(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

// TestSpecsDoNotValidateEachOther is the reason Valid checks the length exactly
// rather than accepting anything longer: two resources with different entropy
// must not accept each other's identifiers, or a task id could be passed where
// a session id belongs and be stored.
func TestSpecsDoNotValidateEachOther(t *testing.T) {
	short := Spec{Prefix: "s_", Bytes: 4}
	long := Spec{Prefix: "l_", Bytes: 16}

	shortID, err := short.New()
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	longID, err := long.New()
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}

	if !short.Valid(shortID) || !long.Valid(longID) {
		t.Fatal("a spec rejected an identifier it generated")
	}
	if short.Valid(longID) {
		t.Errorf("the %d-byte spec accepted %q, a %d-byte identifier", short.Bytes, longID, long.Bytes)
	}
	if long.Valid(shortID) {
		t.Errorf("the %d-byte spec accepted %q, a %d-byte identifier", long.Bytes, shortID, short.Bytes)
	}
	// The same body under a different prefix is a different kind of thing.
	other := Spec{Prefix: "o_", Bytes: short.Bytes}
	if other.Valid(shortID) {
		t.Errorf("the %q spec accepted %q", other.Prefix, shortID)
	}
}

// TestUnusableSpecsAreReported pins the failure mode of a spec that was written
// wrong: it must not silently produce identifiers that all validate.
func TestUnusableSpecsAreReported(t *testing.T) {
	broken := []Spec{
		{Prefix: "", Bytes: 8},
		{Prefix: "x_", Bytes: 0},
		{Prefix: "x_", Bytes: -1},
	}
	for _, spec := range broken {
		if _, err := spec.New(); err == nil {
			t.Errorf("New() succeeded for %s, want an error", spec)
		}
		if spec.Valid("x_0123456789abcdef") {
			t.Errorf("Valid() accepted an identifier for %s, want false", spec)
		}
	}
}

func TestBodyLenAndString(t *testing.T) {
	spec := Spec{Prefix: "task_", Bytes: 12}
	if got, want := spec.BodyLen(), 24; got != want {
		t.Errorf("BodyLen() = %d, want %d", got, want)
	}
	if got, want := spec.String(), "task_<12 bytes>"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// UUIDv4
// ---------------------------------------------------------------------------

func TestUUIDv4HasTheCanonicalForm(t *testing.T) {
	id, err := UUIDv4()
	if err != nil {
		t.Fatalf("UUIDv4 returned an error: %v", err)
	}
	if len(id) != uuidLen {
		t.Fatalf("UUIDv4() = %q, which is %d characters, want %d", id, len(id), uuidLen)
	}
	for _, i := range []int{8, 13, 18, 23} {
		if id[i] != '-' {
			t.Errorf("UUIDv4() = %q, want a hyphen at index %d", id, i)
		}
	}
	for i, c := range id {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			t.Fatalf("UUIDv4() = %q, want only lowercase hex outside the hyphens; %q is at index %d", id, c, i)
		}
	}
}

// TestUUIDv4CarriesItsVersionAndVariant is the property that separates a UUID
// from a string that merely looks like one. A consumer is entitled to check it.
func TestUUIDv4CarriesItsVersionAndVariant(t *testing.T) {
	for i := 0; i < 500; i++ {
		id, err := UUIDv4()
		if err != nil {
			t.Fatalf("UUIDv4 returned an error: %v", err)
		}
		if id[14] != '4' {
			t.Fatalf("UUIDv4() = %q, want version 4 at index 14", id)
		}
		switch id[19] {
		case '8', '9', 'a', 'b':
		default:
			t.Fatalf("UUIDv4() = %q, want an RFC 4122 variant at index 19", id)
		}
	}
}

func TestUUIDv4IsUnique(t *testing.T) {
	const n = 5000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id, err := UUIDv4()
		if err != nil {
			t.Fatalf("UUIDv4 returned an error: %v", err)
		}
		if seen[id] {
			t.Fatalf("UUIDv4 returned %q twice in %d identifiers", id, n)
		}
		seen[id] = true
	}
}

// TestUUIDv4DoesNotMatchAnySpec pins that the two generators are separate. A
// prefixed identifier is not a UUID, and a UUID must not be accepted where this
// package's own shape is required.
func TestUUIDv4DoesNotMatchAnySpec(t *testing.T) {
	id, err := UUIDv4()
	if err != nil {
		t.Fatalf("UUIDv4 returned an error: %v", err)
	}
	if testSpec.Valid(id) {
		t.Errorf("Valid(%q) = true for a UUID, want false", id)
	}
	if strings.HasPrefix(id, "_") || strings.Contains(id, "_") {
		t.Errorf("UUIDv4() = %q, want the hyphenated form", id)
	}
}
