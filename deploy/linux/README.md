# Deploying AgentMux on Linux

Three files and a procedure. This is the guide for somebody putting AgentMux on
a server they intend to keep running; `docs/DEPLOYMENT.md` is the reference it
points into, and `docs/SECURITY.md` is what to read before the server's port is
reachable from anywhere but this machine.

| File | What it is |
| --- | --- |
| `install.sh` | Installs, and re-run, upgrades. Reads before it writes and prints every change. |
| `agentmux.service` | The systemd unit. Annotated in place; the comments are the documentation. |
| `README.md` | This file. |

## 1. What AgentMux needs

| | |
| --- | --- |
| OS | Linux with systemd. Ubuntu 22.04 and 24.04 are what this was tested on. |
| Runtime | tmux. A working `tmux -V` **in the service account's environment**, not only in yours. |
| Agent | The `claude` CLI, likewise. Optional: without it AgentMux manages projects and hosts terminals, and has no agent to put in one. |
| Ports | One, 8787 by default, bound to loopback. |
| Disk | A data directory for the database and the per-project tmux sockets. Small: kilobytes per project. |

There is no Docker image and this phase does not add one. The runtime is tmux
driving real ptys in a real filesystem on the host, and putting a container
between the two would add a layer whose failure modes are indistinguishable
from tmux's own — which is the thing the whole design is trying to keep
diagnosable. A container is a reasonable future phase with its own answer to
"which filesystem do the projects live on".

## 2. Quick start

From a checkout, on the server, as a user with sudo:

```sh
sudo ./deploy/linux/install.sh --root /srv/projects
```

It builds the server and the frontend, creates an `agentmux` system account,
installs everything under `/opt/agentmux`, writes `/etc/agentmux/agentmux.yaml`
if there is not one, installs and enables the unit, starts it, and checks
`/health`.

If the machine has no Go toolchain or no Node, build the two artifacts somewhere
else and hand them over:

```sh
sudo ./deploy/linux/install.sh --root /srv/projects \
    --binary ./agentmux-server \
    --web    ./web/dist
```

`--help` lists everything else. Two worth knowing:

- `--with-deps` lets the script `apt-get install tmux`. Without it, a missing
  tmux stops the install and prints the command instead. A deployment script
  that silently runs apt on your server is a script that changes a machine you
  did not hand it.
- `--no-start` installs and enables the unit without starting it, for a machine
  where you want to look at the configuration first.

### What it creates

| Path | Owner | Mode | What |
| --- | --- | --- | --- |
| `/opt/agentmux/agentmux-server` | root | 0755 | the binary |
| `/opt/agentmux/web/dist` | root | 0755 | the built UI, served by the server |
| `/etc/agentmux/agentmux.yaml` | root:agentmux | 0640 | configuration; the service reads it and cannot rewrite it |
| `/var/lib/agentmux` | agentmux | 0750 | database, config default, and `tmux/` — one socket per project |
| `/etc/systemd/system/agentmux.service` | root | 0644 | the unit |

## 3. The unit, and the two lines that matter

```sh
systemctl status agentmux      # is it up
journalctl -u agentmux -f      # what it is saying
sudo systemctl restart agentmux
sudo systemctl stop agentmux
```

`Restart=always` with `RestartSec=2` is what makes the server come back after a
crash and after a reboot (`systemctl enable` covers the second).

**`KillMode=process` is what makes a restart survivable.** The default,
`control-group`, signals every process in the unit's cgroup on stop — and a
project's tmux server is in that cgroup, because AgentMux starts it as a child
and tmux detaches by forking, which changes the terminal but not the cgroup.
Under the default, `systemctl restart agentmux` would kill every tmux server and
with them every session and everything running in one. The server would come
back, reconcile honestly, and report every runtime as stopped — having destroyed
them itself. The unit file explains this at the line; it is the single setting
here that changes what the product does.

The unit deliberately ships **without** `ProtectSystem`, `ProtectHome`,
`PrivateTmp` or `NoNewPrivileges`. §7 below says why, and what each would break.

## 4. Recovery

Three cases, and the difference between them is the whole of what "recovery"
means here.

### Case A — the service restarted, tmux is still there

```sh
sudo systemctl restart agentmux
curl -s http://127.0.0.1:8787/api/server   # runtimeAvailable, no orphans
```

Every project whose tmux session survived is **running again** — the server
adopts the existing session on startup and re-attaches its output reader.
Nothing is restarted and nothing is lost: the sessions, their scrollback and
whatever processes were in them outlived the server, which is the same property
they have when the server is stopped with Ctrl-C.

This is what `KillMode=process` buys, and it is worth testing on your own
machine rather than taking on faith:

```sh
# start a runtime, note its session, restart the service, look again
sudo systemctl restart agentmux
tmux -S /var/lib/agentmux/tmux/<project-id>.sock ls
```

The socket is still there, and the session in it, and the process in the
session.

### Case B — the machine rebooted, so tmux is gone

A reboot ends every process on the machine, tmux servers included, and no unit
setting changes that. The service comes back on its own; the runtimes do not.

```sh
sudo reboot
# after it comes up
systemctl status agentmux                  # active (running)
curl -s http://127.0.0.1:8787/api/projects # every runtime: stopped
```

Every runtime is reported **stopped**, by the same reconciliation that runs on
any startup. Starting one is a person's decision, and the server does not make
it: a session is a place somebody was working, and re-creating it unasked would
be replacing their terminal with a fresh one and calling it recovery.

### Case C — a tmux session that belongs to no project

The server's database says which projects have runtimes. A machine's tmux
sockets say which sessions exist. When a socket exists and the database has no
project for it — a database restored from a backup taken before the project was
created, a project deleted while its session ran — the session is reported as an
**orphan**:

```
level=WARN msg="a terminal session belongs to no registered project; it is left running" component=session
```

It is reported and left alone. It may hold work somebody needs, and killing it
would be a guess. `journalctl -u agentmux | grep orphan` finds them; the socket
path in the warning is the one to attach to if you want to see what is in it.

## 5. Configuration

`/etc/agentmux/agentmux.yaml`, written on first install and never overwritten
afterwards. Every key and its default are documented in
[`config/agentmux.example.yaml`](../../config/agentmux.example.yaml) in the
repository.

Precedence, and it is worth internalising before editing the unit:

```
CLI arguments  >  AGENTMUX_* environment  >  the config file  >  built-in defaults
```

The unit sets exactly two environment variables — `AGENTMUX_CONFIG` and
`AGENTMUX_DATA_DIR` — and the list is short on purpose. Anything set in
`Environment=` wins over the file, so a value that belongs in the file must not
also appear there: an operator who edited the file and saw no effect would be
looking at a value the unit had already overridden. Those two are there because
the file cannot carry them; the file's own location cannot be inside the file,
and the data directory is deliberately not settable from it.

After editing:

```sh
sudo systemctl restart agentmux     # the configuration is read at startup
```

The server logs what it resolved on every start, so the first thing to check
after a change is the startup line:

```sh
journalctl -u agentmux -n 5
```

**`beta.enabled` is off, and the installer writes it off.** It is the one key
this file sets that a beta might want turned on: with it on, the server records
five event types into a `usage_events` table, each row an event name and a
timestamp and nothing else. It has no column for terminal output, anything
typed, a prompt, or a project or session name, and no HTTP endpoint returns a
row. Turning it on is a decision about collecting from the people using your
deployment, which is why nothing turns it on for you — `docs/BETA_TEST.md` §10
is what it records and how to read it.

## 6. Upgrade

`install.sh` re-run **is** the upgrade. It stops the service, copies the
database aside, replaces the binary and the bundle, starts the service and
checks `/health` — the sequence in `docs/DEPLOYMENT.md` §Upgrade, performed
rather than described.

It then checks `GET /api/health` too, and a build that does not answer it is
reported rather than failed: that route arrived in 0.7.5, so a server that is
running and answering `/health` but not `/api/health` is an older binary, and
the installer says so instead of calling the deployment down.

It decides it is an upgrade from the machine — a binary under the prefix, or
the unit file — and not from whether the service happens to be running, so
re-running it on a server that is down still takes the copy. It copies
`agentmux.db` and, when they exist, the `-wal` and `-shm` with it, as one set.

```sh
git pull
sudo ./deploy/linux/install.sh
```

By hand, if you would rather:

```sh
sudo systemctl stop agentmux
sudo cp -a /var/lib/agentmux/agentmux.db      /var/lib/agentmux/agentmux.db.backup-$(date -u +%Y%m%dT%H%M%SZ)
sudo cp -a /var/lib/agentmux/agentmux.db-wal  /var/lib/agentmux/agentmux.db.backup-$(date -u +%Y%m%dT%H%M%SZ)-wal 2>/dev/null || true
sudo install -m 0755 ./agentmux-server /opt/agentmux/agentmux-server
sudo systemctl start agentmux
curl -s http://127.0.0.1:8787/health
```

`cp` is a valid backup **here and only here**, because the service is stopped
and nothing is writing. Copying the `-wal` alongside is what keeps it complete:
in WAL mode a commit lands in `agentmux.db-wal` and only reaches the database
file at a checkpoint, so a `.db` copied without it is short everything since
the last one. A clean stop checkpoints and removes the `-wal`, which is why
that line is conditional and why a one-file copy is usually right — but not
when the service was killed or the machine lost power. For a copy taken while
the server runs, use the procedure in `docs/BACKUP.md` §3; a plain `cp` of a
live SQLite database can produce a file that opens cleanly and is quietly
missing its last transaction.

Verify the upgrade took effect by comparing the version, not by trusting that
the service restarted:

```sh
curl -s http://127.0.0.1:8787/health
# {"status":"ok","version":"0.7.5","commit":"a1b2c3d4e5f6","runtime":"available"}
```

`commit` is the git revision the binary was built from. It is absent on a binary
built without the linker flags, which is the honest answer rather than a
placeholder.

## 7. Hardening

The unit ships without a sandbox. AgentMux's whole job is to run programs a
person asks for, in a terminal, as the service account — and those programs are
the user's own: a build that writes into the projects root, a test that binds a
port, an editor that keeps state in `$HOME`, and routinely a `sudo` in a pane.

A sandbox tight enough to be worth having is one that breaks some of them, and
it breaks them in the one place nobody can see the error: inside a terminal that
simply stops working. So the decision belongs to your deployment, not to this
file.

If you do want it, here is what each directive costs. Add them one at a time and
run a real workload through a pane after each.

| Directive | What it does | What it breaks |
| --- | --- | --- |
| `NoNewPrivileges=yes` | No setuid/setgid escalation | **`sudo` stops working in every terminal.** If anyone working in an AgentMux pane might need it, do not add this. |
| `ProtectSystem=strict` | Whole filesystem read-only | Everything the runtime writes. `ReadWritePaths=` for the projects root, the data directory and the home directory is the minimum, and it is easy to miss one. |
| `ProtectHome=read-only` | `$HOME` read-only | Every tool that keeps state there: `~/.npm`, `~/.cache`, `~/.gitconfig` writes, `~/.claude`. Claude Code needs to write to its own directory. |
| `PrivateTmp=yes` | A private `/tmp` per service | Anything that expects to share `/tmp` with a process outside the service. A socket or lock file there becomes invisible to an SSH session on the same machine. |
| `ProtectKernelTunables=yes` | `/proc/sys`, `/sys` read-only | Very little. This is the one on this list with no realistic cost, and a reasonable default to add first. |
| `RestrictAddressFamilies=` | Limits socket types | Binding a port from inside a pane, depending on what you allow. |

Two things are true without any of them, and are the reason the defaults are
defensible: the service runs as an unprivileged system account with no sudo
rights of its own, and it binds loopback. `docs/SECURITY.md` §1 is the full
statement of what that does and does not protect.

## 8. WSL2

The runtime is tmux, which has no native Windows build that AgentMux can drive,
so on a Windows host the server belongs **inside** the distribution. There is no
Windows-native unit and there will not be one.

Enable systemd for the distribution first, from Windows, in
`%USERPROFILE%\.wslconfig`:

```ini
[boot]
systemd=true
```

then `wsl --shutdown` and start the distribution again. Check with
`systemctl is-system-running` — it should say `running`, not
`offline` or `unknown`. `install.sh` refuses to install when it does not, and
says this.

After that the instructions above are unchanged; you run the same script inside
the distribution. Two differences are worth knowing:

- **Where the projects live.** Windows drives are mounted under `/mnt`, and a
  project under `/mnt/c/...` is slower for the runtime than one under the
  distribution's own filesystem, because every file operation crosses the 9p
  boundary. AgentMux maps the two path spaces for you either way; the speed
  difference is real and is not AgentMux's to fix.
- **Reboots and WSL.** `wsl --shutdown`, or an idle timeout, ends the
  distribution and everything in it. From AgentMux's point of view this is Case
  B above: sessions are gone, runtimes come back stopped. Windows Task Scheduler
  can start the distribution at logon if you want it up without opening a
  terminal window first.

## 9. Uninstall

```sh
sudo ./deploy/linux/install.sh --uninstall
```

It stops and removes the unit and nothing else. **Your projects, your database
and your configuration are left exactly where they are**, and it prints the
three paths so that deleting them is a decision you make deliberately rather
than a thing that happened.

If tmux sessions from the installation are still running it says so, with their
socket directory. Stopping the service detached the server; it did not end the
sessions, which is the same property that makes a restart safe. End them
individually if you want them gone:

```sh
tmux -S /var/lib/agentmux/tmux/<project-id>.sock kill-server
```

## 10. When it will not start

```sh
systemctl status agentmux
journalctl -u agentmux -n 50 --no-pager
```

| Symptom | Almost always |
| --- | --- |
| `status=203/EXEC` | The binary is not at `ExecStart`, or is not executable. |
| Binds, then `runtimeAvailable: false` | tmux is not on **the service account's** PATH. `sudo -u agentmux tmux -V` is the check; a `tmux` that works in your shell proves nothing. |
| `address already in use` | Something else holds the port — often an older AgentMux started by hand. `ss -ltnp \| grep 8787`. |
| Restarts every two seconds | The journal has the reason; it is at `error` level and near the bottom of the last attempt. |
| Sessions gone after `systemctl restart` | `KillMode` is not `process`. See §3. |

The startup log line says what was resolved — the version, the data directory,
the projects roots, the runtime mode — which is usually enough to see the
mistake without reading anything else.
