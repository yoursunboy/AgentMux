# AgentMux security

What AgentMux's security actually is, what it deliberately is not, and what a
deployment has to decide for itself. Read this before the server's port is
reachable from anywhere but this machine.

The rest of the documentation describes what AgentMux does. This file describes
what protects it, and it starts with the one thing that does not.

## 1. What AgentMux is, from a security standpoint

**AgentMux has no authentication.** There is no password, no token, no account,
no session, no cookie, no API key, and no setting anywhere that turns one on.
Who can reach the port is the whole of its access control.

That is not a gap in an otherwise finished feature. It is a design decision for
a single-user, self-hosted tool, and it is the premise every other statement in
this file is derived from. The project's own README lists Authentication in the
column of things that do not exist yet, and points here.

What it means concretely: anyone who can open the port can call the same API the
browser calls. They can create and register projects, start a runtime
(`POST /api/projects/{id}/runtime/start`), start the Claude Code CLI inside it
(`POST /api/projects/{id}/runtime/agent/start`), and open `GET /api/ws` and type
into the terminal. A terminal on the service account is a shell on the machine
under that account's rights — see §2. The API also lists and reads every project
under the configured Projects Roots, and every write a project's runtime makes
is a write the same account can make.

Where that leaves a deployment:

- **Loopback by default.** The shipped configuration, and the file `install.sh`
  writes, bind `127.0.0.1`. Nothing outside the machine can reach the port.
- **A private network, a VPN, or an authenticating reverse proxy** if it must be
  reachable further than that. The proxy is where the authentication AgentMux
  does not have belongs. Nothing on the AgentMux side needs to change for this:
  it never inspects a credential, so it cannot disagree with whatever the proxy
  decided.
- **Never the open internet.** A machine whose port is reachable from the
  internet is a machine whose shell is reachable from the internet, with the
  service account's rights, with no password in between.

**The control lease is not authentication.** When several browsers watch the same
project, exactly one holds a lease and may type; the others are viewers. It
decides which of the clients that are *already allowed in* is typing, and it is
about a terminal being read by two people at once, not about who may reach the
server. Any client that is connected may ask for a free project's lease. See
`docs/MULTI_DEVICE.md`.

The parts of this that follow from the missing authentication are stated where
they belong: the account in §2, the bind address in §3, what the browser-origin
check does and does not stop in §4, and the checklist in §13.

## 2. The service account

`deploy/linux/install.sh` creates one account, `agentmux`, with

```sh
useradd --system --create-home --shell /bin/bash agentmux
```

`--system` makes it an unprivileged system account. It is created with no sudo
rights and no supplementary groups, so there is no group membership that widens
what it can reach. The login shell is deliberate rather than careless: a
terminal is what this account is for, because tmux runs a shell in every
session, and an account with `/usr/sbin/nologin` has no terminal to host.

The unit runs the server as this account (`User=agentmux`, `Group=agentmux`),
and every process a person starts in a pane is a child of it and inherits the
same rights.

What that protects:

- Nothing on the machine runs as root through AgentMux. There is no setuid
  helper, no privileged daemon, and no path from a terminal to root other than
  the ones the machine's own configuration already provides.
- A program run in a pane can do what that account can do, and no more. It
  cannot read a file the account cannot read, modify a file it cannot write, or
  signal a process it does not own.
- The unit installs the binary and the unit file as root and the account cannot
  rewrite either, so a compromised pane cannot replace the server that will be
  started at the next boot.

What it does not protect:

- **Anything the account can do, a visitor can do.** The account exists to run
  the user's own programs, so it can reach everything those programs need —
  which includes reading and writing every project under the Projects Roots
  (`install.sh` suggests the root be owned by `agentmux`), writing in its own
  home directory, running programs resolved on its `PATH`, and running `sudo` if
  the machine's sudoers grants it. The account is not granted sudo by AgentMux;
  it may still be granted it by the machine.
- **The database and data directory are the account's.** A visitor can read,
  rewrite, or delete `agentmux.db`, and can create or kill the tmux servers
  under the data directory's socket directory. Backups exist for the accidental
  version of this, not the deliberate one — `docs/BACKUP.md`.
- **Nothing about it is a credential boundary.** It has no credential of its
  own, because there is none to have — see §5. There is nothing here for a
  visitor to steal that the account could not already read.

This is what `deploy/linux/README.md` §7 means when it says two things are true
without any sandbox: the service runs as an unprivileged account with no sudo
rights of its own, and it binds loopback. Both are true. Neither is
authentication.

## 3. Binding

The listener's address is `server.host` in the configuration file, `-host` on
the command line, or `AGENTMUX_HOST` in the environment, in that order of
precedence (`CLI arguments > AGENTMUX_* environment > config file > built-in
defaults`). The built-in default is `127.0.0.1`, and it is what `install.sh`
writes into the file it generates:

```yaml
server:
  # Loopback only. AgentMux has no authentication: who can reach this port is
  # the whole of its access control. See docs/SECURITY.md §1 before changing it.
  host: 127.0.0.1
```

`127.0.0.1` means the loopback interface: a process on this machine can
connect, and nothing on any network can. For a server with no authentication
this is the only safe default, and it is the reason a supervisor, a health check
and a browser on the same machine all work with nothing configured.

`0.0.0.0` means every interface this machine has — the loopback one, the one
facing the LAN, and any other that is up, including ones that were not in mind
when it was written. Anyone who can route to any of them can open a terminal on
the service account. A bind on `0.0.0.0` is a decision about a shell, not about
a web page, and it should be made the way §1 describes: on a private network, or
behind a VPN, or behind a reverse proxy that authenticates. `config/agentmux.example.yaml`
says the same thing at the key, and the installer's file says it at the line.

Binding an address is the *only* thing that decides who can reach the port.
Nothing else in AgentMux narrows it (§1), and the browser-origin check in §4 is
not a substitute for it.

## 4. Browser-origin policy

AgentMux serves two kinds of request, and both are checked against the request's
`Origin` header by the same function — `browserOriginAllowed`, in
`internal/httpapi/ws.go`: ordinary HTTP, and the WebSocket handshake at
`GET /api/ws`. One policy, asked at both places a browser can reach this server.

| `Origin` | A read (`GET`, `HEAD`, `OPTIONS`) | Anything else |
| --- | --- | --- |
| absent | allowed | allowed |
| the `Host` this request was sent to | allowed | allowed |
| listed in `server.allowedOrigins` | allowed, with CORS headers | allowed, with CORS headers |
| anything else | answered, with no CORS headers | **`403 forbidden`**, with a line in the log |

**The second column is the one that had to be added, and how it was found is
worth recording.** Until Phase 6.5 the policy was applied to the WebSocket and
to the CORS headers, and the CORS headers protect only the reply: withholding
`Access-Control-Allow-Origin` stops another page from reading what this server
said and does nothing about the request arriving. A `POST` with a simple content
type — `text/plain` carrying a JSON body — needs no preflight, so a browser
delivers it and the server acts on it. This was measured rather than reasoned
about, against the installed service: a page on `https://evil.example`
registered a project and was answered `201 Created`. The comment above the
middleware had claimed the opposite in so many words, which is what made it
worth checking. A test now sends that request and asserts the `403`
(`TestACrossOriginWriteIsRefused`), and it fails against the old code.

Why each case is where it is:

- **Same origin is always allowed**, wherever this server happens to be
  reachable. The page came from here; a user on a tablet on the LAN opened the
  UI that this server served, and it must be able to talk back without anything
  being configured first. This is also why the check compares against the `Host`
  the request arrived on rather than against the loopback list: a browser that
  reached this server at the address of the machine has a non-loopback origin
  and is still this server's own page.
- **Configured origins are allowed** so that the frontend can be served from
  somewhere else — a Vite dev server, typically. This list is the one place an
  operator can widen the policy, so `allowedOrigins` should hold nothing that is
  not recognised; §13 has it as a checklist item.
- **No `Origin` is allowed**, and that is correct rather than a hole. Browsers
  always send `Origin` on a WebSocket handshake and on any request that is not a
  plain read, so a request without one did not come from a page: it came from a
  program on this machine or on the network, and such a program does not need a
  browser's origin to reach a local server. The cross-origin attack this check
  exists to stop requires a browser, so refusing the request would break every
  non-browser client — including this project's own tests, its recovery script
  and `curl` — without protecting anything.
- **A read from another origin is answered and is not refused.** There is no
  effect to protect, and the reply is unreadable to the caller anyway, which is
  what CORS is for. Refusing it would break a listed origin for no gain.
- **The WebSocket check is not a formality.** A WebSocket is not subject to the
  same-origin policy once the server has accepted it, so an upgrader whose
  `CheckOrigin` returned `true` would open a terminal to any page in any browser
  on the machine. That is why the upgrader is built per server with this policy
  attached, rather than left at the default most examples use.

**This protects against a malicious web page in a browser. It does not protect
against anyone who can make a network request.** A page from another site cannot
open a terminal, cannot change anything, and cannot read an API response. A
program — `curl`, a script, a browser with the check disabled, anything on the
network — is not a page, is not subject to any of this, and reaches the same
endpoints. Stopping that is the port's job: the bind address in §3, and whatever
sits in front of it. The origin check is a defense against one specific
browser-shaped attack, and reading it as authentication would be reading it as
something it is not.

The policy is a property of the server process, read from the configuration at
startup; changing `allowedOrigins` requires a restart.

## 5. What AgentMux stores

The database is SQLite, at `<data-dir>/agentmux.db`. Its schema is four tables
(`migrations/`, `internal/storage/`):

| Table | What it holds |
| --- | --- |
| `projects` | One row per registered project: its id, display name, host path, runtime path, collection path, pinned workspace slot, archived flag, and timestamps. |
| `project_runtime` | One row per runtime: which backend owns it, the session name, the last known state, the canonical terminal size, and when the server last confirmed it existed. |
| `settings` | A key/value store for installation-level settings. In this build it holds the installation identifier, `server.installId`. |
| `schema_migrations` | Which migrations have run. |

It holds **no terminal output**. That is a rule in the schema's own comments and
in `internal/storage`: output belongs to the tmux session, which keeps it in its
own scrollback, and a row per chunk of terminal output would be an unbounded,
write-amplifying copy of something tmux already has. It holds **no prompts and no
conversation content** — nothing in the schema has a column for them. It records
what must survive a restart and nothing about what was on the screen.

It holds **no credential**, because AgentMux has none. There is no key, token or
password in the configuration file either: `internal/config` states that no
secret belongs there, and `config/agentmux.example.yaml` has a short section
saying there are none and nowhere to put one. The Claude Code CLI keeps its own
credentials in its own files, outside AgentMux's reach, and
`internal/claude` is written so that it never reads, prints, or returns one: it
does not open Claude's configuration, does not read an API key from the
environment, does not touch the credential store, and reports nothing about
whether the installation is signed in. Whether the CLI is authenticated is
decided when the program starts and is said on its own terminal. A launcher that
probed for a token would be a second place for a secret to exist, and the first
place for one to leak into a log line or an API response.

The installation identifier is worth naming because it is the one stored value
with a privacy flavour. It is generated once, kept in `settings`, and
deliberately **not** published by any endpoint: an identifier that is stable
across every request from a machine is a tracking token whether or not it is
called a credential.

### 5.1 Nothing in the repository either

The rule is kept outside the schema as well. No credential is stored anywhere in
this repository: not in source, not in `config.json`, not in `.env`, not in a
test fixture, and not in a shell or PowerShell script. Where a connection needs
one, it is entered interactively at the moment it is needed and is not written
down afterwards. The local note that names the runtime test machine
(`docs/serverinfo.md`) gives the host, the account and the authentication method,
and says that authentication is interactive.

That file is in `.gitignore` because it names a machine, not because it holds a
credential. The distinction matters in both directions: ignoring a file is not
what makes it safe, and a file worth ignoring is still governed by the rule above.

**This section used to end there, with the stronger claim that the note file
"holds none". That claim was checked during Phase 7.5 and was false**, so it is
corrected here rather than left standing. The file's table held a plaintext
password for one of its rows — three lines above its own sentence saying no
password is recorded anywhere — and four tracked documents repeated the
"no password" claim on the strength of it.

What was and was not exposed:

- **Not committed.** `git check-ignore -v docs/serverinfo.md` reports
  `.gitignore:34`, and `git ls-files` does not list it, so the password never
  entered the repository's history. §9's check passes on the strict question it
  asks.
- **Still exposed.** A password in a working-tree file is a password in an
  editor's buffer, a filesystem that gets backed up, and a file that a future
  `git add -f` or a `.gitignore` edit would commit. "Not in git" is not the same
  as "not leaked", and the gap between them is where this was sitting.

The two things that follow from it are an operator's, not this document's: the
password must be **rotated**, and the row must be **deleted** from the note file
— a file that names the host and the account and says authentication is
interactive needs no secret to be useful, which is the whole design above.

The claim above is now about what is *tracked*, which is checkable, rather than
about what is *on disk*, which this document cannot see. The check is one command
over the tracked tree:

```bash
git grep -nEi '(password|passwd|secret|api[_-]?key|token)[[:space:]]*[:=][[:space:]]*[^[:space:]]'
```

Anything it prints in a tracked file is a finding. Note what it cannot see: an
untracked file, which is exactly how the one above was missed, so it is run
alongside `git status --ignored` rather than instead of it. Phase 7.5's report
records both, with their output.

## 6. What it never logs

Every log record carries four things and no more: a **timestamp**, a **level**, a
**component** (the subsystem the record came from), and an **event** — what
happened, in the present tense. Detail travels as structured attributes, and the
attributes are identifiers rather than names, because a project may be renamed
and a line quoting the old name would describe a project that no longer exists
under it.

Four things never reach a record, under any level, in either format:

- **Terminal output.** A pane's bytes are the program's output, not AgentMux's
  event, and they may be anything the user's program printed. Where a record
  needs to say something about a block of terminal bytes it says how many there
  were, not what they were.
- **Anything a user typed.** Keystrokes are input to a terminal, and a log is
  the easiest place for them to end up.
- **Prompts and conversation content.**
- **Credentials**: API keys, tokens, passwords, provider configuration,
  `Authorization` headers, and request or response bodies.

The HTTP middleware records method, path, status, byte count and duration, and
nothing else — no headers and no body — and the path is recorded without its
query string, which can carry a token. The panic handler records the method, the
path and the panic value, and deliberately not the request body. Anything that
might carry a credential is passed through `Redact` before it reaches a record.

`internal/logging` holds the rule, and two tests hold AgentMux to it.
`internal/terminal/control_test.go`'s `TestNeitherTypingNorOutputIsEverLogged`
types a marker into a terminal, has the terminal print a different marker,
refuses a third marker from a viewer, and asserts that none of the three appears
anywhere in what the server wrote. Its counterpart,
`TestALogRecordCarriesOnlyTheFourFields`, walks every control-plane path — a
grant, a queue, a refusal, an accepted handover, a release, a suspension, a
resume, a lapse — and asserts that each was logged and that the only attributes
any of those records carry are the client and the project. That second test is
the one that fails when somebody adds a helpful count or a duration to a line
that was already telling the story, which is how the rule is usually broken.

Rotation is deliberately not AgentMux's: the server writes to stderr and the
supervisor owns the file. Under systemd that is the journal, which rotates on
its own. `docs/DEPLOYMENT.md` §Logs has the queries.

## 7. What it does and does not publish over HTTP

Two endpoints describe the server, and both are reachable by anyone who can
reach the port (§1). A third answers the same liveness question under the API's
own prefix, and a fourth exists only when debug is on; all four are below.

**`GET /health`** returns liveness, identity, and one machine capability:

```json
{"status":"ok","version":"0.7.5","commit":"31e0f208a3aa","runtime":"available"}
```

`status` is `ok` whenever the process can serve a request at all — the endpoint
always answers 200, because a supervisor should not have to parse a body to
learn that the process is alive, and because the only failures it could report
are failures of the machine it runs on (a missing tmux would have a restart loop
that never installs tmux). `version` and `commit` are the release and the git
revision, which is what makes "did the upgrade take effect" a question `curl`
can answer. `runtime` is readiness: `available` or `unavailable`, whether a
persistent terminal session can execute here at all.

There is no field in that response that could carry a credential, and no field
that describes the filesystem. A test asserts the exact key set
(`TestHealthSaysNothingElse`), because a field added later by somebody who had
not read this document would fail an exact-set assertion and would pass a list
of forbidden names.

**`GET /api/health`** answers the same question for clients that only speak the
API, and adds one field: `uptime`, how long the process has been running.

```json
{"status":"ok","version":"0.7.5","uptime":"3h12m4s","runtimeAvailable":true}
```

Readiness is spelled as a boolean here rather than as `/health`'s `available` /
`unavailable` string, and that is the whole of the difference. Both routes call
one expression in the server (`internal/httpapi/handlers.go`, `runtimeAvailable`),
so they cannot come to different conclusions about the same machine — a second
independent reading is how two health checks come to disagree, and an operator
then has to decide which one to believe.

`uptime` is measured from process start, not from when the server began
listening, so it is not a way to infer when the database was opened. It names no
path, no project and no client. A test asserts the exact key set for this
response too (`TestAPIHealthSaysNothingElse`).

**`GET /api/server`** is the richer report: version, phase, uptime, operating
system and architecture, where the runtime executes, the runtime backend, whether
tmux is available, the Projects Roots, discovery depth, dependency probes, and
what the server found when it resolved the tmux binary and the Claude Code CLI.
It reports the machine's *platform* — `linux`, `windows`, `wsl`, and the
architecture — rather than the machine's hostname or a username, which it never
asks the operating system for.

Four fields are withheld unless debug mode is on, and each is the path to
something the server itself keeps:

| Field | What it names |
| --- | --- |
| `dataDirectory` | Where AgentMux keeps its state |
| `databasePath` | The SQLite file |
| `configFile` | The configuration file that was read |
| `webDirectory` | The built frontend's directory |

Withheld means **absent from the JSON**, not present and empty: a client that
saw a key would otherwise have to know which empty values mean "nothing" and
which mean "not for you". Both halves are asserted by tests
(`TestServerInfoWithholdsTheServersOwnPaths`,
`TestServerInfoReportsItsPathsInDebugMode`).

**One path is deliberately not in that list, and the exception is worth naming
rather than leaving for a reader to find.** `tmux.socketDir` is published in
every mode, and it is a directory inside the data directory — so the disclosure
above is not quite as complete as the table makes it look. It stays because it
is the one path that answers a question the operator actually has: a socket left
in that directory by a project that no longer exists is the orphan case, and a
report that could not say where the sockets are could not explain one. A test
asserts it is non-empty for that reason (`tmux.socketDir is empty; a diagnostic
that cannot say where the sockets are cannot explain an orphan`). Anyone who can
read this field can already open a terminal on this machine (§1), so it tells
them nothing they could not ask for, but the honest description of the posture
is "these four, plus the socket directory", not "these four".

Debug mode is `server.debug` in the configuration file, `-debug` on the command
line, or `AGENTMUX_DEBUG` in the environment. It is off by default, and turning
it on is a deliberate act:

```bash
./agentmux-server -debug
```

**It is separate from `logging.level` on purpose.** An operator who turned up log
verbosity to chase a problem has not thereby agreed to publish a map of the disk
from an endpoint that has no authentication. Coupling the two would publish
filesystem paths as a side effect of a decision about logs.

The implication is worth stating plainly: **with debug on, `GET /api/server`
discloses the local filesystem layout, to anyone who can reach the port.**
Nothing about it is a credential, and the four paths are not needed to use the
API — they are needed to probe it. Turn it on to diagnose a deployment, turn it
off again, and do not turn it on at all on a server whose port is reachable
beyond this machine. §13 has it as a checklist item.

**Debug affects two things, and they are both reads.** This paragraph used to say
"debug affects nothing else", which was true when it was written and is not any
more, so here is the whole of what the flag does:

1. `GET /api/server` discloses the four paths in the table above.
2. `GET /api/debug/runtime` **exists**. It is registered only when debug is on;
   on every other installation the path is not routed and answers
   `404 not_found` like any unknown path.

The second is worth its own paragraph, because it is the one thing Phase 7.5
added to this document's subject. The endpoint returns four counts and a boolean:
how many runtimes this server supervises, whether the tmux binary can be run, how
many sessions are live, how many terminal sockets are open.

```json
{"runtimeCount":1,"tmuxAvailable":true,"activeSessions":1,"websocketConnections":0,"subscriptions":0}
```

**There is no field in it that could carry content.** No project name, no
identifier, no session name, no path, no prompt, no tool input, no byte of
terminal output or input history. That is not a filter applied to a richer
response — the struct has five fields and a test asserts the encoded key set
exactly (`TestTheDebugEndpointReportsCounts`), then greps the encoded bytes for
the project's own name, its id, the data directory and the strings `password`,
`token`, `secret`, `prompt`, `transcript`, `input`, `capture` and `output`
(`TestTheDebugEndpointNeverCarriesTerminalContent`). A sixth field added later
fails the build.

It is a read in the strict sense: it accepts no method but `GET`, takes no input
of any kind, and holds no state. A `POST` to it is answered by the server's
`/api/` catch-all with the same `404 not_found` any invented path gets, so a
refused write is indistinguishable from a path that was never there — and a test
asserts the runtime count is unchanged across four refused writes
(`TestTheDebugEndpointRefusesAnythingButARead`).

`/health` and `/api/health` are unchanged by the flag: a probe answered every few
seconds by a supervisor is not where a filesystem layout belongs, and a test
asserts the four fields do not appear there even with debug on. So is every
mutating endpoint — the flag skips no handler and relaxes no check, and the one
route it adds is a count.

The honest summary is that debug now widens two read-only surfaces instead of
one. Both are the kind of thing you turn on to diagnose a deployment and turn off
again, and neither is a reason to enable it on a port reachable beyond this
machine. §13 has it as a checklist item.

## 8. File permissions

`install.sh` sets these, and they are the first of two layers:

| Path | Owner | Mode | What it is |
| --- | --- | --- | --- |
| `<prefix>/agentmux-server` | root:root | 0755 | the server binary |
| `<prefix>/web/dist` | root:root | 0755 | the built frontend |
| `<config-dir>` | root:agentmux | 0750 | the configuration directory |
| `<config-dir>/agentmux.yaml` | root:agentmux | 0640 | the configuration file |
| `<data-dir>` | agentmux:agentmux | 0750 | database, config default, tmux sockets |
| `<data-dir>/agentmux.db` | agentmux:agentmux | 0640 | the SQLite database |
| `/etc/systemd/system/agentmux.service` | root:root | 0644 | the unit |

Two facts follow from that table.

**The service can read its configuration and cannot rewrite it.** The config
file is owned by root and only group-readable; the directory holding it is
root-owned and not writable by the group. So a setting cannot change itself, and
a compromised pane cannot rewrite the configuration the next start will read —

which matters more here than it usually would, because a configuration file is
where a bind address lives (§3), and an attacker who could edit it could widen
the listener without ever touching the binary.

**The database is not world-readable.** It is `0640`, owned by the service
account and its group, so another unprivileged account on the machine cannot
read it. The data directory is `0750` for the same reason.

The database is the one entry in that table that `install.sh` does not create:
the file does not exist until the server opens it. It is created by the service,
under the service account, so its ownership follows from that, and its mode
follows from the unit's

```
UMask=0027
```

which is the second layer. Files the server creates are group-readable and not
world-readable — `0640` for the database, the same shape as the configuration
file — and the umask applies to everything else the server writes as well,
including a WAL or journal file SQLite creates beside the database. The umask
does not replace the permissions `install.sh` sets on the directories; it is
what covers the files created inside them afterwards.

The binary and the unit file are root-owned and not writable by the service
account, so the account can neither replace the program that runs at the next
boot nor the unit that starts it.

## 9. Sandboxing: why the unit ships without one

`deploy/linux/agentmux.service` deliberately ships with no `ProtectSystem`, no
`ProtectHome`, no `PrivateTmp` and no `NoNewPrivileges`. The omission is a
decision, and the argument for it is the product's own purpose.

AgentMux exists to run programs a person asks for, in a terminal, as the service
account. Those programs are the user's own: a build that writes into the
Projects Root, a test that binds a port, an editor that keeps state in `$HOME`,
and routinely a `sudo` in a pane. A sandbox tight enough to be worth having is a
sandbox that breaks some of them — and it breaks them in the one place nobody
can see the error, inside a pane that stops working, where the symptom is a
program that hangs or exits without printing why.

So the decision belongs to the deployment rather than to the shipped unit. A
machine where AgentMux only ever builds code in one directory can be locked down
hard; a machine where it is a general development environment cannot. Add these
one at a time, and run a real workload through a pane after each.

| Directive | What it does | What it breaks |
| --- | --- | --- |
| `NoNewPrivileges=yes` | No setuid/setgid escalation | **`sudo` stops working in every terminal.** If anyone working in an AgentMux pane might need it, do not add this. |
| `ProtectSystem=strict` | Whole filesystem read-only | Every write the runtime performs. `ReadWritePaths=` for the Projects Root, the data directory and the home directory is the minimum, and it is easy to miss one. |
| `ProtectHome=read-only` | `$HOME` read-only | Every tool that keeps state there: `~/.cache`, `~/.npm`, `~/.gitconfig` writes, and `~/.claude`. Claude Code needs to write to its own directory. |
| `PrivateTmp=yes` | A private `/tmp` per service | Anything that expects to share `/tmp` with a process outside the service. A socket or lock file there becomes invisible to an SSH session on the same machine. |
| `ProtectKernelTunables=yes` | `/proc/sys`, `/sys` read-only | Very little. This is the one on the list with no realistic cost, and a reasonable first addition. |
| `RestrictAddressFamilies=` | Limits socket types | Binding a port from inside a pane, depending on what you allow. |

None of these is a substitute for §1. They narrow what a pane can do *after*
somebody has already reached the port and started a terminal; they do not
narrow who can.

## 10. The systemd process model, as a security property

The unit sets

```
KillMode=process
```

and it is required for runtime recovery. `process` sends the stop signal to the
server alone. The systemd default, `control-group`, sends it to every process in
the unit's cgroup — and a project's tmux server is one of those, because
AgentMux starts it as a child and tmux detaches by forking, which changes the
terminal but not the cgroup. Under the default, `systemctl restart agentmux`
would kill every tmux server, and with them every session and everything running
in one; the server would come back, reconcile honestly, and report every runtime
as stopped, having destroyed them itself.

The cost is the other side of the same property, and it is worth stating as the
trade it is: **the sessions outlive the server on purpose, and stopping the
service therefore does not end them.** A `systemctl stop agentmux` leaves every
tmux server running, with whatever is in it, on the account that owns it —
including a program somebody started and forgot about. `install.sh --uninstall`
says the same thing from the other end: it removes the unit, leaves the sockets
alone, and prints how to end each session by hand. If the point of stopping the
service is to end what is running on the machine, stopping the service is not
enough; the sessions have to be ended, or the machine rebooted.

One consequence for reading logs and process lists: after a stop, the server is
gone and the sessions are not, so nothing in AgentMux's own output describes what
is still running. `docs/DEPLOYMENT.md` and `deploy/linux/README.md` §4 cover the
three recovery cases this produces.

## 11. Dependencies

AgentMux is a Go server and a React frontend, and both carry third-party code.

- **Go modules** — `go.mod`, pinned by `go.sum`. The direct dependencies are
  `github.com/gorilla/websocket`, `gopkg.in/yaml.v3` and `modernc.org/sqlite`
  (a pure-Go SQLite, chosen so the server builds on Windows and Linux without a
  C toolchain), with their own transitive dependencies below them.
- **npm packages** — `web/package.json`, pinned by `web/package-lock.json`. The
  runtime dependencies are React and xterm.js; the build and test toolchain is a
  set of devDependencies `install.sh` installs with `npm ci`, which reads the
  lockfile rather than resolving versions again.

Check both, and keep both current:

```bash
govulncheck ./...          # Go: known vulnerabilities in the module graph
npm --prefix web audit     # npm: known vulnerabilities in the lockfile
```

`govulncheck` reports only the vulnerabilities a reachable code path could
actually hit, which is why it is the tool to run rather than a scanner that
counts advisories. `npm audit` reads the locked versions, so it reports what the
build will actually use.

The honest part: **the versions are pinned in the lockfiles, and a pin is not a
guarantee.** `go.sum` and `web/package-lock.json` record exactly what was
verified when they were written; a vulnerability disclosed afterwards changes
the risk of a pinned version without changing the lockfile. Neither tool is run
automatically by anything in this repository, and nothing between releases
watches for a new advisory. Running them is a task for whoever maintains a
deployment, and upgrading is a deliberate act with an upgrade procedure
(`docs/DEPLOYMENT.md` §Upgrade).

## 12. Reporting a vulnerability

Open a private security advisory on the repository at
<https://github.com/yoursunboy/AgentMux> (the repository's **Security** tab, then
**Report a vulnerability**). A private advisory is visible to the maintainers
and not to the public, which is the point: it gives a fix somewhere to happen
before the report becomes a description of an unpatched hole.

Include:

- what you did, in enough detail to repeat it — the request, the message, or the
  sequence;
- what happened, and what you expected instead;
- the version and the git commit, from `GET /health` — `commit` is the revision
  the binary was built from, and it is the difference between a report about
  this build and a report about a build nobody is running;
- how the server was deployed: whether it was reachable beyond loopback, whether
  anything authenticated in front of it, and whether debug mode was on;
- anything you think the fix should not do.

There is no bug bounty and no security contact address. The advisory is the
route.

## 13. A checklist before exposing the server beyond this machine

Work through this before the port is reachable from anything but the machine it
runs on. It is short because the list of things that protect AgentMux is short.

- [ ] **Confirm the bind address.** `server.host` in the configuration file, or
      `-host` / `AGENTMUX_HOST`. If it is not `127.0.0.1`, you have decided the
      port may be reached from elsewhere, and the next two items are now load
      bearing. `ss -ltnp | grep <port>` shows what the running process actually
      bound, which is the only answer that counts.
- [ ] **Put an authenticating proxy or a VPN in front of it.** AgentMux has no
      authentication (§1): whatever sits in front of the port is the whole of
      it, and the session it terminates is only as good as the proxy's own
      configuration. Check that the proxy requires a credential for `/api/` and
      for `/api/ws` alike, and that a WebSocket upgrade passes through it.
- [ ] **Confirm `allowedOrigins` holds nothing unrecognised.** Every entry is a
      site whose pages may talk to this server (§4). The default is empty, and
      an entry should be there because somebody put it there on purpose.
- [ ] **Confirm debug is off.** `server.debug`, `-debug`, `AGENTMUX_DEBUG`,
      `curl -s http://127.0.0.1:<port>/api/server | grep -i 'dataDirectory\|databasePath\|configFile\|webDirectory'`
      — output means debug is on, and the server is publishing its filesystem
      layout to anyone who can reach it (§7).
- [ ] **Confirm the data directory and database are not world-readable.**
      `ls -ld <data-dir> <data-dir>/agentmux.db` should show `0750` and `0640`,
      owned by the service account (§8). Check the socket directory inside the
      data directory too: it holds one socket per project, and a socket is a
      terminal.
- [ ] **Confirm the Projects Root holds nothing the service account should not
      read.** Anyone who can reach the port can read everything under it, and
      can write wherever the account can write (§1, §2). A root that also
      contains credentials, private keys, or another person's work is a root
      that has just been published to whoever is on the other side of the
      proxy.

Two more, worth doing whether or not the port leaves the machine:

- [ ] The service account has no sudo rights and no supplementary groups (§2),
      and nothing placed there since install (`sudo -l -U agentmux`).
- [ ] `govulncheck ./...` and `npm --prefix web audit` have been run recently
      (§11).
