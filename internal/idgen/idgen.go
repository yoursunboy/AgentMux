// Package idgen generates the random identifiers AgentMux names its resources
// by.
//
// Every identifier in this codebase has the same shape: a prefix that says what
// kind of thing it names, followed by random bytes in lowercase hex.
//
//	p_6f1a…        a project
//	evt_3c9b…      an event
//	task_44e0…     a task
//	sess_9a27…     an agent session
//
// # Why the identifier is generated and not assigned
//
// Identity the storage engine hands out is identity only the storage engine
// knows. Two AgentMux installations writing to two databases cannot agree on an
// integer, and a row that has to be stored before it can be named cannot be
// logged, queued or handed to a client before it is written. A random
// identifier is unique before it exists anywhere, which is what makes the rest
// possible.
//
// # Why this is one package
//
// The shape was written three times before this package existed - once in the
// project model, once in the event model, and once here for tasks and sessions.
// Three copies of twenty lines is not a crisis, but it is three places for the
// entropy, the alphabet or the prefix rule to drift apart, and the failure that
// produces is an identifier that one package accepts and another rejects.
// A Spec is the whole of the configuration; nothing else about an identifier is
// per-resource.
//
// The terminal package's client identifiers are deliberately not built here.
// They are not resource names: a client id is machine-local, never stored, and
// bounded by a protocol constant rather than by a fixed length.
//
// # The one identifier that is not AgentMux's to shape
//
// UUIDv4 exists beside Spec rather than as a Spec because its format is not
// this package's decision. Every other identifier here is AgentMux naming its
// own resource, so AgentMux chooses the shape. A session id handed to an
// external tool is the opposite: the tool states the format, and a value that
// does not match is rejected by something outside this codebase. It is here
// rather than at the call site so that the version and variant bits are written
// once.
package idgen

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// uuidLen is the length of the canonical 8-4-4-4-12 form.
const uuidLen = 36

// UUIDv4 returns a random version-4 UUID in the canonical hyphenated form.
//
// The version and variant bits are set rather than left random: a value that
// merely looks like a UUID is not one, and a consumer that validates the format
// is entitled to reject it. Bytes 6 and 8 carry them, per RFC 4122 §4.4.
//
// It fails only if the platform's random source does, for the same reason Spec
// .New does: a server that cannot name the thing it is about to start has a
// broken host, and saying so is more useful than a stack trace.
func UUIDv4() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("idgen: generate a uuid: %w", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10

	var b strings.Builder
	b.Grow(uuidLen)
	body := hex.EncodeToString(buf)
	for i := 0; i < len(body); i++ {
		switch i {
		case 8, 12, 16, 20:
			b.WriteByte('-')
		}
		b.WriteByte(body[i])
	}
	return b.String(), nil
}

// Spec describes one kind of identifier.
//
// It is a struct rather than a set of arguments because it is a property of the
// resource, not of a call: a package declares its spec once, beside the type it
// names, and both the generator and the validator are derived from it. That is
// what makes "the ids this server produces" and "the ids this server accepts"
// the same set by construction.
type Spec struct {
	// Prefix marks the identifier's kind, and ends with an underscore:
	// "p_", "evt_", "task_", "sess_".
	Prefix string

	// Bytes is the entropy per identifier.
	//
	// Eight bytes is 64 bits, which is enough for an installation that will
	// never hold a billion of anything. Sixteen is 128 bits, the same entropy a
	// version-4 UUID carries, and is used where the identifier may be handed to
	// something outside this installation - an event id is a pagination cursor
	// and appears in a URL.
	Bytes int
}

// BodyLen is the number of hex characters in the random part.
func (s Spec) BodyLen() int { return s.Bytes * 2 }

// String renders the spec for a log line or an error message.
func (s Spec) String() string {
	return fmt.Sprintf("%s<%d bytes>", s.Prefix, s.Bytes)
}

// New returns a fresh identifier.
//
// It fails only if the platform's random source does. That is worth reporting
// rather than panicking on: a server that cannot name a row it is about to
// write has a broken host, and saying so is more useful than a stack trace.
func (s Spec) New() (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	buf := make([]byte, s.Bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("idgen: generate %s id: %w", s.Prefix, err)
	}
	return s.Prefix + hex.EncodeToString(buf), nil
}

// Valid reports whether id has exactly the shape New produces.
//
// It is exact rather than permissive: the prefix must match, the body must be
// the right length, and every character must be lowercase hex. A validator that
// accepted a longer body would let an identifier from a different generator
// through, and the check exists precisely to stop that.
func (s Spec) Valid(id string) bool {
	if err := s.validate(); err != nil {
		return false
	}
	if !strings.HasPrefix(id, s.Prefix) {
		return false
	}
	body := id[len(s.Prefix):]
	if len(body) != s.BodyLen() {
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

// validate reports whether the spec itself is usable.
//
// A spec with no prefix or no entropy is a programming error, and it is caught
// here rather than producing identifiers that all look alike or none that
// validate.
func (s Spec) validate() error {
	if s.Prefix == "" {
		return fmt.Errorf("idgen: a spec needs a prefix")
	}
	if s.Bytes < 1 {
		return fmt.Errorf("idgen: spec %q needs at least one byte of entropy", s.Prefix)
	}
	return nil
}
