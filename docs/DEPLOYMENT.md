# Deploying AgentMux

This is the reference for running AgentMux as a service: what it needs, how it is
installed, every setting it reads, what it answers, what a restart does and does
not bring back, and what to do when it will not start. `deploy/linux/README.md`
is the operator's guide that points into this file; it is shorter, procedural,
and worth reading first. `docs/SECURITY.md` is what to read before the port is
reachable from anywhere but this machine.

Every claim here is derived from the shipped files: the systemd unit, the
installer, the configuration example, and the server's own source. Where a
statement is a property of a specific program version it says which.

## 1. Platforms

AgentMux has one supported deployment shape and one supported way to reach it.

On Linux the chain is `systemd → AgentMux Server → tmux → Claude Code`. systemd
owns the process lifecycle, the server owns the HTTP listener and the project
model, tmux owns the terminal sessions, and the Claude Code CLI runs inside a
tmux session as an ordinary foreground program. Each layer is a real process on
a real filesystem, which is the point: a failure in one is visible in the
process table and in the journal rather than hidden behind an abstraction.

Ubuntu 22.04 and 24.04 are the versions this was tested on. The differences
between them surface in the unit file rather than in the server: 24.04 ships
systemd 255, 22.04 ships 249, and §4 explains which two restart directives need
the newer one and what happens without them. Debian 13 and RHEL 10 also carry
systemd 254 or newer.

There is no Docker image, and this phase does not add one. The runtime is tmux
driving real ptys in a real filesystem on the host, and a container between the
two would add a layer whose failure modes are indistinguishable from tmux's
own — which is the thing the whole design is trying to keep diagnosable. A
container is a reasonable future phase with its own answer to "which filesystem
do the projects live on".

### Windows

Native Windows tmux is not supported, and a native Windows service will not be
built. tmux has no native Windows build that AgentMux can drive, and the
programs a runtime hosts are Linux processes, so a server started on the Windows
side has no runtime at all: it starts, it manages projects, it serves the UI
and its API, and it reports `runtimeAvailable: false` through `GET /api/server`.

The supported Windows deployment therefore puts the **AgentMux Server inside the
WSL2 distribution**, with systemd enabled for that distribution. From the
distribution's point of view this is an ordinary Linux server running the
ordinary unit; the only Windows-specific steps are enabling systemd and knowing
what a distribution shutdown means (§13). `deploy/linux/install.sh` is the same
script you run there, from inside the distribution. There is no Windows-native
path and there is not going to be one.

### What has been validated, and what has not

Phase 6.5 was validated on a real Linux server — systemd 255, tmux 3.4, Ubuntu
24.04 under WSL2 — where the installer, the service, all three recovery cases,
the upgrade path, the health endpoint, the WebSocket terminal round trip and the
frontend served by the server were exercised rather than argued.

Two edges were not exercised there, and they are stated rather than glossed. The
installer's in-script build path (`go build`, `npm run build`) could not run in
that distribution, which has neither toolchain; both artifacts were built on the
Windows side and handed over through `--binary` and `--web`, which are supported
paths rather than a workaround. And the final link in the chain — the `claude`
CLI running inside a project's runtime — was not exercised end to end on that
host, because a per-user Claude Code install lives under a human's home
directory that the unprivileged service account cannot traverse. That is the
correct arrangement rather than a defect, and everything up to it was verified
end to end.

## 2. What you need

| | |
| --- | --- |
| OS | Linux with systemd. Ubuntu 22.04 and 24.04 are what this was tested on. |
| Runtime | tmux. A working `tmux -V` **in the service account's environment**, not only in yours. |
| Agent | The `claude` CLI, resolved the same way. Optional: without it AgentMux manages projects and hosts terminals, and has no agent to put in one. |
| Ports | One, 8787 by default, bound to loopback. |
| Disk | A data directory for the database and the per-project tmux sockets. Small: kilobytes per project. |

The distinction between your environment and the service account's is the single
most common way a correct-looking installation ends up unable to host a
terminal. The service runs as an unprivileged system account, with that
account's `PATH` and that account's home directory, and both `tmux` and `claude`
are resolved there. The installer says this at the point it matters, warning
that the CLI is resolved for the service account and has to be installed there
rather than in yours. A `tmux` that works in your shell proves nothing about the
account the unit starts. `sudo -u agentmux tmux -V` is the check that answers
the actual question, and `runtimeAvailable: false` on a server that otherwise
works is what a missing tmux in the service account's environment looks like.

A missing tmux is fatal to the install, because tmux is the runtime. A missing
`claude` is a warning, because a server without it is still a working server: it
manages projects and hosts terminals, and has no agent to put in one.

## 3. Installing

From a checkout, on the server, as a user with sudo:

```sh
sudo ./deploy/linux/install.sh --root /srv/projects
```

The script requires root, because it makes system-level changes, and it requires
systemd, because the unit it installs is a systemd unit. It checks for
`/run/systemd/system` rather than for the `systemctl` binary alone, and inside
WSL2 that check failing means systemd is off for the distribution — it prints
the `.wslconfig` snippet in §13 rather than a generic error.

It prints every change it makes, it never deletes a data directory, and
`--uninstall` leaves your projects, your database and your configuration where
they are. It is deliberately a script a person reads before running.

### Arguments

| Argument | Default | What it does |
| --- | --- | --- |
| `--prefix DIR` | `/opt/agentmux` | Where the server binary and the web bundle are installed. Must be an absolute path. |
| `--data DIR` | `/var/lib/agentmux` | AgentMux's own state: the database and the per-project tmux sockets. Must be an absolute path. |
| `--config DIR` | `/etc/agentmux` | The directory holding the configuration file. Must be an absolute path. |
| `--user NAME` | `agentmux` | The system account the service runs as. The group is set to the same name. |
| `--port N` | `8787` | The HTTP port written into a **new** configuration file. |
| `--root PATH` | `/srv/projects` | The Projects Root written into a **new** configuration file. |
| `--from DIR` | the checkout the script is in | A checkout to build from. |
| `--binary PATH` | — | An already-built `agentmux-server` binary; skips the Go build. |
| `--web PATH` | — | An already-built web bundle directory — the one holding `index.html`. |
| `--with-deps` | off | Let the script install tmux with apt if it is missing. |
| `--no-start` | off | Install and enable the unit, but do not start it. |
| `--uninstall` | off | Stop and remove the unit; keep the data, the configuration and the binary. |
| `-h`, `--help` | — | The script's own summary of all of the above. |

`--port` and `--root` configure a first install only, because they are written
into a configuration file that is never overwritten afterwards. An existing
configuration file is kept exactly as it is, which is what makes a re-run an
upgrade rather than a reset. This also means the installer's health check is
built from its own `--port` value, so an installation whose configuration names
a different port should be re-run with the matching `--port` — otherwise the
check asks a port nothing is listening on.

`--user` is the one argument whose effect is not confined to the files it
writes: the account is created if it does not exist, and the account's group is
used as the group of the configuration file and the data directory.

### What it creates

| Path | Owner | Mode | What |
| --- | --- | --- | --- |
| `/opt/agentmux/agentmux-server` | root | 0755 | the binary |
| `/opt/agentmux/web/dist` | root | 0755 | the built UI, served by the server |
| `/etc/agentmux/agentmux.yaml` | root:agentmux | 0640 | configuration; the service reads it and cannot rewrite it |
| `/var/lib/agentmux` | agentmux | 0750 | database, config default, and `tmux/` — one socket per project |
| `/etc/systemd/system/agentmux.service` | root | 0644 | the unit |

The config directory itself is made root-owned at 0750 and the file is
group-readable, so the service can read its configuration and cannot rewrite it.
That is on purpose: a setting that changes itself is a setting nobody can reason
about.

The service account is created as a **system** account with a login shell and a
home directory. The shell is not cosmetic — tmux runs a shell in every session,
and an account with `/usr/sbin/nologin` has no terminal to host. It is created
with no sudo rights and no supplementary groups, which is the first line of what
`docs/SECURITY.md` asserts about the deployment.

The binary is installed by writing `agentmux-server.new` and renaming it over
the old name, rather than being written in place. A server still running from
the old inode — a straggler the stop did not catch — can therefore finish rather
than read a half-written file.

The unit on disk is generated by substituting the paths and the account name
into the shipped unit file, comments included. The result is readable on its
own: an operator debugging a unit should not have to find the script that
generated it.

### `--with-deps`

Without it, a missing tmux stops the install and prints the command to run
instead. With it, the script runs `apt-get install -y --no-install-recommends
tmux` with `DEBIAN_FRONTEND=noninteractive`. A deployment script that silently
runs apt on somebody's server is a script that changes a machine it was not
handed, which is why installing anything is opt-in.

tmux is the only package the script will install. A missing Go toolchain or a
missing npm stops the build with a message telling you to install it, or to
build the artifact elsewhere and pass `--binary` or `--web`. The `--help` text
describes `--with-deps` as installing "tmux and the build tools"; the code
installs tmux only, and the README describes it the same way the code behaves.

### `--no-start`

`--no-start` installs everything, reloads systemd, enables the unit at boot, and
stops there, printing `sudo systemctl start agentmux` as the next step. It is
for a machine where you want to read the configuration before anything runs it.
Without it the script restarts the unit and then verifies the result.

Verification asks `http://127.0.0.1:<port>/health` up to thirty times, one
second apart, because the server has a database to open and a host to probe
before it answers. It prints the body it got, and it reads the `runtime` field:
when that says anything but `available` it warns that the server is up but
cannot host a terminal here, and points at the journal, because a service that
is up on a host with no tmux is a working AgentMux and exactly the state a
person installing it is most likely to be in. When the check fails outright it
prints `systemctl --no-pager --full status` and the last thirty journal lines —
a health check that failed and a service that failed to start look the same to a
reader and are different things to a fixer. The HTTP client is curl, then wget,
then bash's own `/dev/tcp`, since neither curl nor wget is guaranteed on a
minimal server.

Re-running the script without `--uninstall` is the upgrade procedure, and §11 is
the sequence it performs.

## 4. The systemd unit

`deploy/linux/agentmux.service` is installed to
`/etc/systemd/system/agentmux.service`. It is annotated in place, and the
annotations are the documentation for whoever has to change it. This section is
what those annotations mean.

| Directive | Value | Why |
| --- | --- | --- |
| `Type` | `simple` | The server does not fork; it is the process systemd supervises. |
| `User`, `Group` | `agentmux` | An unprivileged system account with no sudo rights. |
| `WorkingDirectory` | `/opt/agentmux` | Load-bearing, not cosmetic — see below. |
| `ExecStart` | `/opt/agentmux/agentmux-server` | No arguments. Everything the unit has to say is in `Environment=`. |
| `After`, `Wants` | `network-online.target` | A non-loopback bind before the interface exists fails outright. |
| `Restart` | `always` | Covers a clean exit, a crash, and a machine that came back up. |
| `RestartSec` | `2` | The first gap between restarts. |
| `RestartSteps`, `RestartMaxDelaySec` | `5`, `30` | The gap doubles instead of staying at two seconds. Needs systemd 254 or newer. |
| `TimeoutStopSec` | `30` | Bounded, because the server drains in-flight requests on SIGTERM. |
| `KillMode` | `process` | The line that decides whether a restart survives. §4.1. |
| `LimitNOFILE` | `65536` | One descriptor per browser socket, per pty and per tmux control client. |
| `UMask` | `0027` | Files the server creates are group-readable and not world-readable. |
| `StandardOutput`, `StandardError` | `journal` | journald owns the file. §10. |
| `SyslogIdentifier` | `agentmux` | The records are attributable in a shared journal. |
| `WantedBy` | `multi-user.target` | What `systemctl enable` acts on. |

`WorkingDirectory=/opt/agentmux` is the line a reader is most likely to treat as
tidiness. It is not. The built frontend's default path, `web/dist`, is relative,
and it is resolved against the process working directory, so the unit is what
makes the configuration's default land on the directory `install.sh` installs
the bundle into. An absolute `web.dir` removes the dependency; nothing else
does.

`After=` and `Wants=` are `network-online.target` rather than `Requires=`,
because a machine that reaches `multi-user.target` without a network still runs
AgentMux perfectly well on loopback — and loopback is the default bind.

### 4.1 Environment: two values, and why only two

```
Environment=AGENTMUX_CONFIG=/etc/agentmux/agentmux.yaml
Environment=AGENTMUX_DATA_DIR=/var/lib/agentmux
```

Anything set here wins over the configuration file, because the documented
precedence is CLI arguments over environment over file over defaults (§5). So a
setting that belongs in `/etc/agentmux/agentmux.yaml` must not also appear here:
an operator who edited the file and saw no effect would be looking at a value
the unit had already overridden, and would have no reason to suspect the unit.

These two are here because the file cannot carry them. The configuration file's
own location cannot be inside the configuration file, and the data directory is
deliberately not settable from the file — the default configuration file lives
*inside* the data directory, so a file that could move the data directory would
be deciding where it itself is. Both facts are also stated at the foot of
`config/agentmux.example.yaml`, which is where somebody looking for a `dataDir`
key will end up.

The practical rule for anyone editing this unit: a third `Environment=` line is
an override, not a default, and it should be added only for a value that cannot
be expressed in the file at all.

### 4.2 Restart behaviour

`Restart=always` covers both an exit and a crash, and it covers the case that
matters most here: a machine that came back up. A server that stopped is a
server whose terminals stopped being reachable. Combined with
`systemctl enable` — which the installer runs — this is what makes "the server
restarts itself" a fact rather than an intention. What does and does not come
back with it is §12.

`RestartSteps=5` and `RestartMaxDelaySec=30` make the gap between restarts
double rather than stay at two seconds. Without them a server that cannot start
— because its port is taken, because its database is unreadable — would be
restarted twice a second forever, filling the journal with the same failure and
making the one useful line impossible to find.

Those two directives need systemd 254 or newer: Ubuntu 24.04, Debian 13, RHEL
10. On an older systemd they are reported as unknown keys and ignored. Ubuntu
22.04 ships systemd 249, so on 22.04 what applies instead is the built-in start
rate limit: five restarts in ten seconds, after which the unit is left in a
failed state. That is a worse answer than a backoff but not a wrong one, and it
is not worth the complexity of shipping two unit files. `systemd-analyze verify
/etc/systemd/system/agentmux.service` on the target machine will say which case
you are in.

`TimeoutStopSec=30` is bounded rather than generous on purpose. The server
drains in-flight requests on SIGTERM and its own shutdown grace is ten seconds
by default (`server.shutdownGraceSeconds`), so ninety seconds would leave a
stuck client holding the unit in `deactivating` for a minute and a half while
the server had long since stopped listening.

`LimitNOFILE=65536` exists because the descriptor count is not small: one per
browser socket, one per pty, one per tmux control client, and a workspace of ten
viewers over several projects reaches systemd's default soft limit of 1024.

`UMask=0027` is the second of two mechanisms, not the only one. The installer
sets the data directory to 0750 and owns it by the service account; the umask
covers the files the server creates inside it afterwards.

### 4.3 KillMode=process

**This is the single setting in the unit that decides whether a restart
preserves runtimes.**

The default, `control-group`, sends the stop signal to every process in the
unit's cgroup. A project's tmux server is one of those. AgentMux starts a tmux
server as a child of the unit, and tmux detaches by forking — which changes the
terminal a process is attached to, but not the cgroup it belongs to. Detachment
is a terminal concept; the cgroup is a kernel concept, and the forked server
stays where the original was put.

Under the default, `systemctl restart agentmux` would therefore signal every
tmux server, and with them every session and everything running in one. The
server would come back up, reconcile honestly against what survived, and
correctly report every runtime as stopped — having destroyed them itself. The
symptom is a deployment where restarts appear to work and every restart costs
everyone their work.

With `KillMode=process` the stop signal goes to the server alone. The sessions
outlive it exactly as they outlive a Ctrl-C on the command line, and the server
adopts them on the way back up. That is the whole of runtime recovery on a
service restart, and it is worth testing on your own machine rather than taking
on faith:

```sh
sudo systemctl restart agentmux
tmux -S /var/lib/agentmux/tmux/<project-id>.sock ls
```

The socket is still there, and the session in it, and the process in the
session. `deploy/linux/recovery-test.sh` does exactly this and compares the tmux
server's pid before and after, so that a configuration which restarted the
server would be caught rather than mistaken for a preserved one.

What `KillMode=process` does not do is survive a reboot. A reboot ends every
process on the machine, tmux servers included, and no unit setting changes that.
Runtimes come back stopped after a reboot, and resuming one is a person's
decision (§12, Case B).

### 4.4 Hardening, deliberately absent

The unit ships with no `ProtectSystem`, no `ProtectHome`, no `PrivateTmp` and no
`NoNewPrivileges`, and the omission is a decision rather than an oversight.
AgentMux's whole job is to run programs a person asks for, in a terminal, as the
service account. Those programs are the user's own: a build that writes into the
projects root, a test that binds a port, an editor that keeps state in `$HOME`,
and routinely a `sudo` in a pane. A sandbox tight enough to be worth having is a
sandbox that breaks some of them, and it breaks them in the one place a person
cannot see the error: inside a pane that stops working.

So the decision belongs to the deployment rather than to the unit file. A
machine where AgentMux only ever builds code in one directory can be locked down
hard. `deploy/linux/README.md` §7 lists the directives worth adding and the
exact thing each one would break — `NoNewPrivileges=yes` stops `sudo` working in
every terminal, `ProtectHome=read-only` breaks Claude Code's own state
directory, and so on. Read that before adding any of them, and add them one at a
time with a real workload run through a pane after each.

What is true without any of them: the service runs as an unprivileged system
account with no sudo rights, it binds loopback by default, and it holds no
credentials to leak. `docs/SECURITY.md` §1 is the full statement of what that
does and does not protect.

## 5. Configuration

The configuration file lives at `/etc/agentmux/agentmux.yaml` in a deployment
made by `install.sh`, which is where the unit's `AGENTMUX_CONFIG` points. In any
other arrangement it is wherever `-config` or `AGENTMUX_CONFIG` says, and failing
both of those it is `<data-dir>/config.json`, which the server writes on first
run so that a fresh installation leaves something documented and editable
behind. That write happens only when no explicit file was read: a server started
with `-config /etc/agentmux/agentmux.yaml` has been configured by somebody who
knows where the file is, and a second file inside the data directory would be two
files that disagree, one of which is ignored. A file named explicitly that does
not exist is an error; the implicit `<data-dir>/config.json` not existing means
the defaults apply.

### 5.1 Format

The file is JSON by default, and YAML when its name ends in `.yaml` or `.yml`.
YAML is not a second configuration language. The document is decoded into a
generic structure, re-encoded, and handed to the same `json.Unmarshal` the JSON
path uses, so there is exactly **one decoder** underneath. The two formats
cannot drift apart, cannot disagree about a key name, and cannot differ in how
they treat an unknown key, a wrong type, or a missing section. What YAML adds is
comments, which is what lets the checked-in example explain itself.

An empty file, or one holding only comments, is an empty configuration rather
than a parse failure: a person who has emptied the file to start again means to
fall back to the defaults.

### 5.2 Precedence

```
CLI arguments  >  AGENTMUX_* environment  >  the config file  >  built-in defaults
```

The order is not a preference. A flag is what a person types when they want this
run to differ; an environment variable is what a service manager sets for every
run; the configuration file is what persists and is edited rarely; the defaults
are what happens when nobody has said anything. The more specific statement
beats the less specific one, which is why the file cannot override the flag.

The practical consequence, and the one worth internalising before editing the
unit: **anything set in the unit's `Environment=` lines cannot be changed by
editing the file.** The unit sets `AGENTMUX_CONFIG` and `AGENTMUX_DATA_DIR` and
nothing else, for the reasons in §4.1.

The configuration is read at startup. A change takes effect on the next start:

```sh
sudo systemctl restart agentmux
journalctl -u agentmux -n 5
```

The first startup line says what was resolved — version, commit, phase, config
file, data directory, projects roots, discovery depth, runtime mode, log level
and debug state — which is usually enough to see a mistake without reading
anything else.

### 5.3 Key reference

Every key is optional. An empty or zero value means the runtime's own default,
applied by the package that owns the concept rather than duplicated in the
configuration loader, so that a value written down twice cannot come to disagree
with itself.

| Config file key | Environment | Flag | Default | Meaning |
| --- | --- | --- | --- | --- |
| `server.host` | `AGENTMUX_HOST` | `-host` | `127.0.0.1` | The address to bind. Loopback is the right default for a terminal server with no authentication: who can reach the port is the whole of its access control. |
| `server.port` | `AGENTMUX_PORT` | `-port` | `8787` | The HTTP port. |
| `server.readHeaderTimeoutSeconds` | — | — | `10` | How long a client has to send its request headers. |
| `server.readTimeoutSeconds` | — | — | `0` | Disabled, and it must stay disabled: a read deadline applies to hijacked connections too. |
| `server.writeTimeoutSeconds` | — | — | `0` | Disabled for the same reason, and more sharply: a non-zero write deadline would close every terminal on a timer, because the terminal WebSocket is a hijacked connection. |
| `server.idleTimeoutSeconds` | — | — | `120` | How long an idle keep-alive connection is held open. |
| `server.shutdownGraceSeconds` | — | — | `10` | How long a shutdown waits for in-flight requests before the process exits. It is why `TimeoutStopSec` is 30 and not 90. |
| `server.controlGraceSeconds` | — | — | `30` | How long a browser that holds a project's terminal keeps it after its connection drops. A grace period rather than a timeout: the usual reason a controller's socket closes is a network that will come back, and releasing on the disconnect would hand the terminal to whoever asks first during the gap. Unset means the terminal package's own default, which is thirty seconds. |
| `server.allowedOrigins` | — | — | `[]` | Extra browser origins permitted by CORS, by the WebSocket upgrade's Origin check, and by the check on requests that change something, on top of the loopback origins that are always allowed. Needed only when the UI is served from somewhere other than this server. `docs/SECURITY.md` §4 is the rule. |
| `server.debug` | `AGENTMUX_DEBUG` | `-debug` | `false` | Widens what `GET /api/server` reports about this machine. §9. |
| `storage.sqlitePath` | — | — | `agentmux.db` | The metadata database. A relative path is resolved against the data directory, so the default is `<data-dir>/agentmux.db`. It holds projects, their workspace slots and one row per runtime; it holds no terminal output, no prompts and no conversation content. |
| `projects.roots` | `AGENTMUX_PROJECTS_ROOTS` | `-projects-root` (repeatable) | `/srv/projects`; otherwise the platform's own root | The directories scanned for projects, in order. The first is the default target for a new project. Absolute paths only. A root that does not exist is a warning rather than a failure, because the drive may not be mounted yet. |
| `projects.discoveryDepth` | — | — | `3` | How deep below a root discovery walks. Depth 2 reaches `Root/Collection/Project`; 3 leaves a level of margin. Valid range 1–8. |
| `projects.maxCandidates` | — | — | `500` | The bound on candidates from a single scan. |
| `projects.maxScanDirectories` | — | — | `20000` | The bound on directories visited, so a root that turns out to be a home directory with a million files in it ends the scan rather than the server. |
| `projects.scanTimeoutSeconds` | — | — | `20` | The time bound on a single scan. |
| `runtime.mode` | `AGENTMUX_RUNTIME_MODE` | `-runtime-mode` | `auto` | One of `auto`, `native` or `wsl`. On a Linux server this is effectively `native` whatever it says, and leaving it as `auto` is correct. |
| `runtime.distro` | `AGENTMUX_RUNTIME_DISTRO` | `-runtime-distro` | `""` (auto-detect) | The WSL distribution to run in. Ignored off Windows. Worth pinning when more than one distribution is installed and the automatic choice is not the one the projects live in. |
| `runtime.wslMountRoot` | `AGENTMUX_WSL_MOUNT_ROOT` | — | `/mnt` | The mount point under which WSL exposes Windows drives. |
| `terminal.tmuxBinary` | `AGENTMUX_TMUX_BINARY` | `-tmux-binary` | `""` (`tmux` on PATH) | Which tmux every project's runtime runs. Worth setting when a machine has more than one tmux: a runtime that measured one binary and drove another would report a version for a program that never ran. |
| `terminal.tmuxSocketDir` | `AGENTMUX_TMUX_SOCKET_DIR` | `-tmux-socket-dir` | `""` (`<data-dir>/tmux`) | Where the per-project sockets live. One project, one tmux server, one socket — that is the whole of the fault isolation, because a tmux server is identified by its socket and two sockets are two servers that cannot take each other down. A relative path is resolved against the data directory, not the working directory. |
| `terminal.shell` | `AGENTMUX_TERMINAL_SHELL` | `-shell` | `""` (the account's shell) | The shell a session runs when no command is given. |
| `terminal.claudeBinary` | `AGENTMUX_CLAUDE_BINARY` | `-claude-binary` | `""` (`claude` on PATH) | The Claude Code CLI a project's runtime starts, resolved in the account's environment. Configurable for the same reason `tmuxBinary` is. |
| `terminal.canonicalCols` | — | — | `0` (runtime default) | Canonical terminal width for a session that has no client yet. A session needs a size before anyone attaches; letting the first client decide would make the size a side effect of who clicked first and would resize a program that had already drawn itself. |
| `terminal.canonicalRows` | — | — | `0` (runtime default) | The same, in rows. |
| `terminal.historyChunks` | — | — | `0` (runtime default) | How much recent output is kept in memory per runtime so a reconnecting client can be given what it missed. This is not the session's scrollback: that lives in tmux and is bounded by tmux's own history limit. |
| `terminal.historyBytes` | — | — | `0` (runtime default) | The same bound in bytes. |
| `terminal.socket` | `AGENTMUX_TMUX_SOCKET` | `-tmux-socket` | — | **Deprecated.** It named one shared tmux socket, which stopped describing anything in Phase 2.5 when every project got its own server. It is accepted and warned about rather than rejected, so an existing invocation keeps starting. It is not reinterpreted as a path or as a socket directory, because both readings would move every runtime to a path the user never named, silently. Remove the setting. |
| `logging.level` | `AGENTMUX_LOG_LEVEL` | `-log-level` | `info` | `debug`, `info`, `warn` or `error`. |
| `logging.format` | `AGENTMUX_LOG_FORMAT` | `-log-format` | `text` | `text` is for a person reading `journalctl`; `json` is for a log pipeline. Either way the record carries the same four things. §10. |
| `web.dir` | `AGENTMUX_WEB_DIR` | `-web-dir` | `web/dist` | The directory holding the built UI. A relative path is resolved against the process working directory, which the unit sets to the prefix. When the directory is missing the server still runs; it serves the API and no UI, and says so in its log. |
| `dataDir` | `AGENTMUX_DATA_DIR` | `-data-dir` | the platform's configuration directory | **Not settable from the file.** §5.4. |
| — | `AGENTMUX_CONFIG` | `-config` | `<data-dir>/config.json` | The configuration file's own path. A file cannot carry its own location, which is the other half of why this is an environment variable in the unit. |

`-projects-root` is repeatable and `AGENTMUX_PROJECTS_ROOTS` accepts a
path-list, split on the platform's list separator and on a comma, so a value
written on one platform is still usable on the other. Repeated roots are
de-duplicated, comparing case-insensitively where the filesystem does.

Precedence has one asymmetry worth knowing: `server.debug` is a boolean, so the
flag can only ever turn it on. That is what the flag is for — a person who wants
it for one run does not want to edit a file first — and the way to turn it off
is to remove it from the file or the environment, which is where it was turned
on. A tri-state pointer would let `-debug=false` beat a file that says `true`,
at the cost of a field every reader has to dereference to answer "is this on".

### 5.4 Why `dataDir` is not in the file

The data directory holds the database, the configuration file and the
per-project tmux sockets. It is set from `-data-dir`, then `AGENTMUX_DATA_DIR`,
then the built-in default. It is deliberately absent from the configuration file,
and the reason is circularity: the default configuration file lives *inside* the
data directory, so a file that could move the data directory would be deciding
where it itself is. A `dataDir` key in the file is ignored, and the server
reports it as a warning rather than accepting it silently — a warning because a
value that looked effective and was not is worse than one that never existed.

The unit sets `AGENTMUX_DATA_DIR=/var/lib/agentmux`, which is why that line
cannot be moved into the file either.

### 5.5 What is rejected and what is only warned about

Fatal at startup: an empty `server.host`; a `server.port` outside 1–65535; a
`runtime.mode` that is not `auto`, `native` or `wsl`; a `logging.level` that is
not `debug`, `info`, `warn` or `error`; a `logging.format` that is not `text` or
`json`; a `projects.discoveryDepth` outside 1–8; an empty `projects.roots`; and
any root that is not an absolute path.

Non-fatal, collected and surfaced through `GET /api/server` so the UI can show
them instead of pretending everything is fine: a Projects Root that is not
accessible or is not a directory, a `dataDir` key in the file, and the
deprecated `terminal.socket`. A missing root is a warning because refusing to
start would mean the whole server is down because one volume is not mounted.

## 6. Secrets

There are none, and there is nowhere in the configuration to put one.

AgentMux holds no credentials of its own. The Claude Code CLI keeps its own, in
its own files, outside AgentMux's reach, and whether the CLI is signed in is
decided when it starts and is said on its own terminal — the server does not
look, so it does not claim. The configuration file has no `apiKey`, no token and
no provider block, and adding one would not be read. That is deliberate: a file
that looks like it holds a credential is a file somebody will put one in.

The installation identifier the server generates on first run is stored in the
settings table and is not returned by any endpoint. An identifier that is stable
across every request from a machine is a tracking token whether or not it is
called a credential.

What that leaves as the deployment's access control is the bind address. There
is no authentication in this build, so who can reach the port is the whole of
it. `docs/SECURITY.md` is the full statement — §1 on what the unprivileged
account and the loopback bind do and do not protect, §2 on the absent
credentials, §4 on what the API deliberately does not report, and §7 on exactly
what debug mode adds.

The same holds for the repository and for the machine you deploy from: nothing
in this tree stores a credential, and the one local note that names a machine —
`docs/serverinfo.md`, the runtime test host — carries a host, an account and the
statement that authentication is interactive, and is in `.gitignore` besides.
`docs/SECURITY.md` §5.1 is the full statement.

## 7. Running it

```sh
sudo systemctl start agentmux
sudo systemctl stop agentmux
sudo systemctl restart agentmux
systemctl status agentmux
journalctl -u agentmux -f
curl -s http://127.0.0.1:8787/health
```

The unit is enabled at boot by the installer, so `systemctl enable agentmux` is
only needed if you installed with `--no-start` on a machine where you then
disabled it, or if you are assembling the deployment by hand.

The configuration is read once, at startup. There is no reload signal: a change
to `/etc/agentmux/agentmux.yaml` takes effect on the next start, and `restart`
is the way to get one.

`systemctl status` is the first thing to look at and the least informative on
its own, because it shows the unit's state rather than the server's. A server
that is running and cannot host a terminal is `active (running)`, and correctly
so. The distinction is in the health endpoint rather than in the unit state,
which is the subject of the next section.

## 8. Health and readiness

```
GET /health
```

```json
{"status":"ok","version":"0.6.5","commit":"a1b2c3d4e5f6","runtime":"available"}
```

| Field | Value | What it is |
| --- | --- | --- |
| `status` | `"ok"` | Liveness. This is the field a supervisor reads. |
| `version` | the release, e.g. `"0.6.5"` | Which build answered. |
| `commit` | the git revision, twelve characters | Absent when the binary carries no commit. |
| `runtime` | `"available"` or `"unavailable"` | Readiness: whether a persistent terminal session can execute on this machine at all. |

`status` is liveness and `runtime` is readiness, and the two are different
questions that a reader has to be able to tell apart. A server on a host with no
tmux is working perfectly: it manages projects, serves the UI and answers every
endpoint. It cannot host a terminal. Calling that unhealthy would have a
supervisor restart a process that is doing nothing wrong, in a loop, and the
restart would not install tmux. So the word is `unavailable` rather than
`unhealthy`, and the reason is in the journal rather than in this body.

The endpoint **always returns 200**. The only failures this handler could report
are failures of the machine it runs on, and a health check that returned 503 for
a missing tmux would be asking a supervisor to restart AgentMux until tmux
appeared. A supervisor should not have to parse a body to learn that the process
is alive.

The response carries no credential, no filesystem detail and no secret. It is
three fields, and the shortness is the design: a health check is asked every few
seconds by a program that has to decide one thing, and everything else is either
useless to that decision or reconnaissance. A test asserts the exact key set
rather than the absence of particular names, so a field added later by somebody
who did not read `docs/SECURITY.md` fails the build — a list of forbidden names
would not have caught it. Debug mode does not reach this endpoint at all.

The path is deliberately outside `/api`. It is not part of the product's API
surface, it is not versioned with the protocol, and a monitor should not have to
track the protocol version to ask whether the process is alive. A test pins the
placement, including that the SPA fallback does not answer it.

`runtime` is computed from three conditions, all of which are required and each
of which is a different kind of fact: this build contains the runtime; the
process is on the side of the WSL boundary where a runtime can execute; and the
tmux probe — which asks the environment the runtime will actually use — finds
tmux. The same three feed `GET /api/server`, which publishes the verdict
alongside the inputs it was derived from. The rule is written down once, so the
two endpoints cannot come to disagree about the same machine.

## 9. Server information

```
GET /api/server
```

This is the richer answer, and it is the one the UI reads to decide what to
offer. It reports the application name, the version, the roadmap phase and
`status: "online"`; when the process started and how long it has been up
(`startedAt` in RFC 3339 and `uptimeSeconds`); the operating system the process
runs on and its architecture (`host`, `hostArch`), the runtime mode and the
runtime's own OS (`runtimeMode`, `runtimeOs`), the WSL distribution when there is
one, and the path mapper in use; where the server process itself is running
(`environment`), which on Windows is not where the runtime runs; and the two
halves of "what would run a terminal here, and is it installed" —
`runtimeBackend`, which is the runtime's own name for itself and is `tmux`
today, and `tmuxAvailable`, which is the dependency probe's verdict.

Alongside those it reports the Projects Root and the full ordered list, the
discovery depth, whether this build contains the runtime at all
(`terminalRuntimeImplemented`), one sentence naming why a terminal cannot be
offered when it cannot (`terminalBlocker`), the dependency probe results, the
provider block, the resolved tmux installation (the binary, its version and
where the sockets live), the resolved Claude Code installation when there is
one, and the feature map the UI uses to disable what this server cannot do
instead of inferring capability from a version string. Configuration warnings
are carried here, which is how a Projects Root that is not mounted becomes
visible in the interface.

Hostname and username are not reported by this endpoint, in any mode. The four
fields that describe where the server keeps its own files —
`dataDirectory`, `databasePath`, `configFile` and `webDirectory` — are reported
**only** when debug mode is on, and are otherwise absent rather than blank, so a
client can read their absence as "withheld" rather than "misconfigured". Each is
resolved during configuration and cannot legitimately be empty.

Debug mode is turned on by `-debug`, by `AGENTMUX_DEBUG`, or by `server.debug`
in the configuration file. Its reason for existing is that a bug report about a
misconfigured deployment is otherwise unanswerable: an operator can turn it on
for one run and hand over a report that names the paths.

It is deliberately separate from `logging.level`. An operator who turns up log
verbosity to chase a problem has not thereby decided to publish a map of the
disk from an endpoint that has no authentication, and a setting that coupled the
two would publish those paths by surprise. Enabling it affects nothing else: no
handler is skipped, no check is relaxed, and no credential exists to reveal.

## 10. Logs

### 10.1 The record

Every record carries four things: a timestamp, a level, a component and an
event. The first two come from the structured handler, the component names the
subsystem the record came from, and the event is the message — what happened, in
the present tense, without a subject.

```
time=2026-09-19T10:04:11.882Z level=INFO msg="runtime started" component=session projectId=prj_7f3a session=amx-prj_7f3a
```

Structured attributes carry the detail: identifiers, counts, durations,
outcomes. Identifiers rather than names, because a project may be renamed and a
line that quoted the old name would describe a project that no longer exists
under it.

The component is one of the subsystem names, and they follow the package layout
so that a record can be traced back to the code that wrote it without a lookup
table.

| Component | Subsystem |
| --- | --- |
| `config` | configuration resolution |
| `storage` | the metadata database |
| `host` | the host adapter and its platform probing |
| `project` | the project service and discovery |
| `session` | the terminal runtime and its sessions |
| `claude` | the Claude Code launcher |
| `terminal` | the terminal hub and the control plane |
| `httpapi` | the HTTP API |
| `server` | the process itself: startup, configuration, listening, shutdown |

`logging.format: json` changes the encoding and nothing else. The same four
things are in the record either way, which is what makes a query written against
the text output still meaningful against a pipeline's JSON.

### 10.2 What is never logged

- **Terminal output.** A pane's bytes are the program's output, not AgentMux's
  event, and they may contain anything the user's program printed. Where a
  record needs to say something about a block of terminal bytes, it says how
  many there were and not what they were.
- **Anything the user typed, and any prompt or conversation content.**
- **Credentials**: API keys, tokens, passwords, provider configuration,
  `Authorization` headers, request or response bodies.

The HTTP middleware records only the method, the path, the status, the byte
count and the duration. The path is logged without its query string, which can
carry a token. Anything that might carry a credential must go through the
redaction helper before it reaches a log record, which writes a fixed
placeholder in place of a non-empty value and nothing at all in place of an
empty one.

A test asserts this rather than trusting it, and it asserts the sharpest case.
One test types a marker into a terminal, has the terminal print a different
marker, and has a viewer's input refused as a third, then asserts that none of
the three appears anywhere in what the server wrote — refused input being the
case most likely to be logged, because it is text a person typed into something
that told them not to, and it is exactly the text that would be sitting in a log
the next morning. A second test asserts the shape from the other side: it walks
a full control-plane lifecycle and fails on any log attribute that is not the
client, the project, or the event.

### 10.3 Rotation

AgentMux ships no logger backend and no log rotation, deliberately. The server
writes to stderr, and under systemd `StandardOutput=journal` and
`StandardError=journal` hand that to journald — which owns the file, rotates it
by size and age, and vacates old entries. A logger that also rotated would be a
second thing deciding when a file ends.

That means rotation, retention and vacuuming are journald's configuration and
not AgentMux's, in `/etc/systemd/journald.conf`:

| Setting | What it bounds |
| --- | --- |
| `SystemMaxUse` | The total size of the persistent journal under `/var/log/journal`. |
| `SystemKeepFree` | How much filesystem space journald leaves free. |
| `MaxRetentionSec` | The maximum age of an entry, regardless of size. |
| `MaxFileSec` | How long a single journal file is written to before journald starts a new one. |
| `RuntimeMaxUse` | The same size bound for the volatile journal under `/run/log/journal`, which is what a machine without persistent storage uses. |

The queries an operator actually wants:

```sh
journalctl -u agentmux -f                       # follow it
journalctl -u agentmux -n 50 --no-pager         # the last fifty lines
journalctl -u agentmux --since "-15min"         # what happened during the incident
journalctl -u agentmux -p err                   # errors and above only
journalctl -u agentmux -o json-pretty           # pipe it somewhere
journalctl -u agentmux | grep orphan            # sessions belonging to no project
journalctl -u agentmux -n 5                     # the startup line, after a configuration change
```

`SyslogIdentifier=agentmux` is what makes the unit's records attributable if the
journal is also carrying another service's output under a shared identifier. On
a machine where more than one AgentMux installation writes to the journal — two
data directories, two units — set a distinct `SyslogIdentifier` in each unit
rather than filtering after the fact.

## 11. Upgrade

The sequence is `stop service → back up the database → replace the binary →
start service → verify /health`.

**Re-running `install.sh` is the upgrade.** It performs that sequence rather
than describing it:

```sh
git pull
sudo ./deploy/linux/install.sh
```

Concretely, the script checks whether there is an installation here — a binary
under the prefix, or the unit file — and if there is, stops the service and
copies the database aside before replacing anything. It copies
`agentmux.db.pre-upgrade-<UTC timestamp>` and, when they are present, the `-wal`
and `-shm` beside it, as one set; and it says so when there is nothing to copy.
It keeps the existing configuration file untouched; installs the new binary by
rename and replaces `web/dist`; reloads systemd and re-enables the unit;
restarts it; and verifies `/health`, failing the script if the check does not
answer within thirty seconds. It closes by printing that runtimes were left as
they were: a project whose tmux session survived the restart is running again,
and one whose session did not is reported stopped, with starting it being yours
to ask for.

Two details of that are worth stating rather than leaving in the source, because
both were wrong in the first version of the script:

- **The copy is taken whenever there is a database, not only when the service
  was up.** The script used to decide it was an upgrade by asking
  `systemctl is-active`. A server that is down — crashed, stopped by hand, or
  not yet started since a reboot — therefore read as a fresh install: the copy
  was skipped silently and the binary was replaced underneath a database that
  had been written to since the last upgrade. The case where a backup matters
  most was the one case that did not take one. It now asks whether an
  installation exists, which is a question about the machine rather than about
  the moment.
- **The `-wal` is part of the copy.** SQLite runs here in WAL mode, so a commit
  appends to `agentmux.db-wal` and the database file is only brought up to date
  at a checkpoint. A copy of `agentmux.db` alone is consistent as of the last
  checkpoint and quietly missing everything after it. A clean stop checkpoints
  and removes the `-wal`, so in the ordinary case there is nothing to copy and
  this is a one-file backup — but the script now also runs when the service did
  *not* stop cleanly, which is exactly when a `-wal` is on disk holding
  transactions. `docs/BACKUP.md` §3.3 is the same three files.

By hand, if you would rather:

```sh
sudo systemctl stop agentmux
sudo cp -a /var/lib/agentmux/agentmux.db      /var/lib/agentmux/agentmux.db.backup-$(date -u +%Y%m%dT%H%M%SZ)
sudo cp -a /var/lib/agentmux/agentmux.db-wal  /var/lib/agentmux/agentmux.db.backup-$(date -u +%Y%m%dT%H%M%SZ)-wal 2>/dev/null || true
sudo install -m 0755 ./agentmux-server /opt/agentmux/agentmux-server
sudo systemctl start agentmux
curl -s http://127.0.0.1:8787/health
```

A plain `cp` of the database is a valid backup **here and only here**, because
the service is stopped and nothing is writing to it. A copy taken while the
server runs can miss the write in flight and produce a file that opens cleanly
and is quietly missing its last transaction — which is worse than a copy that
fails, because it looks like a good backup until it is restored. For a copy
taken without stopping anything, use the hot-copy procedure in
`docs/BACKUP.md` §2.

Verify the upgrade took effect by comparing the version, not by trusting that the
service restarted:

```sh
curl -s http://127.0.0.1:8787/health
# {"status":"ok","version":"0.6.5","commit":"a1b2c3d4e5f6","runtime":"available"}
```

`commit` is the git revision the binary was built from, set at link time by the
release build. A service that restarted into the old binary looks identical to
one that restarted into the new one until you compare this field. The field is
absent on a binary built without the linker flags — a plain `go build` from a
working tree, or a `go test` binary — and its absence is the honest answer
rather than a placeholder. `agentmux-server -version` prints the same identity,
with the build date, without asking the running server anything.

One property of the upgrade is worth stating because it is easy to mistake for a
failure: the tmux sessions are not restarted, not re-created and not disturbed.
The stop that begins an upgrade detaches the server from them, and the start that
ends it adopts them back. Runtimes are the one thing an upgrade does not touch.

## 12. Recovery

Three cases, and the difference between them is the whole of what "recovery"
means here. `deploy/linux/recovery-test.sh` exercises all three against a real
installation — it stops and starts the real unit, looks at real tmux sockets, and
compares the tmux server's pid across a restart so that a configuration which
restarted the server would be caught rather than mistaken for a preserved one.

### Case A — the service restarted, and tmux is still there

```sh
sudo systemctl restart agentmux
curl -s http://127.0.0.1:8787/api/server
```

Every project whose tmux session survived is **running again**. The server adopts
the existing session on startup and re-attaches its output reader. Nothing is
restarted and nothing is lost: the sessions, their scrollback and whatever
processes were in them outlived the server, which is the same property they have
when the server is stopped with Ctrl-C. This is what `KillMode=process` buys
(§4.3), and it is the reason a restart is a safe operation rather than a
disruptive one.

### Case B — the machine rebooted, so tmux is gone

A reboot ends every process on the machine, tmux servers included, and no unit
setting changes that. The service comes back on its own, because
`Restart=always` covers the exit and `systemctl enable`, which the installer
runs, brings the unit up at boot. The runtimes do not come back.

```sh
sudo reboot
# after it comes up
systemctl status agentmux
curl -s http://127.0.0.1:8787/api/projects # every runtime: stopped
```

Every runtime is reported **stopped**, by the same reconciliation that runs on
any startup. Reconciliation **never starts anything**. Starting a runtime is a
person's decision, and the server does not make it: a session is a place somebody
was working, and re-creating it unasked would be replacing their terminal with a
fresh one and calling it recovery. The reconciliation reports how many are
running, how many are stopped and how many are orphans, and stops there.

### Case C — a tmux session that belongs to no project

The database says which projects have runtimes. The machine's tmux sockets say
which sessions exist. When a socket exists and the database has no project for
it — a database restored from a backup taken before the project was created, a
project deleted while its session ran — the session is reported as an
**orphan**:

```
level=WARN msg="a terminal session belongs to no registered project; it is left running" component=session
```

It is reported and left running. It may hold work somebody needs, and killing it
would be a guess. The warning names the session, the project id it would have
belonged to and the socket directory; the socket path is the one to attach to if
you want to see what is in it. Two details of the classification are worth
knowing, because they explain answers that look like misses: a socket is treated
as an orphan only when the id in its filename has the shape AgentMux issues, and
a socket the service account cannot read is a different case with a different
and correct answer — reported as unclassifiable and left untouched.

## 13. WSL2

On a Windows host the server belongs inside the distribution. Enable systemd for
that distribution first, from Windows, in `%USERPROFILE%\.wslconfig`:

```ini
[boot]
systemd=true
```

Then `wsl --shutdown` and start the distribution again. `systemctl
is-system-running` should say `running` rather than `offline` or `unknown`.
`install.sh` refuses to install without it: it checks for `/run/systemd/system`
and, failing that, prints this snippet rather than a generic message about a
missing supervisor. There is no Windows-native unit and there will not be one.

After that the instructions above are unchanged; you run the same script inside
the distribution. Two differences are worth knowing.

**Where the projects live.** Windows drives are mounted under `/mnt`, and every
file operation on a project under `/mnt/c/...` crosses the 9p boundary into the
Windows filesystem, which is slower than an operation on the distribution's own
filesystem. AgentMux maps the two path spaces for you either way — the same
project has a Windows path and a WSL path, and the server reports both — and the
speed difference is real and is not AgentMux's to fix. A project that will be
built repeatedly belongs under the distribution's own filesystem.

**Reboots and WSL.** `wsl --shutdown`, or an idle timeout, ends the distribution
and everything in it, tmux servers included. From AgentMux's point of view this
is Case B exactly: the sessions are gone and the runtimes come back stopped. The
service returns when the distribution starts again, which is a Windows boot by
default. Windows Task Scheduler can start the distribution at logon if you want
it up without opening a terminal window first.

The unit's `After=network-online.target` matters slightly less inside WSL than on
a server, because the distribution's networking appears with the distribution;
it costs nothing and it is correct in both places.

## 14. Performance baseline

The numbers below were **measured**, not estimated. Single machine, WSL2 Ubuntu
24.04, a real systemd unit, measured on 2026-09-19. The tool is
`deploy/linux/loadtest.py`.

| Scenario | CPU, % of one core | Peak RSS | File descriptors | WebSocket subscribe p95 | Runtime read p95 |
| --- | --- | --- | --- | --- | --- |
| 1 project / 1 viewer | 0.3 % | 18 MB | 20 | 0.2 ms | 7.0 ms |
| 5 projects / 5 viewers | 0.7 % | 21 MB | 40 | 0.3 ms | 7.6 ms |
| 5 projects / 10 viewers | 1.0 % | 23 MB | 45 | 0.7 ms | 9.1 ms |

`CPU` is the server process's CPU over the measured phase as a percentage of one
core. `Peak RSS` is the server process's resident set. `File descriptors` is
that process's open descriptor count, and it is the number to watch as a
workspace grows, because it is one per browser socket, one per pty and one per
tmux control client — which is why the unit raises `LimitNOFILE` to 65536
(§4). `WebSocket subscribe p95` is the time from a subscribe on an open socket
to the reply for it. `Runtime read p95` is the HTTP time to ask for a project's
runtime.

Reproduce it with:

```sh
python3 deploy/linux/loadtest.py --scenario all
```

The three scenario names are `1project`, `5projects` and `10viewers`, and `all`
runs the table above in order. It measures the server as a deployment rather than
as a benchmark: one process, one machine, no synthetic flood, and what it reports
is what an operator would see in `systemctl status` and `top` while a handful of
people look at terminals. The WebSocket client is written into the script rather
than imported, because installing a dependency in order to measure a server is a
poor trade. It leaves the projects it created in place and stops their runtimes
when it is done, so a second run is comparable to the first.

This is a baseline for an operator sizing a machine or deciding whether a
workspace will fit, and not a stress-test platform. The load is bounded by what
the product is for — a workspace of people, each holding one project's terminal
at a time — and a number taken under a shape the product does not have would not
predict the shape it does.

## 15. Uninstall

```sh
sudo ./deploy/linux/install.sh --uninstall
```

It requires root, like every other mode of the script, and it does not require
systemd to be present, so it remains usable on a machine where the unit was
never installed. It stops the unit, disables it, removes
`/etc/systemd/system/agentmux.service` and reloads systemd, and it prints that
no unit was installed when there is none.

Everything else is **kept, deliberately**. Your projects, your database, your
configuration and the installed binary are left exactly where they are, and the
script prints the three paths — data, config, prefix — so that deleting them is a
decision you make rather than a thing that happened. Nothing in the script
removes a data directory.

If tmux sessions from the installation are still running, it says so and names
the socket directory, because stopping the service detached the server and did
not end the sessions. That is the same property that makes a restart safe, seen
from the other side. End them individually if you want them gone:

```sh
tmux -S /var/lib/agentmux/tmux/<project-id>.sock kill-server
```

To remove the rest, remove the three directories the script printed. Check the
data directory before you do: it holds the database and every project's socket,
and the sessions attached to those sockets may still be holding work.

## 16. Without systemd

The shipped unit is a systemd unit. On a machine whose init is something else,
run the server under whatever supervisor you do have. `install.sh` says this
rather than guessing, and there is no second unit file and no init script
shipped with AgentMux.

What a supervisor has to reproduce from the unit is short:

| What the unit provides | Why the server needs it |
| --- | --- |
| `WorkingDirectory=/opt/agentmux` | `web.dir` defaults to the relative `web/dist`, which is resolved against the working directory. An absolute `web.dir` removes the dependency. |
| `AGENTMUX_CONFIG`, `AGENTMUX_DATA_DIR` | The configuration file's location and the data directory, which the configuration file is not allowed to carry. |
| `Restart=always` | A server that stopped is a server whose terminals stopped being reachable. |
| `KillMode=process` | The server must be signalled without signalling the tmux servers in its process group. Under a supervisor, this is "stop the server, not the group" — a supervisor that kills the whole process group takes every session with it, and the reasoning in §4.3 applies unchanged. |
| `LimitNOFILE=65536` | Descriptor count grows with viewers, ptys and tmux control clients. |
| Stderr to a file or a journal | The server writes its records to stderr and ships no rotation of its own (§10.3). Whatever collects that stream owns rotation and retention. |

The binary takes every setting as a flag as well as through the environment, so
a supervisor that prefers a command line to an environment block can use one.
Log direction and level are configurable; log destination is not, because the
supervisor owns the file.

## 17. Troubleshooting

```sh
systemctl status agentmux
journalctl -u agentmux -n 50 --no-pager
```

| Symptom | Almost always |
| --- | --- |
| `status=203/EXEC` | The binary is not at `ExecStart`, or is not executable. Check `install -m 0755` was the last thing to write it. |
| Binds, then `runtimeAvailable: false` | tmux is not on **the service account's** PATH. `sudo -u agentmux tmux -V` is the check; a `tmux` that works in your shell proves nothing. Install it in the account's environment, or set `terminal.tmuxBinary` to an absolute path. |
| `address already in use` | Something else holds the port — often an older AgentMux started by hand. `ss -ltnp \| grep 8787`. |
| Restarts every two seconds | The journal has the reason; it is at `error` level and near the bottom of the last attempt. On systemd 254 or newer the loop will slow itself down; on 22.04 it will stop after five restarts in ten seconds and leave the unit failed. |
| Sessions gone after `systemctl restart` | `KillMode` is not `process`. See §4.3 — this is the one setting that decides whether a restart preserves runtimes. |
| `runtimeAvailable: false` and the health check passes anyway | Correct behaviour, and worth reading twice: a server up on a host with no tmux is a working server that cannot host a terminal. §8. |
| The installer's health check fails but the service is running | The check is built from the installer's own `--port`, which may not be the port the existing configuration names. §3. |
| `dataDir` in the configuration file has no effect | It is ignored by design and reported as a warning. §5.4. |
| A `terminal.socket` setting has no effect | It is deprecated and reported as a warning. §5.3. |

The startup log line says what was resolved — the version, the commit, the
phase, the configuration file, the data directory, the projects roots, the
discovery depth, the runtime mode, the log level and whether debug is on — which
is usually enough to see the mistake without reading anything else. It is written
at startup, so `journalctl -u agentmux -n 5` straight after a restart is the
first query to reach for.

Two edges of this deployment are known and are stated rather than glossed, both
from the Phase 6.5 validation. The final `claude` link — the CLI running inside
a project's runtime — was not exercised end to end on the validation host,
because a per-user Claude Code install under a human's home directory is not
traversable by the unprivileged service account. That is the correct
arrangement, and it is also the shape of a failure an operator will meet: a
runtime that starts, hosts a terminal and has no agent in it. The installer's
build path was likewise not exercised there, for want of a toolchain; see §1 for
both.
