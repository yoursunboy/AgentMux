# AgentMux Runtime

This document describes the persistent session runtime: where the server runs, how tmux is driven,
what the `SessionBackend` boundary is, what survives a restart, and what the runtime deliberately does
not do yet.

It is written for someone who has to change this code, or to explain to a user why their terminal
behaved the way it did.

Phase 2 built this runtime on **one shared tmux server**. Phase 2.5 replaced that with **one tmux
server per project** — see §12 — and added the Control Monitor described in §13. Everything in this
document describes the per-project arrangement unless it says otherwise.

## 1. The run model

**In Windows + WSL mode, the AgentMux Server process itself runs inside WSL.**

```text
Windows host                          WSL distribution (Linux)
────────────                          ────────────────────────
project files on D:\            ←→    /mnt/d/... (drvfs)
VS Code GUI                     
the user's browser              →     AgentMux Server (Go, linux/amd64)
                                      ├─ tmux server on <dataDir>/tmux/p_a1b2….sock
                                      │    └─ pty for project A's session
                                      ├─ tmux server on <dataDir>/tmux/p_c3d4….sock
                                      │    └─ pty for project B's session
                                      └─ one Control Monitor per running runtime
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

### One project, one socket, one server

**Every project's runtime is its own tmux server, addressed by its own socket.**

```text
<dataDir>/tmux/<projectId>.sock      ← the socket
<dataDir>/tmux/                      ← mode 0700, owner-only
```

tmux identifies a server by its socket path, so two paths are two servers: separate processes,
separate session trees, separate lifetimes. `tmux -S <path>` is that statement, and every command
AgentMux issues carries it.

Phase 2 used one shared socket named `agentmux`. That made the fault domain the whole installation:
one server dying took every project's terminal with it, and AgentMux could not tell "this project's
work ended" from "the runtime died". The full reasoning, the measured isolation properties, and the
stale-socket rules are in **§12**.

Three properties of the path are load-bearing:

- **It is derived from the project id, never from a display name.** `SocketDir.Path` returns `""` for
  anything that is not an id AgentMux issues, and callers must treat that as "unavailable" rather than
  substituting a name. A path derived from a mutable string would move a project's runtime whenever
  the string changed, and a crafted name could address a path outside the socket directory.
- **It is recomputable, so it is not stored.** `dataDir` + `projectId` is the whole function. See the
  note on migration 0002 in §6.
- **The socket directory is `0700`.** A tmux socket is a control channel: anything that can open it
  can type into a project's terminal, read everything it prints, and kill it. The mode is set
  explicitly rather than left to the umask, and an existing directory is tightened on startup.

`requireSocket()` in `internal/session/tmux.go` refuses any operation with an empty socket path. An
empty path does not mean "no socket" to tmux — it means *the user's default one*, where their own
sessions live. One refusal at the single choke point every server-addressing command passes through is
what makes "AgentMux never touches your tmux" a property of the code rather than a review habit.

`-f /dev/null` is still passed: the user's `~/.tmux.conf` must not change how AgentMux's sessions
behave, and a bug report about those sessions must be reproducible.

Sessions are named `amx-{projectId}` — see §4.

### Bootstrap

tmux has a wrinkle that cost an afternoon and is worth recording: a session has to be created by one
client invocation, because there is no state to configure before a server exists.

- `set-option -g ...` on its own cannot start a server; it fails with *"no server running"*.
- `start-server` alone starts a server that exits immediately, because `exit-empty` is on.
- The working pattern is a single invocation:

  ```text
  tmux -S <socket> -f /dev/null start-server \; set-option -g ... \; new-session -d ...
  ```

  The `\;`-joined command list runs inside one client, which is what keeps the server alive between
  the first command and the last.

Because `exit-empty` is on, the *server* outlives its last session only by the time it takes to exit.
`kill-server` and `kill-session` both leave the socket **file** behind, so a stale socket is what every
finished runtime leaves rather than a fault (§12).

### `attach-session` starts a server

Measured on tmux 3.4 and 3.7c, and the reason a Control Monitor checks for its session *before*
attaching:

```text
tmux -S <path> -C attach-session -t <name>     # no server on <path>
  → starts a server, prints "no sessions", exits, leaves the socket file behind
```

No other command measured does this. `has-session`, `list-sessions`, `list-panes -a`, `kill-server`
and `show-options -g` against a nonexistent socket report the absence and exit, creating nothing.

Two consequences, both observed rather than reasoned about. A reader of a terminal must not be able to
bring a server into existence — a monitor whose session had ended was conjuring a server on the
project's socket in order to discover there was nothing to read. And a probe racing that newborn
server is told *"server exited unexpectedly"*, a state that is neither live nor provably stale, so
nothing cleans it and a reconciliation reports it as unreadable. `tmuxSubscription.pump()` therefore
asks `sessionExists` first, and `SocketDir.Probe` asks a second time when it gets that wording.

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

#### A control client is a stream, not a buffer

**Output produced between a stream ending and the next client attaching is not recoverable.** tmux
sends a newly attached control client nothing that happened before it attached. This is a property of
the mechanism, not a defect in the parser, and it has one consequence worth stating plainly: a
Control Monitor that reconnects has a hole in its output for however long it was disconnected.

AgentMux does not paper over this. A reconnect is counted (`MonitorStats.streamReconnects`), and the
gap is the honest cost of a monitor that outlives its own client. §13 records the remedy the protocol
does allow — a `capture-pane` snapshot on reconnect — and why Phase 2.5 did not take it.

The practical effect on a client is that output is complete while the monitor is attached and has a
gap across a reconnect. It is never *wrong*: nothing is reordered, invented, or attributed to the
wrong project.

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

Startup scans two sources and matches them against each other: the `project_runtime` table, and the
socket directory. For each AgentMux socket it asks first whether the file exists, then whether a
server answers on it, then what sessions that server has; the project id is read from the file name,
which is why the name is the id and nothing else.

| Case | Database | Socket / server | Result |
| --- | --- | --- | --- |
| A | project + record | server answers, session exists | `RUNNING`, session rediscovered, Control Monitor re-established |
| B | project + record | no server, or server with no session | `STOPPED` or `ERROR` per the state model. **Never reported `RUNNING`.** Not started automatically. |
| C | no project | server answers, `amx-*` session exists | recorded as an **orphan runtime**, logged as a warning, **left running** |
| D | either | socket file present, no server behind it | **stale socket**: confirmed, then removed, and logged |
| — | either | socket that cannot be classified | reported, **nothing deleted** |

Case B is the point of the whole arrangement. A runtime that has lost its server must stop claiming to
be `RUNNING`; a status that outlives the thing it describes is worse than no status. This is pinned by
`TestManagerStopsClaimingRunningWhenTheServerGoesAway`, which survived the Phase 2.5 refactor
unchanged.

Case B does not auto-start: a server restart is not a request to run the user's programs. Case C does
not kill: a session whose project record is missing may be the only copy of work in progress, and
destroying it on the strength of a missing row is a guess AgentMux refuses to make.

Case D is the ordinary state of a project nobody is running, not a fault — `kill-server` leaves the
socket file behind on every version measured. It still has to be cleaned up, because a directory that
only grows makes a live socket indistinguishable from a dead one at a glance. The confirmation is
deliberately repeated: **probe, wait, probe again**, and only then unlink — and only if the path is
still a socket file. A single failed connection is not evidence; a server that is starting up, a
machine under load, or a socket replaced between two calls all look like one. See `SocketDir.Reclaim`
and §12.

`Reconcile` returns a `ReconcileReport` counting `Running`, `Stopped`, `Orphans`, `StaleSockets` and
`UnreadableSockets` — a value, not an error, so a partial answer is still an answer.

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
- **An absolute socket path.** `dataDir` + `projectId` computes it exactly, on every start, with no
  state to keep in sync. A stored copy would be a second source of truth that a moved data directory
  or a changed `-tmux-socket-dir` would silently contradict — and the failure mode would be a runtime
  addressed at a socket nobody is listening on. Migration `0002` therefore stores no socket column.

### Configuration, and the `-tmux-socket` that used to matter

| Setting | Meaning |
| --- | --- |
| `tmuxBinary` (`-tmux-binary`) | The tmux executable **every** path uses — version detection, dependency detection, create, list, attach, control mode, send, resize, stop, destroy, reconciliation, diagnostics, stress and integration tests. Default `tmux` on `PATH`; may be `/usr/bin/tmux` or a user-local path. |
| `tmuxSocketDir` (`-tmux-socket-dir`) | The directory holding one socket per project. Default `<dataDir>/tmux`. |

The single-binary rule is enforced by construction rather than by review: the resolved path is carried
in one `tmuxInstall` value that every command goes through, so there is no remaining `exec.Command("tmux", …)`
that could pick up a different tmux from `PATH`. `TestTheConfiguredBinaryStopsAServer` proves it by
configuring a *recording fake* tmux as the binary and asserting the configured one is what ran.

`-tmux-socket` named the one shared socket. It is still accepted and still means what it always meant,
but it no longer has any effect, and the server says so at startup with a deprecation warning rather
than silently reinterpreting it. It is kept rather than removed so an existing configuration file
does not fail to parse; it is not kept as a *design* — nothing reads it to decide where a runtime
lives, because "shared socket" is the arrangement Phase 2.5 removed.

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

### What `GET /api/server` reports about tmux

```json
"tmux": {
  "available": true,
  "version": "3.4",
  "binary": "/usr/bin/tmux",
  "socketDir": "/home/user/.local/share/agentmux/tmux",
  "minimumVersion": "3.0"
}
```

`binary` is reported rather than assumed because on a machine with two tmux installations `"3.4"` is
not a measurement and `"3.4 at /usr/bin/tmux"` is. It is the same resolved path every operation uses.

There is deliberately **no `recommendedVersion` field yet**. The compatibility matrix (§14) measured
four environments and reproduced the Phase 2 anomaly in none of them, so there is no evidence on which
to base a recommendation, and a "recommended" that is really "the one we happened to test" would be
read as a requirement. If a recommendation is added later it must be distinguishable from
`minimumVersion`, which is a genuine refusal: below tmux 3.0 the operations this backend depends on do
not exist, and `Available` says so. Anything above that is a warning, never a hard refusal — refusing
to start on a version AgentMux has not personally tested would take the runtime away from a user whose
tmux is merely unusual.

### The debug endpoints

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

### The Phase 2.5 fault-isolation run

The same ground again, against the per-project architecture, driving a real AgentMux server inside
WSL with a throwaway Projects Root under `/tmp`. This is the run that measures the claim the phase
exists to make — *a fault in one project stops there* — rather than asserting it.

1. Three temporary projects registered (A, B, C).
2. All three started, all three `RUNNING`.
3. Three **distinct** socket paths, all present as socket files.
4. Three **distinct** tmux server processes (pids 593737 / 593761 / 593784 in the recorded run).
5. Each project running a continuing process of its own (`sleep 600` in each pane).
6. **B's tmux server killed** — `kill-server` on B's socket, and no other.
7. A's process still running.
8. C's process still running.
9. The manager reports B as `STOPPED`, and A and C still as `RUNNING`.
10. **B destroyed** via `DELETE …/runtime`; B's socket file gone.
11. A and C still running.
12. The AgentMux server itself stopped.
13. A's and C's tmux servers still up, both processes still running.
14. AgentMux server restarted.
15. A and C reconciled to `RUNNING`, `sessionAlive`, each with a Control Monitor attached.
16. Input works again — a token typed into A arrives.
17. Resize works again — A's pane measured at 100×30.

All seventeen passed. The load-bearing ones are 6–9 and 11: killing one project's server, and
destroying one project's runtime, must not be observable from the others.

`TestKillingOneProjectsServerLeavesTheOthersRunning` is the automated form of 6–9, and it does not
just check status: the survivors must still answer on their own sockets and must still produce output
that reaches the history buffer, so a runtime that survived but stopped being watched fails the test.

## 10. Limitations

Stated rather than hidden.

- **The Phase 2 anomaly, recorded as it was written at the time.** Kept verbatim, and kept *because*
  it was the thing Phase 2.5 set out to explain. The re-measurement that follows does not delete it.

  > A tmux server can die when a control client detaches. Measured, not inferred, and not fully
  > explained. On tmux 3.4 (Ubuntu 24.04 under WSL2), terminating a `tmux -C attach-session` client
  > ends the whole tmux server roughly 0.3–0.7% of the time, destroying every session on that socket —
  > including sessions no client ever touched. What was ruled out matters as much as what was seen. It
  > is not AgentMux's fallback kill: 300 detaches, slowest 2.2 ms against a 500 ms grace, so that kill
  > never runs, and a graceful-detach variant measured identically. It is not `destroy-unattached` or
  > `exit-unattached`, both off, and tmux's own `server_loop` says a server holding a session must not
  > exit. It is not a pane exiting — `remain-on-exit` does not save it. It does not happen at all
  > without a control client (0 losses in 3600 rounds of otherwise identical client churn), or with an
  > explicit long-running pane command (0 in 600), or from a shell script driving the same tmux
  > sequence (0 in 300). The server's own debug log, made to log by `SIGUSR2`, stops at `lost client`
  > with no signal handler, no session destroy, and no exit record.

  **Phase 2.5 re-measurement: 0 failures observed in 21,000 rounds.** The matrix is in §14. One harness
  was used unchanged across all four environments — WSL2 and pure Linux, tmux 3.4 and a source-built
  3.7c — plus four fault shapes at higher churn (graceful detach, hard kill, subscribe/unsubscribe,
  and control mode with a noisy pane) and two control runs with the harness's confirmation step
  removed. Every run: 0 server deaths, 0 session deaths, 0 fallback kills. Slowest detach 2.70 ms to
  37.02 ms against a 500 ms grace.

  **The earlier hypothesis is withdrawn because the rate it predicted does not survive the
  measurement.** At 0.3–0.7%, 21,000 rounds would have been expected to produce on the order of 60 to
  150 server deaths. None were observed, in any environment, on either version. The *observation*
  stays exactly as recorded — something did happen in Phase 2, and this document does not pretend
  otherwise. What is withdrawn is the *explanation*: that detaching a control client kills a tmux 3.4
  server at that rate is not supported, and it should not be repeated as a property of tmux 3.4.

  What remains unknown is the original trigger. The Phase 2 stress script was deleted and could not be
  reconstructed, so the conditions that produced the anomaly cannot be deliberately reproduced, and
  the run above is a *re-measurement of the same shape*, not a replay.

  **This is not a fix and must not be reported as one.** `0 failures observed in 21,000 rounds` is the
  whole claim; the 95% upper bound that puts on the rate is roughly 0.014%, which is below what Phase 2
  reported and above zero. Nothing was changed in tmux, and nothing in AgentMux was changed in order
  to make this measurement come out this way.

  What the phase *did* change is the consequence. Under the Phase 2 arrangement a server death took
  every project's terminal with it; under the per-project arrangement it takes one, and the surviving
  projects keep running (§12, §9 steps 6–11). That is worth having whether or not the original anomaly
  ever returns — and the rule that follows from it is that isolation is never rolled back on the
  strength of a clean measurement. A run of zeros says the bug is rare, not that the architecture was
  unnecessary.
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

## 11. What this phase does not do

Explicitly out of scope, and not stubbed or faked anywhere in the code:

Claude Code launch · Claude Session Resume · xterm.js or any real terminal view · WebSocket terminal
streaming · Prompt Bar · Controller/Viewer roles · Claude Hooks · Waiting/Completed states · CC Switch
· provider switching · push notifications · Codex, Gemini, OpenCode.

Phase 2.5 added no runtime feature. What it changed is where a runtime lives (one tmux server per
project, §12), how one is watched (a single server-owned Control Monitor, §13), and what is known
about which tmux versions do this reliably (§14). A browser still cannot create, address, or destroy
anything in this document.

`GET /api/server` reports `features.terminal` as true only when the build has the runtime, the server
is on the right side of the WSL boundary, **and** tmux is installed where sessions run. The UI offers
Start/Stop runtime and says plainly that the terminal view arrives in Phase 4; it does not draw an
empty rectangle.

## 12. Runtime isolation

**The fault domain of a runtime is one project.** Everything in this section exists to make that true
and to keep it measurable.

| Property | Mechanism | Where it is enforced |
| --- | --- | --- |
| One server per project | `tmux -S <socketDir>/<projectId>.sock` | `SocketDir.Path`, and `requireSocket` refusing an empty path |
| The socket name is stable | Derived from `projectId`, never a display name | `project.ValidID`; `Path` returns `""` for anything else |
| The socket dir is not world-readable | mode `0700`, re-applied on every start | `NewSocketDir` |
| One project's fault stops there | the above, and nothing shared between runs | measured, not assumed — see below |

Isolation is a **property of the process tree**, so it is measured destructively. A test that starts
three projects and checks there are three sockets passes on an architecture that shares everything
until the first fault, which is exactly the architecture being replaced.

What is required to hold (`internal/session/isolation_test.go`, plus §9's seventeen-step run):

- **Independent lifetimes.** Killing project B's tmux server must leave A and C running, with their
  sessions intact, their sockets live, and their output still arriving. Requiring the survivors to
  *produce output* and not merely to report a status is the point: a runtime that survived but is no
  longer watched has failed, quietly.
- **Independent destroy.** `DELETE …/runtime` on B must remove B's session and B's socket and nothing
  else, and a subsequent reconciliation must not still be accounting for B.
- **Independent input, output and resize.** Typing into A must not appear in B or C; resizing A must
  not move B's or C's pane size. Each project's pane geometry is read back from its own server.
- **Independent of AgentMux.** The server stopping must not end any project; the server restarting
  must adopt every surviving session and re-establish a Control Monitor for each, without starting
  anything that had stopped.
- **Cross-project safety in cleanup.** Stale-socket removal is per-socket and confirmed twice. There is
  no bulk operation, no `pkill tmux`, and no path in the codebase that addresses the user's default
  socket. `KillServer` on B's socket is the widest thing a destroy can do.

`Stop` and `Destroy` remain two different operations and stay apart in the API. Stop ends the work and
keeps the session, its scrollback and its scrollback's server; Destroy removes the session, its
scrollback, and the project's runtime record. A user who stops a session and finds their scrollback
gone has been given the wrong one of these.

## 13. The Control Monitor

**One Control Monitor per running runtime. It is owned by the AgentMux Server Runtime, never by a
client.**

This is a rule about ownership, and it is the reason the phase reworked the monitor at all. In the
Phase 2 arrangement a control client was a per-viewer thing: a browser connecting meant a client, a
browser closing meant the client died, and a phone refreshing could therefore end a stream that a
desktop was relying on. A viewer must not be able to affect a runtime. There is deliberately **no path
that creates one Control Monitor per viewer**, and the browser does not appear anywhere in this
section.

```text
AgentMux Server
  └─ Manager
       └─ one tmuxSubscription per running runtime
            └─ pump(): tmux -C attach-session, re-attached as needed
                 └─ Control Monitor ──▶ chunk buffer ──▶ subscribers (0..n, none of them owners)
```

| Property | Why |
| --- | --- |
| Exactly one per running runtime | `MonitorStats().Active`; asserted as `== 1` in the reconnect stress |
| Owned by the server, not a client | iPad connecting/disconnecting, a PC connecting, a browser sleeping and waking do not appear in this code path at all |
| Reconnecting is invisible above it | A stream ending is absorbed by `pump()`; the subscription outlives its streams |
| Reconnect never destroys the runtime | **The Control Monitor is not the runtime owner — the tmux server is.** A monitor cannot end the session it is watching |
| Server exit closes the monitor, not the runtime | `Close` detaches; the tmux server and everything in it keep running (§9 steps 12–13) |
| The monitor is the only live-output path | Control mode, not `capture-pane` polling |

### Measured under repeated drops

`TestControlMonitorReconnectsUnderRepeatedDrops` kills the control client of a running project while
the server and session stay up — the fault a network interruption, a suspended laptop, or a `kill` on
a client produces. The number of rounds is raised with `AGENTMUX_STRESS_MONITOR_ROUNDS`; the default
suite runs twelve, and the recorded run below is 500 per tmux version, on both binaries:

```text
Environment:      WSL2 / Ubuntu 24.04 / kernel 5.15.167.4-microsoft-standard-WSL2
tmux 3.4        (/usr/bin/tmux, sha256 034b15c64035f783)   — 500 rounds
tmux 3.7c       (source-built,  sha256 970f7f67970ab60f)   — 500 rounds

                       3.4        3.7c
rounds:                500        500
control clients dropped: 500      500
stream reconnects:     500        500   ← every drop was re-established
subscription reconnects: 0         0    ← the manager never saw a subscription end
monitor failures:        0         0
tmux server deaths:      0         0
runtime deaths:          0         0
monitors attached:       1         1
slowest reconnect:    138ms     135ms
```

**1000 reconnect rounds, 0 monitor failures, 0 tmux server deaths, 0 runtime deaths.**

Two of those numbers are the ones that matter. `tmux server deaths: 0` and `runtime deaths: 0` say a
monitor losing its client is not a reason for a runtime to end. `subscription reconnects: 0` says the
reconnection happened *below* the manager: the manager was never told, which is what makes a dropped
client invisible to everything above — and is also why the count had to be taken inside `pump()`
(`MonitorStats.streamReconnects`) rather than at the manager, where a monitor being disconnected from
constantly would look perfectly healthy.

The reconnect cost — 135–138 ms at the slowest, against a `waitForNewControlClient` deadline of 8 s —
is the window in which the project is not being observed. It is bounded and small, and it is the
number Phase 4 needs when it decides how much snapshot to take on reconnect. The stress deliberately
does **not** claim the output produced during that window: a control client is a stream and not a
buffer (§2), so the test waits for the replacement client to attach before it types anything. Typing
earlier would have measured the gap rather than the reconnect, and would have passed or failed for a
reason that has nothing to do with the monitor.

### What a reconnect cannot do

Output produced while there was no client attached is gone; see *A control client is a stream, not a
buffer* in §2. The protocol permits exactly one remedy — a `capture-pane` snapshot taken when the
stream is re-established, so the client sees the current screen even though it missed the bytes that
drew it. **Phase 2.5 deliberately did not take it.** A snapshot is `capture-pane`, which is the polling
mechanism this design replaced, and adding one would have meant a second, differently-shaped source of
terminal bytes before anything consumed the first. It remains the documented remedy and it is the
first thing Phase 4 should add, where a client that reconnects has a real reason to want it.

### The command path

Live output travels on one long-lived `tmux -C` client per runtime. **Commands do not go over that
channel**: create, `send-keys`, `resize` and `kill` are one-off `tmux` invocations with an exit code
and a stderr, so a failed resize is reported to the request that asked for it instead of arriving
asynchronously on a stream that also carries a terminal's bytes.

The rule that keeps this from becoming a race: **a one-off command and the control stream must not
address the same thing at the same time in a way whose order matters.** In practice they do not,
because they touch different objects — the control client only reads, and the one-off commands write
metadata (geometry, session existence, pane kill) that the reader does not interpret. Where they could
collide, the manager serializes them behind one mutex per runtime rather than relying on the ordering
of two independent processes.

## 14. tmux compatibility matrix

One harness, unchanged, across four environments — `TestControlModeAttachDetachStress`, driven by
`AGENTMUX_STRESS_ROUNDS` / `AGENTMUX_STRESS_TMUX`. The point of the matrix is that it is the *same*
test: an anomaly that appears in one column and not another is then a difference between the columns
and not between two test programs.

| Env | Platform | tmux | Binary sha256 (first 16) | Rounds | Server deaths | Session deaths | Failure rate |
| --- | --- | --- | --- | ---: | ---: | ---: | ---: |
| A | WSL2 | 3.4-1ubuntu0.1 (distro, `/usr/bin`) — the Phase 2 configuration | `034b15c64035f783` | 5000 | 0 | 0 | 0.000% |
| A churn | WSL2 | 3.4, subscribe/unsubscribe at higher churn | `034b15c64035f783` | 2000 | 0 | 0 | 0.000% |
| A kill | WSL2 | 3.4, hard client kill | `034b15c64035f783` | 2000 | 0 | 0 | 0.000% |
| A subscribe | WSL2 | 3.4, subscription churn | `034b15c64035f783` | 2000 | 0 | 0 | 0.000% |
| A noisy | WSL2 | 3.4, control mode over a pane that never stops writing | `034b15c64035f783` | 2000 | 0 | 0 | 0.000% |
| B | WSL2 | 3.7c, source-built — control mode | `970f7f67970ab60f` | 2000 | 0 | 0 | 0.000% |
| B churn | WSL2 | 3.7c, subscribe/unsubscribe at higher churn | `970f7f67970ab60f` | 2000 | 0 | 0 | 0.000% |
| C | Pure Linux | 3.4-1ubuntu0.1 (distro, `/usr/bin`) | `034b15c64035f783` | 2000 | 0 | 0 | 0.000% |
| D | Pure Linux | 3.7c, source-built (identical binary to B) | `970f7f67970ab60f` | 2000 | 0 | 0 | 0.000% |

**Total: 17,000 rounds re-measured in WSL2 + 4,000 as recorded on pure Linux = 21,000 rounds, 0
server deaths, 0 session deaths, 0 fallback kills.**

Slowest detaches, from the same runs: A 18.29 ms, A churn 18.20 ms, A kill 18.67 ms, A subscribe
17.11 ms, A noisy 15.62 ms, B 14.65 ms, B churn 10.52 ms; C 4.59 ms and D 2.70 ms as recorded. Every
one is far below the 500 ms grace, so the harness's own fallback kill never ran.

Two things about this table are stated rather than glossed. **The WSL2 rows were re-measured in full**
for this document, which is why they are itemised one shape per row; the numbers published here are
the numbers those runs printed, not a reconstruction. **The pure-Linux rows are as recorded earlier in
the phase and were not re-run for this document** — the test host needs interactive authentication
that this environment cannot supply, which is the one blocker named in the Phase 2.5 report — so C and
D stand on their earlier measurement and are labelled "as recorded" wherever they appear. The earlier
exploratory WSL2 runs at other shapes (a 3000-round control run with the harness's confirmation step
removed, slowest detach 37.02 ms, and a 3000-round 3.7c run at 20.57 ms) also reported zero deaths;
they are carried in the historical note in §10 rather than restated as rows here, because their
per-row detail was not re-measured alongside the table.

The 2×2 is deliberate. A and C are the *same binary* on two platforms, so A vs C isolates the
platform; B and D are the same binary on two platforms. A vs B and C vs D isolate the version. The
fault-shape rows exist because the Phase 2 anomaly was a *server disappearing*, and a run that only
ever attaches and detaches cleanly would not exercise the paths most likely to produce one — a hard
kill, a subscription churn, and a control client watching a pane that never stops writing all reach
different teardown code.

Two environment facts, recorded because they bound how far the result generalises: the "latest stable"
column is **vanilla tmux 3.7c, source-built into a private user prefix** (`~/.local/agentmux-tools/`) —
the distro's own tmux was never replaced or uninstalled, and the two installations coexist, selected
per run by `AGENTMUX_STRESS_TMUX`. And the pure-Linux columns ran on the internal test host only, in
an isolated socket directory under `/tmp`, never on its default tmux socket and never touching the
machine's existing sessions.

**All four environments are §二十二 Case 4: nothing reproduced.** Per §21 of this document's own rules
and §10's historical note, that is reported as `0 failures observed in 21,000 rounds` and nothing
stronger — not "fixed", not "stable".
