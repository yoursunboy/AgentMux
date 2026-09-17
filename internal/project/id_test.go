package project

import (
	"strings"
	"testing"
)

func TestNewIDShape(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatalf("NewID returned an error: %v", err)
	}
	if !strings.HasPrefix(id, IDPrefix) {
		t.Errorf("NewID() = %q, want the %q prefix", id, IDPrefix)
	}
	if len(id) != len(IDPrefix)+idBodyLen {
		t.Errorf("NewID() = %q, want %d characters", id, len(IDPrefix)+idBodyLen)
	}
	if !ValidID(id) {
		t.Errorf("ValidID(%q) = false for an identifier NewID produced", id)
	}
	for _, r := range id[len(IDPrefix):] {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Errorf("NewID() = %q, which contains %q; the body must be lower-case hex", id, r)
		}
	}
}

// TestNewIDIsUnique is a smoke test on the property the ID exists for: ten
// bytes of entropy is far more than a single installation can collide on, and
// a duplicate would make two projects share one tmux session name.
func TestNewIDIsUnique(t *testing.T) {
	const count = 2000
	seen := make(map[string]bool, count)
	for i := 0; i < count; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID returned an error on iteration %d: %v", i, err)
		}
		if seen[id] {
			t.Fatalf("NewID produced the duplicate identifier %q after %d calls", id, i)
		}
		seen[id] = true
	}
}

func TestValidID(t *testing.T) {
	valid, err := NewID()
	if err != nil {
		t.Fatalf("NewID returned an error: %v", err)
	}

	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"a generated identifier", valid, true},
		{"empty", "", false},
		{"the prefix alone", IDPrefix, false},
		{"missing the prefix", strings.TrimPrefix(valid, IDPrefix), false},
		{"wrong prefix", "x_" + strings.TrimPrefix(valid, IDPrefix), false},
		{"too short", IDPrefix + "abc", false},
		{"too long", valid + "a", false},
		{"upper-case hex", IDPrefix + strings.ToUpper(strings.TrimPrefix(valid, IDPrefix)), false},
		{"a non-hex character", IDPrefix + strings.Repeat("z", idBodyLen), false},
		{"a path traversal attempt", "../../etc/passwd", false},
		{"a name rather than an identifier", "AgentMux", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidID(tt.id); got != tt.want {
				t.Errorf("ValidID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

// TestSessionNameComesFromTheID is the rule that keeps a running terminal
// alive across a rename.
func TestSessionNameComesFromTheID(t *testing.T) {
	p := &Project{ID: "p_0123456789abcdef0123", Name: "Before"}
	before := p.SessionName()
	if before != "amx-p_0123456789abcdef0123" {
		t.Errorf("SessionName() = %q, want %q", before, "amx-p_0123456789abcdef0123")
	}

	p.Name = "After with spaces and / symbols"
	if after := p.SessionName(); after != before {
		t.Errorf("SessionName() changed from %q to %q after a rename; the session must survive a rename",
			before, after)
	}
}

func TestCloneIsDeep(t *testing.T) {
	slot := 3
	p := &Project{ID: "p_1", Name: "App", PinnedSlot: &slot}

	clone := p.Clone()
	clone.Name = "Other"
	*clone.PinnedSlot = 9

	if p.Name != "App" {
		t.Errorf("mutating the clone changed the original name to %q", p.Name)
	}
	if *p.PinnedSlot != 3 {
		t.Errorf("mutating the clone changed the original pinned slot to %d", *p.PinnedSlot)
	}
	if (*Project)(nil).Clone() != nil {
		t.Error("cloning a nil project must return nil")
	}
}
