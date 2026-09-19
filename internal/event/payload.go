package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Payload limits.
const (
	// MaxPayloadBytes bounds a payload at 4 KiB.
	//
	// An event is a fact, not a document. The bound is what keeps a row small
	// enough that reading a timeline is cheap, and it is small enough that a
	// caller who has something larger to store has found the wrong place to
	// store it.
	MaxPayloadBytes = 4 << 10

	// maxPayloadDepth bounds nesting. 4 KiB of "[[[[..." nests about two
	// thousand deep, which is more recursion than a walk over untrusted input
	// should be handed, so the depth is refused rather than followed.
	maxPayloadDepth = 32
)

// forbiddenPayloadKey is one entry in the vocabulary a payload field name may
// not use.
type forbiddenPayloadKey struct {
	// suffix is the normalised form the check compares against, and the entry
	// matches a field name that *ends* with it as well as one that is exactly
	// it.
	//
	// The suffix rule is what makes the check useful. The bare word is not how
	// these fields are named in practice - `db_password`, `github_token` and
	// `aws_secret_access_key` are - and a check that matched only the exact
	// word would let every one of those through while catching a field called
	// `token` that nobody writes.
	suffix string

	// what names the kind of credential the field looks like, for the error
	// message.
	what string
}

// forbiddenPayloadKeys names the fields a payload may not have.
//
// This is a check on the *shape* of the JSON and not a scan of its text, and
// the distinction is the whole reason it is worth having. Scanning values would
// be unenforceable - a secret that does not look like one passes, and a
// legitimate message containing the word "token" fails - while a field named
// `token` is a mistake somebody makes on purpose and can be caught exactly.
//
// A name is normalised before it is compared, so `api_key`, `apiKey` and
// `API-KEY` are one field. The entries are checked in order and the first one a
// name ends with wins, so a name that matches two of them is reported the same
// way on every run.
//
// It is a guard rail, not a guarantee. The guarantee is that nothing in this
// phase has a credential to put in a payload: the one caller is the runtime
// bridge, whose payloads are a state name and a terminal size.
var forbiddenPayloadKeys = []forbiddenPayloadKey{
	{"password", "password"},
	{"passwd", "password"},
	{"pass", "password"},
	{"pwd", "password"},
	{"token", "token"},
	{"secret", "secret"},
	{"credential", "credential"},
	{"credentials", "credential"},
	{"apikey", "API key"},
	{"accesskey", "API key"},
	{"secretkey", "API key"},
	{"authorization", "credential"},
	{"privatekey", "private key"},
	{"sshkey", "private key"},
	{"cookie", "session credential"},
	{"sessionid", "session credential"},
}

// forbiddenKey returns what a normalised field name looks like, or "" when the
// name is allowed.
func forbiddenKey(normalized string) string {
	for _, entry := range forbiddenPayloadKeys {
		if strings.HasSuffix(normalized, entry.suffix) {
			return entry.what
		}
	}
	return ""
}

// CheckPayload refuses a payload that an immutable, backed-up row must not
// carry.
//
// A stored event is never updated and never deleted, and the database it lives
// in gets backed up. That combination is what makes the rules below hard rather
// than advisory: there is no later moment at which a mistake in a payload can
// be corrected.
//
// The rules are:
//
//   - the payload must be a JSON object, or empty;
//   - it must be at most MaxPayloadBytes;
//   - it must not nest more than maxPayloadDepth deep;
//   - no field anywhere in it may be named after a credential.
//
// It does not and cannot check for terminal output. That rule is kept by the
// payloads being what they are - see docs/AGENT_EVENTS.md §7.
func CheckPayload(payload json.RawMessage) error {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if len(trimmed) > MaxPayloadBytes {
		return newError(CodeInvalidEvent,
			"an event payload must be at most %d bytes; this one is %d",
			MaxPayloadBytes, len(trimmed)).
			withDetail("bytes", len(trimmed)).
			withDetail("maxBytes", MaxPayloadBytes)
	}

	var decoded map[string]any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return newError(CodeInvalidEvent,
			"an event payload must be a JSON object: %s", payloadDecodeReason(err))
	}
	// A second value in the stream means the caller sent something that is not
	// one object, which the first decode would have stopped at.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return newError(CodeInvalidEvent,
			"an event payload must contain a single JSON object")
	}

	if offence := walkPayload(decoded, 0); offence != nil {
		return newError(CodeInvalidEvent,
			"an event payload is refused: %s", offence.Error()).
			withDetail("field", offence.key)
	}
	return nil
}

// payloadOffence is what a payload was refused for.
type payloadOffence struct {
	// key is the offending field name. Empty when the problem is the shape of
	// the payload rather than a field in it.
	key string

	// what names the kind of credential the field looks like.
	what string

	// reason explains a shape problem.
	reason string
}

// Error implements error, and reads as the second half of the message the
// caller builds: "an event payload is refused: it has a field named ...".
func (o *payloadOffence) Error() string {
	if o.key != "" {
		return fmt.Sprintf("it has a field named %q, which names a %s", o.key, o.what)
	}
	return o.reason
}

// walkPayload looks through a decoded payload for a forbidden field name,
// returning the first one it finds.
//
// Objects are visited in sorted key order so that a payload with two offending
// fields names the same one on every run - an error message that changes
// between two identical requests is a support ticket nobody can reproduce.
func walkPayload(value any, depth int) *payloadOffence {
	if depth > maxPayloadDepth {
		return &payloadOffence{
			reason: fmt.Sprintf("it nests more than %d levels deep", maxPayloadDepth),
		}
	}
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		for _, key := range keys {
			if what := forbiddenKey(normalizeKey(key)); what != "" {
				return &payloadOffence{key: key, what: what}
			}
		}
		for _, key := range keys {
			if offence := walkPayload(v[key], depth+1); offence != nil {
				return offence
			}
		}
	case []any:
		for _, item := range v {
			if offence := walkPayload(item, depth+1); offence != nil {
				return offence
			}
		}
	}
	return nil
}

// normalizeKey reduces a field name to the form the forbidden list is written
// in: lowercased, with the separators a JSON key might use removed.
//
//	"api_key", "apiKey", "API-KEY" and "api key" all become "apikey"
func normalizeKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + ('a' - 'A'))
		case c == '_', c == '-', c == '.', c == ' ', c == '/':
			// Dropped: the separators are spelling, and the check is about what
			// the field is called.
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// payloadDecodeReason turns a JSON decode failure into the part of it a caller
// can act on, in the same spirit as the HTTP layer's decoder.
func payloadDecodeReason(err error) string {
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	switch {
	case errors.As(err, &typeErr):
		return fmt.Sprintf("field %q has the wrong type", typeErr.Field)
	case errors.As(err, &syntaxErr):
		return fmt.Sprintf("it is not valid JSON at byte %d", syntaxErr.Offset)
	default:
		return strings.TrimPrefix(err.Error(), "json: ")
	}
}
