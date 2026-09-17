// Package terminal carries a project's terminal to a browser over one
// WebSocket.
//
// # What this package is
//
// It is a transport, and nothing else. It subscribes to the runtime manager,
// frames what the manager publishes, and writes it to a socket. It does not
// know what tmux is, how a session is started, or where a project's directory
// is: every one of those questions is answered by internal/session, and this
// package asks it rather than working it out.
//
// The direction matters more than the layering does. A terminal transport that
// reached a tmux socket itself would be a second path to the same terminal,
// and two paths to one terminal is how a server ends up with a control
// connection per browser tab, a pane that resizes when somebody closes a
// window, and output that arrives twice or not at all depending on which
// connection won. There is one control connection per runtime, it belongs to
// the manager, and this package is one of its readers - which is exactly what
// a browser is, in the manager's data model.
//
// # The wire
//
// Terminal output travels in binary frames; everything else travels in text
// frames carrying JSON. That split is deliberate: terminal bytes are not text,
// and a JSON envelope around a screenful of escape sequences would mean
// base64, a third more bytes, and an encoding step on the hot path. Control is
// the opposite - rare, small, and worth being readable in a log or a browser
// devtools frame list.
//
// The protocol is versioned and the version is checked in both directions, so
// a stale tab left open across a server upgrade is told that it is stale
// instead of being fed bytes it will mis-draw.
package terminal

import "encoding/json"

// ProtocolVersion is the version of the wire protocol in this file.
//
// It must be raised whenever a change would make an older client draw
// something wrong rather than fail: a new frame type is additive and does not
// need it, a change to an existing frame's header does.
const ProtocolVersion = 1

// Endpoint is the only real-time endpoint the server serves.
//
// It is one socket per browser rather than one per terminal. A browser that
// opens a second socket to subscribe to a second project doubles the connection
// count, the ping traffic and the failure modes for no gain: subscriptions are
// multiplexed on one socket, and the frames carry the project they belong to.
const Endpoint = "/api/ws"

// ProtocolParam is the query parameter a client uses to state the version it
// speaks.
const ProtocolParam = "v"

// Subprotocol is the WebSocket subprotocol name.
//
// It is offered and echoed for the benefit of anything in the middle that
// understands subprotocols, and it carries the same version as the query
// parameter so a proxy's log says which protocol was negotiated.
const Subprotocol = "agentmux.terminal.v1"

// Client message types: browser to server.
//
// Every one of them is a JSON object with a "type" field. There is no binary
// message from a client at all: raw terminal input is bytes, but it is bytes
// the client is sending rather than bytes a terminal is producing, and routing
// it through the same typed, size-limited, individually rejectable channel as
// every other client intent is worth the base64.
const (
	// MsgSubscribe asks for a project's terminal.
	//
	// It carries an optional terminal size. A browser knows how large its
	// viewport is before it knows anything else, and telling the server up front
	// means the runtime is resized once, before the snapshot is taken, instead
	// of the client drawing a screen at the old size and then reflowing.
	MsgSubscribe = "subscribe"

	// MsgUnsubscribe releases a project's terminal.
	MsgUnsubscribe = "unsubscribe"

	// MsgInput is raw terminal input: keystrokes, pasted text, control
	// characters, escape sequences.
	MsgInput = "input"

	// MsgResize sets the canonical terminal size.
	MsgResize = "resize"

	// MsgResync asks for the terminal to be re-established from a fresh
	// snapshot, because the client knows it lost output.
	//
	// It carries no reason field, and that is a decision rather than an
	// omission. A reason would be free text from a browser, and free text from
	// a browser that reaches a log is a place terminal output - or a prompt -
	// can end up somewhere nobody meant it to be. The server learns everything
	// it needs from the sequence number it stopped at, and it knows that
	// already.
	MsgResync = "resync"

	// MsgPing is an application-level liveness check. It is not the WebSocket
	// ping: this one proves the client's own message loop is running, and it is
	// answered by the server's, which is the pair that a stalled tab breaks.
	MsgPing = "ping"
)

// Server message types: server to browser.
const (
	// MsgHello is the first message on every connection. It states the
	// protocol version and the server's identity, so a client can refuse to go
	// further rather than guessing.
	MsgHello = "hello"

	// MsgUnsubscribed acknowledges that a subscription ended. It is sent so
	// that a client tearing down a terminal knows the server has stopped
	// sending for it, and can distinguish that from a stream that quietly
	// stopped.
	MsgUnsubscribed = "unsubscribed"

	// MsgResized reports a project's canonical size after it changed.
	//
	// It is sent to every subscriber of that project, not only to the one that
	// asked. There is one pty, so there is one size; a second tab rendering at
	// its own idea of the size would draw a terminal that does not match the
	// one the program inside is drawing for.
	MsgResized = "resized"

	// MsgError reports a failed client message. It names the message it is
	// about, so a client with several subscriptions can tell which one broke.
	MsgError = "error"

	// MsgPong answers MsgPing.
	MsgPong = "pong"
)

// Limits.
//
// Every one of these exists because the input is a browser, which is to say
// untrusted code on a machine the server does not control. They are not
// defensive decoration: a read limit that is missing is a read limit that a
// stuck or hostile client can exceed until the server runs out of memory.
const (
	// MaxClientMessageBytes is the largest single message the server will read
	// from a client.
	//
	// It has to cover the base64 of a MaxInputBytes payload plus the JSON
	// around it, with room to spare for the fields of the largest control
	// message. Anything larger is a client that is not speaking this protocol,
	// and the WebSocket library closes the connection when the limit is
	// reached rather than buffering the excess.
	MaxClientMessageBytes = 160 << 10

	// MaxInputBytes is the largest decoded terminal input in one message.
	//
	// It is far more than a prompt and more than a person types. A paste
	// larger than this is sent as several messages: the terminal does not care
	// where the boundaries fall, because what it receives is a byte stream, and
	// a client that chunks a paste delivers the same bytes in the same order.
	MaxInputBytes = 48 << 10

	// MaxSubscriptions is how many projects one browser may watch at once.
	//
	// The grid in a later phase is six. This is deliberately above that and
	// deliberately finite: without a cap, a client can ask for a subscription
	// in a loop and make the server hold one manager watcher, one goroutine and
	// one output queue per iteration.
	MaxSubscriptions = 16

	// MaxProjectIDLen bounds the project identifier a client may name. Real
	// identifiers are 22 characters; the limit is generous so that it never
	// rejects a legitimate one and tight enough that it cannot be used to make
	// the server hold a large string.
	MaxProjectIDLen = 64

	// MaxReasonLen bounds a client-supplied string that the server echoes back
	// or logs.
	MaxReasonLen = 200

	// MinCols, MinRows, MaxCols and MaxRows bound a requested terminal size.
	//
	// A pty can be made absurdly small or absurdly large, and both are worse
	// than refusing: a 1x1 pty makes every program inside redraw continuously,
	// and a 10000-column one makes tmux's grid enormous for a window that is
	// 200 columns wide. The bounds are wide enough for any real display,
	// including a rotated tablet and a 4K monitor at a small font.
	MinCols = 20
	MinRows = 5
	MaxCols = 500
	MaxRows = 300
)

// Error codes the terminal layer reports.
//
// They are a separate namespace from the runtime's codes on purpose. A client
// switching on an error needs to know whether the terminal protocol was spoken
// wrongly, which is a bug in the client, or whether the runtime refused, which
// is a state of the world. Runtime errors are passed through with their own
// codes; these are the transport's.
const (
	// CodeBadMessage means a client message was not valid protocol: malformed
	// JSON, an unknown type, a missing or ill-typed field.
	CodeBadMessage = "bad_message"

	// CodeUnsupported means the message was understood but this server will not
	// act on it: a binary message from a client, an unknown protocol version.
	CodeUnsupported = "unsupported"

	// CodeBadProject means the named project is not one this server can serve.
	CodeBadProject = "bad_project"

	// CodeTooManySubscriptions means the connection is already watching as many
	// projects as it may.
	CodeTooManySubscriptions = "too_many_subscriptions"

	// CodeNotSubscribed means a message named a project this connection is not
	// watching. Input and resize are only accepted for a terminal the client
	// has already asked for.
	CodeNotSubscribed = "not_subscribed"

	// CodeInputTooLarge means one input message carried more than
	// MaxInputBytes after decoding.
	CodeInputTooLarge = "input_too_large"

	// CodeStreamUnstable means the connection could not keep up with a
	// project's output and re-synchronising did not help. The subscription is
	// dropped rather than retried forever, because a retry loop against a
	// runtime that is outrunning the connection is a loop that never ends.
	CodeStreamUnstable = "stream_unstable"

	// CodeInternal means the server failed in a way the client cannot act on.
	CodeInternal = "internal"
)

// clientMessage is the envelope every client message shares.
//
// It is decoded in one pass and then checked against the message's type, which
// is what hasExtraFields is for. One pass matters because this is the hot path
// - a keystroke is a message - and a per-type struct would mean decoding every
// message twice, once to learn its type and once to read it.
type clientMessage struct {
	Type      string `json:"type"`
	ProjectID string `json:"projectId,omitempty"`
	Data      []byte `json:"data,omitempty"`
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
}

// hasExtraFields reports whether a message carries a field beyond its type and
// its project.
//
// It is what makes the protocol strict without a second decode. The envelope
// has to hold every field of every message, because the type is only known
// after it is parsed; this is how a "cols" on a subscribe is still refused
// rather than silently ignored.
func (m clientMessage) hasExtraFields() bool {
	return len(m.Data) > 0 || m.Cols != 0 || m.Rows != 0
}

// Server messages.
//
// Each is a struct rather than a map so that the field order, the names and
// the omitempty decisions are in one readable place, and so that a test can
// assert on the JSON a client will actually receive.

type helloMessage struct {
	Type     string `json:"type"`
	Protocol int    `json:"protocol"`
	ClientID string `json:"clientId"`
	Server   string `json:"server"`
	Version  string `json:"version"`
}

type unsubscribedMessage struct {
	Type      string `json:"type"`
	ProjectID string `json:"projectId"`
}

type resizedMessage struct {
	Type      string `json:"type"`
	ProjectID string `json:"projectId"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

type errorMessage struct {
	Type      string `json:"type"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	ProjectID string `json:"projectId,omitempty"`
	// About names the client message type that failed, when there was one.
	About string `json:"about,omitempty"`
}

type pongMessage struct {
	Type string `json:"type"`
}

// encodeJSON renders a server message.
//
// It does not return an error, and that is a decision rather than an
// oversight: every value passed here is a struct of strings and integers built
// in this package, and a struct of strings and integers cannot fail to encode.
// A caller that had to handle an error from this would be handling one that
// cannot happen, and the handler for it would be a lie.
func encodeJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		// Unreachable for the types above. Panicking is right: the alternative
		// is a connection that silently stops reporting its errors.
		panic("terminal: server message is not encodable: " + err.Error())
	}
	return data
}
