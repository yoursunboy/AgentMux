package session

import (
	"context"
	"errors"
)

// Sentinel errors a backend reports, so the manager can react to a condition
// rather than to a message.
var (
	// ErrSessionExists means a session with that name already exists.
	ErrSessionExists = errors.New("session already exists")

	// ErrNoSuchSession means no session with that name exists.
	ErrNoSuchSession = errors.New("no such session")

	// ErrSessionExited means a live output stream ended because the session
	// is gone. It is a normal end, not a failure.
	ErrSessionExited = errors.New("session exited")
)

// Backend is the persistent terminal runtime.
//
// This is the SessionBackend: the one interface through which AgentMux creates,
// talks to, and destroys a terminal that outlives it. tmux implements it today.
// A ConPTY, SSH, or container backend would implement the same methods, which
// is the whole reason the boundary is drawn here and not around tmux's command
// set.
//
// The name is Backend rather than SessionBackend because the package already
// says session; session.SessionBackend would stutter without adding meaning.
//
// # What a backend is not
//
// A backend knows nothing about projects, HTTP, or prompts. It is handed a
// name, a directory, and a size. In particular it has no concept of a "Claude
// prompt": the runtime delivers terminal bytes and key input, and interpreting
// them is somebody else's job. A backend that understood prompts could not
// host a plain shell, which is exactly what Phase 2 requires.
//
// # Byte fidelity
//
// Terminal bytes cross this interface unchanged. Implementations must not
// strip ANSI sequences, trim whitespace, normalise line endings, translate
// encodings, or drop control characters. A terminal that rewrites its output
// is not a terminal.
//
// # Concurrency
//
// Every method is safe to call concurrently. Methods that mutate one session
// may serialise against each other internally, but a slow session must never
// block operations on another.
type Backend interface {
	// Name identifies the backend, for example "tmux". It is recorded with the
	// runtime so a future release can tell which backend owns a session.
	Name() string

	// Available reports whether this backend can be used here, as an error
	// explaining what is missing when it cannot. It is a check, not a
	// capability probe: it must be cheap enough to call before an operation.
	Available(ctx context.Context) error

	// Create starts a new detached session. It returns ErrSessionExists if the
	// name is taken.
	//
	// The session must be created independently of any client, so that it
	// survives the client that asked for it.
	Create(ctx context.Context, spec SessionSpec) (*Session, error)

	// Exists reports whether a session with that name exists.
	Exists(ctx context.Context, name string) (bool, error)

	// Inspect returns one session's current state, or ErrNoSuchSession.
	Inspect(ctx context.Context, name string) (*Session, error)

	// List returns every session this backend owns.
	List(ctx context.Context) ([]*Session, error)

	// Launch runs a command inside an existing session by typing it into the
	// session's terminal. It is how work is started in a shell, and it is
	// deliberately not an exec: the command must be visible, interruptible, and
	// part of the session's scrollback.
	Launch(ctx context.Context, name string, command string) error

	// SendInput writes raw terminal bytes to a session: keystrokes, pasted
	// text, control characters, escape sequences. It is the input half of the
	// byte contract and must be able to carry any byte value, not just
	// printable ASCII.
	SendInput(ctx context.Context, name string, data []byte) error

	// Resize changes the session's terminal size, which must take effect in
	// the process running inside it - not merely in a variable the caller
	// keeps.
	Resize(ctx context.Context, name string, cols, rows int) error

	// Stop interrupts whatever is running in the session, leaving the session
	// itself alive. It is the difference between Ctrl-C and closing the
	// window.
	Stop(ctx context.Context, name string) error

	// Destroy removes the session and everything running in it. It is
	// destructive and irreversible.
	Destroy(ctx context.Context, name string) error

	// KillServer stops the server this backend owns, ending every session on
	// it at once.
	//
	// It is the one operation whose blast radius is larger than a session, and
	// it is deliberately part of the interface rather than a capability a
	// caller discovers: whether a shared server may be stopped is a question
	// the architecture has to answer, and since Phase 2.5 the answer is that a
	// backend's server holds exactly one project's runtimes, so stopping it is
	// a project-scoped act. A backend that cannot honour "this stops only
	// yours" must not implement this as anything broader.
	KillServer(ctx context.Context) error

	// Snapshot returns the session's current screen, escape sequences intact,
	// for a client that needs to draw something before live output arrives.
	Snapshot(ctx context.Context, name string) ([]byte, error)

	// Attach opens a live output stream for a session. The caller must Close
	// the subscription. Several subscriptions to one session may be open at
	// once and each must receive the same bytes.
	Attach(ctx context.Context, name string) (Subscription, error)

	// Close releases the backend's own resources. It must not destroy
	// sessions: they are the point of the runtime and outlive their clients.
	Close() error
}

// Subscription is a live stream of one session's output.
//
// It delivers whatever the session produced, in order, as raw bytes. It does
// not buffer unboundedly on the caller's behalf: a caller that stops reading
// will eventually block the stream, which is preferred over silently dropping
// terminal output.
type Subscription interface {
	// Name is the session this stream belongs to.
	Name() string

	// Output is the stream. It is closed when the session ends or the
	// subscription is closed, whichever comes first.
	Output() <-chan []byte

	// Connected reports whether the stream is live right now. A subscription
	// that has dropped but whose session still exists is reconnecting, and
	// saying so is more useful than reporting the session as healthy or as
	// dead.
	Connected() bool

	// Err reports why the stream ended. It returns nil while the stream is
	// live and after a clean Close, and ErrSessionExited when the session
	// itself went away.
	Err() error

	// Close stops the stream and releases its resources. Closing twice is
	// safe.
	Close() error
}
