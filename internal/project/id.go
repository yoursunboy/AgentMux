package project

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// IDPrefix marks a project identifier. It makes an ID recognisable in a log
// line or a tmux session list.
const IDPrefix = "p_"

// idRandomBytes is the entropy per identifier. 10 bytes gives 80 bits, which
// is far beyond what a single-user installation can collide on, while keeping
// the identifier short enough to read aloud.
const idRandomBytes = 10

// idBodyLen is the number of hex characters in the random part of an ID.
const idBodyLen = idRandomBytes * 2

// NewID returns a fresh, stable project identifier.
//
// The identifier is random rather than derived from the project name, because
// the runtime session name is built from it: a name-derived ID would silently
// move a project's session whenever the project was renamed.
func NewID() (string, error) {
	buf := make([]byte, idRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("project: generate id: %w", err)
	}
	return IDPrefix + hex.EncodeToString(buf), nil
}

// ValidID reports whether id has the shape NewID produces.
func ValidID(id string) bool {
	if !strings.HasPrefix(id, IDPrefix) {
		return false
	}
	body := id[len(IDPrefix):]
	if len(body) != idBodyLen {
		return false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
