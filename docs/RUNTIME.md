# AgentMux Runtime

This document describes the Phase 2 persistent session runtime: where the server runs, how tmux is
driven, what the `SessionBackend` boundary is, what survives a restart, and what the runtime
deliberately does not do yet.

It is written for someone who has to change this code, or to explain to a user why their terminal
behaved the way it did.

## 1. The run model

**In Windows + WSL mode, the AgentMux Server process itself runs inside WSL.**

```text
Windows host                          WSL distribution (Linux)
────────────                          ────────────────────────
project files on D:\            ←→    /mnt/d/... (drvfs)
VS Code GUI                     
the user's browser              →     AgentMux Server (Go, linux/amd64)
                                      └─ tmux server on socket "agentmux"
                                         └─ pty per project session
                                            └─ bash → whatever runs in it
```

The alternative — a Windows-native server that shells out to `wsl.exe tmux ...` for every terminal
operation — was considered and rejected as the formal runtime:

- The PTY, the process lifecycle, the ANSI bytes, and control mode all live on the Linux side. A
  Windows server would be a proxy for every one of them, which means owning a second copy of the
  session state that can disagree with the first.
- Each terminal event would cross a process boundary, adding a `wsl.exe` invocation to anything
  latency-sensitive.
- Linux and Windows+WSL would end up with two different `SessionBackend` implementations of the same
  thing, and only one of them would be exercised in day-to-day use.

Windows keeps everything that is genuinely Windows': the project files on `D:`, the VS Code GUI, the
launch of the distribution, and — eventually — a launcher. It does not keep the runtime.

### Consequences you will meet

- `GET /api/server` reports `environment` separately from `host`. On Windows the two disagree by
  design: `host: "windows"`, `environment: "windows"`, `runtimeMode: "wsl"`.
- A Windows-native server still runs, and still manages projects, discovers them, and serves the UI.
  It reports `runtimeAvailable: false` with the sentence *"tmux runtime requires AgentMux Server to
  run inside WSL."* It does **not** silently proxy to WSL and it does not offer a terminal control
  that cannot work.
- Dependency probes (`tmux`, shell, `git`) are run **in the runtime environment**, not on the
  Windows host PATH. `GET /api/server` reports where each probe actually looked in
  `dependencies[].probedIn`, because "tmux not found" is useless advice on a machine where tmux is
  installed one command away, inside the distribution.
- The distribution name is detected, never hardcoded. `host.Detect` reads `WSL_DISTRO_NAME` and
  friends; `-runtime-distro` overrides it. Nothing in the code depends on the string
  `Ubuntu-24.04`.

## 2. tmux

### One socket, one namespace

Every AgentMux session lives on a private tmux socket named `agentmux` (`-tmux-socket` to change it,
`-f /dev/null` so the user's `~/.tmux.conf` cannot change AgentMux's behaviour). Two reasons:

- A user's own tmux sessions must not appear in AgentMux's session list, and an AgentMux session must
  not appear in theirs.
- `List` becomes exact. There is no guessing which of somebody's windows belong to an agent.

Sessions are named `amx-{projectId}` — see §4.

### Bootstrap

tmux has a wrinkle that cost an afternoon and is worth recording: a session has to be created by one
client invocation, because there is no state to configure before a server exists.

- `set-option -g ...` on its own cannot start a server; it fails with *"no server running"*.
- `start-server` alone starts a server that exits immediately, because `exit-empty` is on.
- The working pattern is a single invocation:

  ```text
  tmux -L agentmux -f /dev/null start-server \; set-option -g ... \; new-session -d ...
  ```

  The `\;`-joined command list runs inside one client, which is what keeps the server alive between
  the first command and the last.

### Output: control mode, not polling

Live output comes from a long-lived control-mode client, `tmux -C attach-session`. The alternative —
`capture-pane` on a timer — was rejected for concrete reasons, not for elegance:

| Polling with `capture-pane` | Control mode |
| --- | --- |
| Sees one screenful; scrollback between ticks is gone | Receives every byte the pane writes, in order |
| A spinner redrawing at 30 Hz is sampled into nonsense | Every write is delivered |
| Re-encodes the screen as text, losing `\r` vs `\n` | Delivers pane bytes with an exact escape |
| Costs CPU per session per tick, idle or not | Silent when nothing happens |

`internal/session/control.go` is the parser. It understands `%output` and deliberately ignores the
rest: `%begin`/`%end`/`%error` (no commands are sent over this channel), `%exit`, `%pause`/
`%continue`, and the session/window lifecycle family.

**The `%output` escaping contract** — verified against tmux 3.4, and the reason the decoder works on
bytes rather than text:

| Input byte | On the wire |
| --- | --- |
| `\` | `\\` |
| any byte `< 0x20` or `0x7f` | `\ooo` (three octal digits) |
| any byte `>= 0x80` | passed through **raw** |

That last row is the important one. UTF-8 arrives unescaped, so decoding as ASCII turns every
Chinese character into `U+FFFD`. The first version of this decoder did exactly that.

**Commands do not go over the control channel.** Create, `send-keys`, `resize`, and `kill` are one-off
tmux invocations with an exit code and a stderr, so a failed resize is reported to the HTTP request
that asked for it instead of arriving asynchronously. The cost is one short-lived process per command,
which is invisible next to typing.

A control client is deliberately started **without a TTY**. tmux's control mode is designed for pipes,
and the absence of a terminal is what stops the watching client from imposing its own window size on
the session it is watching.

Re-attach after a dropped client backs off 100 ms → 5 s, doubling.

### `-F` output is not decoded

Session metadata is read with `tmux list-sessions -F '#{...}'`. tmux escapes non-printable bytes in
that output — `\ooo` for most, and the C names `\t \v \f \r` for those four — **but it leaves a
literal backslash alone**. Because a literal `\` is not escaped, any decoding step would corrupt a
path containing one. So `-F` output is not decoded at all, and the field separator is `|` rather than
the more obvious tab: the only variable-length field is last and the split is bounded, so a printable
separator that cannot appear escaped is the safe choice.

Also verified: `#{window_width}` and `#{window_height}` exist; `#{session_width}` and
`#{session_height}` do not.

## 3. SessionBackend

`internal/session/backend.go` holds the interface. It is the one boundary through which AgentMux
creates, talks to, and destroys a terminal that outlives it.

```go
type Backend interface {
	Name() string
	Available(ctx context.Context) error

	Create(ctx context.Context, spec SessionSpec) (*Session, error)
	Exists(ctx context.Context, name string) (bool, error)
	Inspect(ctx context.Context, name string) (*Session, error)
	List(ctx context.Context) ([]*Session, error)

	Launch(ctx context.Context, name string, command string) error
	SendInput(ctx context.Context, name string, data []byte) error
	Resize(ctx context.Context, name string, cols, rows int) error

	Stop(ctx context.Context, name string) error
	Destroy(ctx context.Context, name string) error

	Snapshot(ctx context.Context, name string) ([]byte, error)
	Attach(ctx context.Context, name string) (Subscription, error)

	Close() error
}
```

`Subscription` is a live output stream: `Output() <-chan []byte`, `Connected() bool`, `Err() error`,
`Close() error`.

Three things the interface is deliberate about:

- **It has no concept of a prompt.** A backend is handed a name, a directory, and a size. It carries
  terminal bytes and key input. A backend that understood "Claude prompts" could not host a plain
  shell, which is exactly what Phase 2 requires and what the tests exercise.
- **Bytes are byte-exact.** Implementations must not strip ANSI sequences, trim whitespace, normalise
  line endings, or drop control characters. `SendInput` must carry any byte value, and `Snapshot`
  returns `[]byte` for the same reason — the pane is not guaranteed to be valid UTF-8.
- **It is not shaped around tmux.** `ConPTYBackend`, `SSHBackend`, or a container backend would
  implement the same methods. Nothing above this interface knows what `send-keys` is.

It is not abstracted further than that. There is no plugin registry, no backend factory, and no
capability negotiation, because there is one backend and inventing the second one's requirements in
advance is how the first one's requirements get it wrong.

`TmuxBackend` is the implementation (`internal/session/tmux.go`).

## 4. Session identity

A session is named `amx-{projectId}` and nothing else.

```go
// internal/project/model.go
func SessionNameFor(projectID string) string { return "amx-" + projectID }
```

- **Never the project name.** Renaming a project must not orphan its terminal, and two projects in
  different collections may share a name.
- **One function.** The name is spelled in exactly one place: the runtime manager calls
  `SessionNameFor`, and the HTTP layer reports the manager's answer. No handler, no template, and no
  frontend concatenates `"amx-"` with an id.

## 5. The runtime manager

`internal/session/manager.go` sits between the HTTP handlers and the backend, and owns everything
that is a policy rather than a mechanism: what a session is called, where it runs, how big it is, what
is written down, and how a project's status is derived.

```text
HTTP handlers ──▶ session.Manager ──▶ Backend (tmux)
                        │
                        ├─▶ project.Service   (resolve a project id)
                        └─▶ storage.Runtime   (metadata that must survive a restart)
```

HTTP handlers call the manager and nothing else. A handler that started deciding what a session is
named would be the second place that decision is made.

### Lifecycle

| Operation | Meaning | Session afterwards |
| --- | --- | --- |
| **Create** (implicit in Start) | New detached session, created in `project.runtimePath` | Exists |
| **Start** | Create the session if needed, record intent, begin reading output | Exists, RUNNING |
| **Stop** | Interrupt what is running in the session | **Still exists**, scrollback intact |
| **Destroy** | Remove the session and everything in it | Gone, irreversibly |

**Stop and Destroy are different things** and the API keeps them apart (`POST .../stop` versus
`DELETE .../runtime`). Stop is "end the work"; Destroy is "remove the terminal". A user who stops a
session and finds their scrollback gone has been given the wrong one of these.

`Create` runs the session in `project.runtimePath` — the path the *runtime* sees, not the host path,
not the Projects Root, not the Collection, and not the server's own working directory. This is
verified by test and by the integration run in §9.

### States

`STOPPED`, `STARTING`, `RUNNING`, `STOPPING`, `ERROR`, `ORPHAN`.

There is no `WAITING`, no `COMPLETED`, and no permission state. Those describe the *program* running
inside the terminal, not the terminal, and they belong to the Claude Hooks phase. Only settled states
(`STOPPED`, `RUNNING`, `ERROR`) are ever written to the database: a transition recorded on disk is a
claim about a process that has already moved on.

`ORPHAN` means a session exists that no registered project claims. AgentMux reports it and does not
touch it.

## 6. Persistence

**The server is not the runtime.** Killing the server, closing the browser, or losing the network does
not stop anything: the tmux server owns the PTYs, and it is a separate process that AgentMux merely
talks to. This is the entire reason tmux was chosen.

What that means in practice:

1. `POST /api/projects/{id}/runtime/start` creates the session.
2. Something long-running is launched in it.
3. The AgentMux server is stopped.
4. The work continues.
5. The server is started again.
6. Reconciliation finds the session (Case A) and reports the project as `RUNNING`.
7. Input can be sent again and output read again.

All seven steps are verified in §9.

### Reconciliation on startup

| Case | Database | tmux | Result |
| --- | --- | --- | --- |
| A | project + record | session exists | `RUNNING`, session rediscovered |
| B | project + record | no session | `STOPPED`. **Not started automatically.** |
| C | no project | `amx-*` session exists | recorded as an **orphan runtime**, logged as a warning, **left running** |

Case B does not auto-start: a server restart is not a request to run the user's programs. Case C does
not kill: a session whose project record is missing may be the only copy of work in progress, and
destroying it on the strength of a missing row is a guess AgentMux refuses to make.

### The `project_runtime` table

Migration `migrations/0002_project_runtime.sql`. It stores only what cannot be answered after a
restart:

| Column | Why |
| --- | --- |
| `project_id` | Primary key |
| `backend` | Which backend owns the session, so a future release with two can tell without guessing from the name |
| `session_name` | Derivable from the id — stored so a change to the naming rule cannot make an existing session unreachable |
| `state` | The user's **intent**, checked against reality at startup. A stale value is corrected, not believed |
| `canonical_cols`, `canonical_rows` | The size the project should come back at. A stopped session cannot answer this, and it is a decision the user made, not something recomputable |
| `created_at`, `updated_at`, `last_seen_at` | `last_seen_at` is when the server last confirmed the session existed; null means "never confirmed", which is worth knowing when reading a record that claims to be running |

Deliberately **not** stored:

- **Terminal output, at any granularity.** Output belongs to the tmux session, which holds it in its
  own scrollback. A copy here would be unbounded and would still be a worse terminal than the one it
  copied.
- **Which client is attached, which controller holds the lease, which WebSocket is open, where a
  browser has scrolled.** Every one of these is a property of a live connection; a stored copy
  outlives the connection that made it true and is then read as fact.
- **Liveness as a boolean.** There is no `is_running` column. A boolean written once and read later is
  a claim about a process that is no longer running; liveness is asked of the runtime.

The canonical size *is* stored, because a session that is stopped has no other way to remember the
size its user chose.

## 7. Sequence numbers

Each runtime numbers its output chunks with a monotonically increasing `seq`, and each chunk is
`{projectId, sequence, bytes, timestamp}`.

There is no WebSocket protocol yet (Phase 4). The sequence exists now because it is the thing that
makes a future reconnect possible at all, and retrofitting it onto a live stream is much harder than
starting with it:

- A client that reconnects says "I have up to 7" and is given 8 onward.
- A second client attaching does not disturb the first.
- History is bounded (a fixed number of chunks and bytes per runtime) and the bound is a memory
  decision, not a protocol one.

Every live subscriber receives the same bytes; a subscriber that stops reading applies backpressure to
tmux rather than having its output silently dropped.

## 8. Diagnostics

The debug endpoints exist so the runtime can be exercised end to end before there is a Web Terminal to
exercise it with. **They are off unless `-debug-api` (or `AGENTMUX_DEBUG_API`) turns them on**, and
they are expected to be deleted when Phase 4 provides a real terminal. They are not product API.

| Endpoint | Purpose |
| --- | --- |
| `GET /api/debug/runtimes` | List the backend's sessions and orphans |
| `POST /api/debug/reconcile` | Run reconciliation and return the report |
| `GET /api/debug/projects/{id}/runtime/output?since=N&snapshot=1` | Buffered chunks after `N`, optional pane snapshot |
| `POST /api/debug/projects/{id}/runtime/input` | Send `text` (as UTF-8) **or** `bytes` (base64), optionally `enter` |
| `POST /api/debug/projects/{id}/runtime/resize` | Resize a runtime |

`input` refuses a body carrying both `text` and `bytes`: they are two fields, not a sequence, and a
diagnostic endpoint that silently reordered a test's input would send the test's author looking in the
wrong place.

Chunk data and snapshots are base64 on the wire (`[]byte` in Go), so the terminal's bytes round-trip
exactly rather than through a lossy string.

## 9. Verified behaviour

The following were verified against tmux 3.4 in WSL, not inferred.

**Real WSL run** — server started inside the distribution, temporary Projects Root, test project at
`/tmp/agentmux-phase2-test`:

1. Server runs in WSL; `environment: "wsl"`, `runtimeAvailable: true`.
2. tmux 3.4 detected, `probedIn: "wsl"`.
3. Runtime started for the test project.
4. `tmux -L agentmux list-sessions` shows `amx-...`.
5. Input sent (`echo`, Chinese text, a control character).
6. Raw output received, byte-exact.
7. ANSI colour sequence preserved in the output.
8. Unicode and Chinese preserved, not replaced with `U+FFFD`.
9. Resize 80×24 → 100×30, confirmed **inside** the process with `tput cols` / `tput lines`.
10. Server stopped.
11. The task in the session continues running.
12. Server restarted.
13. Runtime rediscovered as `RUNNING` (Case A reconciliation).
14. Input sent again, output read again.
15. `stop` interrupts the work, session alive.
16. `DELETE runtime` removes the session.

Working directory is `project.runtimePath`, verified inside the session — not the Projects Root, not
the Collection, not the server's cwd.

Automated tests cover the same ground without a real tmux where they can: session naming, create,
duplicate create, exists, list, stop, destroy; input (ASCII, UTF-8, control bytes, escape sequences,
non-UTF-8 bytes); resize; output fidelity (ANSI, Unicode, progressive same-line output); three
sessions with no cross-talk; and the cwd check.

## 10. Limitations

Stated rather than hidden.

- **A tmux server can die when a control client detaches.** Measured, not inferred, and not fully
  explained. On tmux 3.4 (Ubuntu 24.04 under WSL2), terminating a `tmux -C attach-session` client
  ends the whole tmux server roughly 0.3–0.7% of the time, destroying every session on that socket —
  including sessions no client ever touched. What was ruled out matters as much as what was seen. It
  is not AgentMux's fallback kill: 300 detaches, slowest 2.2 ms against a 500 ms grace, so that kill
  never runs, and a graceful-detach variant measured identically. It is not `destroy-unattached` or
  `exit-unattached`, both off, and tmux's own `server_loop` says a server holding a session must not
  exit. It is not a pane exiting — `remain-on-exit` does not save it. It does not happen at all
  without a control client (0 losses in 3600 rounds of otherwise identical client churn), or with an
  explicit long-running pane command (0 in 600), or from a shell script driving the same tmux
  sequence (0 in 300). The server's own debug log, made to log by `SIGUSR2`, stops at `lost client`
  with no signal handler, no session destroy, and no exit record.

  Nothing outside tmux can prevent this, so the requirement becomes that AgentMux does not lie about
  it. The runtime stops reporting `Running` once the session behind it is gone — pinned by
  `TestManagerStopsClaimingRunningWhenTheServerGoesAway` — and the next startup reconciles it as
  Case B, reporting `Stopped` and starting nothing. The consequence for a user is a terminal that
  disappears; the consequence for the design is that persistence is a property of the tmux server,
  and the tmux server is the one component AgentMux does not own.
- **PTY line discipline acts on some control bytes.** `send-keys -H` round-trips bytes exactly to the
  pane, but the shell's terminal driver interprets a small set of them — most visibly `0x7f` (ERASE),
  which deletes a character instead of arriving as data. This is the terminal behaving correctly, not
  a bug, and it is recorded as a pinned test rather than fought. A future input path that needs to
  bypass the line discipline will need a different mechanism than keystrokes.
- **No flow control to the client.** A subscriber that stops reading applies backpressure to tmux
  rather than dropping output. That is the right default (terminal output is not droppable) and it
  means a stalled client can eventually stall the session it is watching.
- **History is in memory and bounded.** A server restart loses the buffered chunks; the pane's own
  scrollback in tmux is unaffected, and that is what a client gets on reconnect.
- **One backend.** `TmuxBackend` is the only implementation. The interface is shaped so a second one
  fits, but nothing has been written against a hypothetical second one.
- **The debug API is unauthenticated**, like the rest of the Phase 2 API, and it exposes raw terminal
  input and output. It is off by default and must not be turned on outside a local test.
- **`-debug-api` exists only until Phase 4.** If it is still here after the Web Terminal lands, that
  is a bug.

## 11. What Phase 2 does not do

Explicitly out of scope, and not stubbed or faked anywhere in the code:

Claude Code launch · Claude Session Resume · xterm.js or any real terminal view · WebSocket terminal
streaming · Prompt Bar · Controller/Viewer roles · Claude Hooks · Waiting/Completed states · CC Switch
· provider switching · push notifications · Codex, Gemini, OpenCode.

`GET /api/server` reports `features.terminal` as true only when the build has the runtime, the server
is on the right side of the WSL boundary, **and** tmux is installed where sessions run. The UI offers
Start/Stop runtime and says plainly that the terminal view arrives in Phase 4; it does not draw an
empty rectangle.
