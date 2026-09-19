# Backing up AgentMux

AgentMux's own state is one SQLite file and one configuration file. Everything
else this document mentions is either somebody else's data or something that must
never be copied. A complete, useful backup is a few kilobytes, which is worth
saying at the start, because it is the reason there is no excuse for not having
one.

The paths below are the Linux service's defaults, from `deploy/linux/install.sh`
and `deploy/linux/agentmux.service`. A server started by hand on another platform
resolves the same names relative to its own data directory, which the startup log
line prints on every run.

## 1. What there is to back up

| Path | What it is | Needed? |
| --- | --- | --- |
| `/var/lib/agentmux/agentmux.db` | the metadata database: projects, workspace slots, one row per runtime | **Yes** |
| `/etc/agentmux/agentmux.yaml` | the configuration the service reads | **Yes** |
| your Projects Roots — `/srv/projects` by default | your own project files | Not AgentMux's to back up; see below |
| `/var/lib/agentmux/tmux/` | one tmux socket per project | No, and copying it is harmful — §2 |
| `/opt/agentmux/agentmux-server`, `/opt/agentmux/web/dist` | the binary and the built UI | No; rebuilt from source, and `install.sh` replaces both |
| `/etc/systemd/system/agentmux.service` | the systemd unit | No; regenerated from the repository's copy on every `install.sh` run |

### The database

This is the whole of what AgentMux remembers. It is SQLite, in WAL mode, and its
schema is the authoritative list of what it can hold. Four tables exist.

| Table | One row per | Holds |
| --- | --- | --- |
| `projects` | registered project | identifier, display name, the host and runtime paths, the collection folder, the workspace slot, the archived flag, timestamps |
| `project_runtime` | project that has a runtime record | backend, session name, last settled state, canonical terminal size, timestamps |
| `settings` | installation-level setting | key and value; today the only key is `server.installId`, generated on first run |
| `schema_migrations` | applied migration | version, name, time applied; two rows today |

The workspace layout is not a table of its own. "Workspace slot" is the
`projects.pinned_slot` column on the project row, and the ordering the UI shows
is derived from it. `config/agentmux.example.yaml` refers to "workspace slots" in
the singular sense of that column.

What the database does not hold, and this is the part that matters:

- **no terminal output**, at any granularity. The schema has no column for it, and
  `internal/storage` states as a boundary that terminal output is never stored
  there;
- **no prompts and no conversation content**. Nothing in the schema holds them;
- **no client or lease state** — which browser is attached, which controller holds
  a lease, where a client has scrolled to. Every one of those is a property of a
  live connection, and a stored copy would outlive the connection that made it
  true;
- **no liveness as a fact**. There is no `is_running` column. Liveness is asked of
  the runtime, which is the only thing that knows;
- **no credential**. See §2.

Restoring a backup restores the installation's identity with it, because
`server.installId` lives in the settings table and is generated once.

### The configuration file

The service reads the file named by `AGENTMUX_CONFIG`, which the unit sets to
`/etc/agentmux/agentmux.yaml`. A server started without a unit resolves its
configuration from `-config`, then `AGENTMUX_CONFIG`, then `<data-dir>/config.json`.
Back up the file the service actually reads, not the one you assume it reads — the
startup log line names it in its `configFile` field.

`install.sh` writes the configuration only when there is not one already, so this
file accumulates edits that exist nowhere else unless you keep it in your own
configuration management.

### The projects

The projects are your own files, in your own directories, and they are not
AgentMux's to back up. AgentMux stores a path to a project and never copies or
moves its contents, and it never writes into a managed project repository. Where
those paths point comes from `projects.roots` in the configuration; that is the
list telling you which directories matter, and protecting them is whatever you
already use for your own work — version control, snapshots, a backup tool that
knows nothing about AgentMux. A second backup system layered over your projects
would be a second thing to keep correct and would protect nothing more.

## 2. What not to back up, and why

### The tmux sockets

One socket per project lives at `/var/lib/agentmux/tmux/<project-id>.sock`, in a
directory AgentMux deliberately creates owner-only.

A socket is a live IPC endpoint, not data. It is a name in the filesystem that a
running tmux server is listening on, and the interesting half of that pair is the
process, which is not in any backup. Restoring the file restores nothing, because
nothing is behind it. A restored socket file is worse than no socket at all: it is
a file that looks like a runtime, and the classification of a file with no server
behind it is only reachable by asking tmux. A socket AgentMux cannot classify is
left untouched by reconciliation — so a copy of a socket buys nothing and costs a
diagnostic that has to be run down later.

There is a second reason, and it is the stronger one. The socket directory is mode
0700 because a tmux socket is a control channel: anything that can open it can
type into that project's terminal, read its output, and kill it. A backup of the
socket directory is a backup of a control channel, and a control channel that
outlives the process it belonged to is a file waiting to be misunderstood.

### Terminal output and scrollback

There is nothing on disk to back up and nothing to restore. Terminal output
exists in exactly two places: in the tmux session's own scrollback, which is
memory bounded by tmux's history limit, and in the browser that is displaying it.
Neither is disk state. Neither survives a reboot, and neither was ever in the
database or in any file AgentMux writes.

So a session that is gone is gone, and its scrollback with it. No backup
procedure in this document recovers a line of it, and no procedure could: the
bytes were never written anywhere a backup could reach. §5 covers what that means
in practice after a restore.

Conversation content is the same story for a different reason. Prompts and Claude
Code's own conversation state are not in AgentMux's database because AgentMux
never stores them. The CLI keeps its own state in its own directory — the service
account's `~/.claude` — which AgentMux neither reads nor writes. Backing that up
is the CLI's business and not this document's.

### Credentials

AgentMux holds none. There is nowhere in the configuration to put one and nothing
in the schema to store one, and the CLI keeps its own credentials in its own
files, outside AgentMux's reach. So if a backup of AgentMux contains a credential,
somebody put it there, and the response is to find out how it got in and take it
out of the running configuration — not to keep the backup.

The rule is worth stating plainly. A backup containing a credential is a liability
that outlives the thing it protects. It is copied to more places than the original,
it is kept longer than the original, and it is protected by whatever the backup
target's permissions happen to be rather than by the modes `install.sh` set on the
data directory. A credential inside a backup has to be rotated, not deleted, because
you cannot know how many copies of the file exist. Nothing in this document needs
one, so nothing in it is an excuse to add one.

## 3. Backing up the database while the server runs

The failure mode first, because it is the whole reason this section exists.

SQLite runs here in WAL mode, with `synchronous(NORMAL)`, set in the connection
string in `internal/storage/sqlite.go`. In WAL mode a commit appends to
`agentmux.db-wal` and the main database file is only brought up to date at a
checkpoint. A `cp` of `agentmux.db` alone therefore copies a file that is
internally consistent as of the last checkpoint and missing every transaction
since.

That copy opens cleanly. `PRAGMA integrity_check` says `ok`. Nothing about the file
announces that it is short of rows. You find out when you restore it, which is the
worst possible moment — the failure is silent until it is expensive. This is the
kind of broken backup that is worse than none, because it is trusted.

Three correct procedures, in order of preference.

### 3.1 `.backup` — the online backup API

```sh
sqlite3 /var/lib/agentmux/agentmux.db \
  ".backup '/var/backups/agentmux/agentmux-$(date -u +%Y%m%dT%H%M%SZ).db'"
```

This is the one to use against a live server. It copies through a read
transaction on the source, so it sees one consistent snapshot and a concurrent
writer cannot tear it. It is safe against a running writer by construction rather
than by timing, which is the property nothing else here has.

### 3.2 `VACUUM INTO` — a compacted consistent copy

```sh
sqlite3 /var/lib/agentmux/agentmux.db \
  "VACUUM INTO '/var/backups/agentmux/agentmux-$(date -u +%Y%m%dT%H%M%SZ).db'"
```

This writes a fresh database rather than copying pages, so the result is compacted
and is a single file with no `-wal` beside it. The destination must not already
exist. It is the better choice when the copy has to be small, and it makes the
check in §8 unambiguous because there is only one file to check.

### 3.3 Without sqlite3: copy `.db`, `-wal` and `-shm` together

```sh
stamp=$(date -u +%Y%m%dT%H%M%SZ)
dest=/var/backups/agentmux
cp -a /var/lib/agentmux/agentmux.db     "$dest/agentmux-${stamp}.db"
cp -a /var/lib/agentmux/agentmux.db-wal "$dest/agentmux-${stamp}.db-wal"
cp -a /var/lib/agentmux/agentmux.db-shm "$dest/agentmux-${stamp}.db-shm"
```

All three and not one, for the reasons the table below gives.

| File | What it carries | Why it is in the set |
| --- | --- | --- |
| `agentmux.db` | the database as of the last checkpoint | the copy everyone takes, and on its own the one that is silently short |
| `agentmux.db-wal` | every commit since that checkpoint | this is the part a `.db`-only copy is missing, and it is the whole failure mode above |
| `agentmux.db-shm` | the index over that WAL | SQLite rebuilds it when it is missing, so it is the least important of the three — but a `-wal` beside no `-shm` forces a recovery pass over a file you have not tested, and three files copied together are a set that describes one instant |

This is the weakest of the three procedures, and it is worth being honest about
why: three `cp` calls are three instants, and a writer that commits between them
can produce a set that disagrees with itself. The §8 check is what catches that
disagreement. Take the copies while the server is quiet if you can, and install
`sqlite3` if you can — it is one `apt-get install` away, and 3.1 needs nothing
else.

One rule for all three: write the copy somewhere other than the data directory. A
backup stored inside the directory it protects is destroyed by the same accident
that destroys the original, and a restore that replaces the data directory would
take it along.

## 4. Backing up a stopped server

This is the case where a plain `cp` is valid, and the reason is not the command.
It is that nothing is writing. A clean stop closes the database, SQLite
checkpoints the WAL and removes it on the last connection close, and every
committed transaction is then in `agentmux.db` itself. Copy that one file and you
have all of it.

```sh
sudo systemctl stop agentmux
sudo cp -a /var/lib/agentmux/agentmux.db \
           /var/backups/agentmux/agentmux-$(date -u +%Y%m%dT%H%M%SZ).db
sudo systemctl start agentmux
```

`deploy/linux/install.sh` does exactly this during an upgrade and names the copy
`agentmux.db.pre-upgrade-<UTC timestamp>`:

```sh
stamp=$(date -u +%Y%m%dT%H%M%SZ)
backup="${db}.pre-upgrade-${stamp}"
for suffix in "" "-wal" "-shm"; do
  [ -f "${db}${suffix}" ] || continue
  cp -a "${db}${suffix}" "${backup}${suffix}"
done
```

Those lines sit inside the upgrade's own branch, after `systemctl stop`, and are
skipped with a message when there is no database yet. The script's comment on them
says the copy is a cold one, which is what makes it valid, and points at the
hot-copy procedure for a backup taken without stopping anything.

The loop is §3.3 applied, and both halves of this were wrong in the first version
of the script — worth recording here because this is the document that explains
why they matter:

- **It was a `.db`-only copy.** `cp -a "$db" "$backup"`, one file, which is the
  copy this document calls out two sections up as the one that is silently short.
  It was defensible in the narrow case the script then handled — the service had
  just stopped cleanly, which checkpoints the WAL and removes it — but it was one
  step away from the failure mode, and the step was taken by the next fix.
- **The branch did not run when the service was down.** It was entered on
  `systemctl is-active`, so a server that had crashed, been stopped by hand, or
  not yet started since a reboot read as a fresh install: no stop, no copy, no
  message, and the binary replaced underneath a database written to since the
  last upgrade. That is the one case where the `-wal` is most likely to be
  present and the copy most likely to matter. It is entered now on whether an
  installation exists — a binary under the prefix, or the unit file — which is a
  question about the machine rather than about the moment, and it reports the
  number of files it copied.

The cost of this procedure is a maintenance window, and the benefit is that it is
the easiest correct backup there is: no tooling beyond `cp`, no snapshot
semantics, nothing to get wrong. If you can stop the service, stop it.

## 5. Restoring

Stop the service, move the current database aside rather than deleting it, put the
backup in place, start, verify. Then read the second half of this section, which is
the part people are surprised by.

```sh
stamp=$(date -u +%Y%m%dT%H%M%SZ)
sudo systemctl stop agentmux

# Move the current database aside rather than deleting it. If it turns out the
# restore is the wrong backup, the thing it replaced is still on the disk.
sudo mkdir -p /var/lib/agentmux/aside
for f in agentmux.db agentmux.db-wal agentmux.db-shm; do
  sudo mv "/var/lib/agentmux/$f" "/var/lib/agentmux/aside/${f}.replaced-${stamp}" 2>/dev/null || true
done

sudo install -m 0640 -o agentmux -g agentmux \
  /var/backups/agentmux/agentmux-20260919T031500Z.db \
  /var/lib/agentmux/agentmux.db

sudo systemctl start agentmux
```

The owner and mode are not decoration. `install.sh` creates the data directory as
`agentmux:agentmux` with mode 0750, and the unit's `UMask=0027` is what gives files
the server creates inside it mode 0640. The service runs as `agentmux`. A restored
database owned by root is a database the service cannot open, and it fails in a way
that reads as a corrupt database rather than as a permissions mistake. A server
started by hand instead of through the unit inherits the shell's umask, so its
database may be 0644 — one more reason to restore through the unit.

The sidecars move as a set, and that is deliberate. A database left behind by an
unclean stop has a `-wal` holding transactions that were never checkpointed. Left
beside a *different* `agentmux.db`, that WAL is a set of pages from another
database about to be replayed over this one. Moving all three together keeps the
aside copy complete and leaves the name you install onto clear.

Then verify, in this order:

```sh
journalctl -u agentmux -n 20 --no-pager
curl -s http://127.0.0.1:8787/health
curl -s http://127.0.0.1:8787/api/projects
curl -s http://127.0.0.1:8787/api/server
```

What to look for in the journal:

- the startup line carries the resolved configuration file, data directory and
  Projects Roots, which is the fastest way to see that it read what you think it
  read;
- `schema is up to date`, or `schema migrations applied` followed by the list of
  migrations that ran. A backup taken on an older release is migrated forward
  automatically on the first start;
- the reconciliation line, carrying the running, stopped and orphan counts, and one
  warning per orphan.

Then `/api/projects` should be the project list you expect, and `/api/server`
should report `runtimeAvailable: true`.

### A restored database does not restore runtimes

The database says what AgentMux knows. The tmux servers on the machine say what is
actually running. After a restore those two have been written by different
histories, and reconciliation at startup compares them and reports the difference
rather than acting on it: it never starts anything, and it never kills anything.

**Case 1 — the database names a project whose tmux session does not exist.**

Reported **stopped**. This is correct, and it is the ordinary outcome: the backup
carries a project list, and the sessions it was written alongside may be long gone.
The session is not recreated, because recreating one would replace a terminal
somebody was working in with a fresh one and call it recovery. Start it when you
want it:

```sh
curl -s -X POST http://127.0.0.1:8787/api/projects/<project-id>/runtime/start
```

**Case 2 — a tmux session exists for a project the restored database does not know.**

Reported as an **orphan** and left running:

```
level=WARN msg="a terminal session belongs to no registered project; it is left running"
```

This is also correct. The session may hold work somebody needs, and killing it
would be a guess. Find them with:

```sh
journalctl -u agentmux | grep "belongs to no registered project"
```

Each line carries the session name and the socket path. To look at what is in one:

```sh
tmux -S /var/lib/agentmux/tmux/<project-id>.sock attach
```

and to end it once you know it holds nothing you want:

```sh
tmux -S /var/lib/agentmux/tmux/<project-id>.sock kill-server
```

One thing that does not work: registering the project again does not re-adopt its
session. A project's identifier is random and fixed at registration, and both the
session name (`amx-<id>`) and the socket path (`<id>.sock`) are derived from it. The
server adopts a session whose project it knows; an orphan is by definition one whose
project it does not, and a project registered again gets a new identifier, so the
session stays an orphan. The choice is to read the session where it is or to end
it, not to hand it to a new project row.

`deploy/linux/recovery-test.sh` exercises this case against a real installation.
Case C starts a session for a project identifier the server does not know, restarts
the service, and checks that the orphan appears in the journal and is still running
afterwards. Run it on a machine you have just restored rather than on one you have
not.

## 6. What a restore does and does not get you

It gets you:

- the project list — every registered project, with its host and runtime paths, its
  collection folder and its archived flag;
- the workspace layout — which projects are pinned to which slot, and the ordering
  that follows from `projects.pinned_slot`;
- the runtime rows — for each project, its session name, its last settled state,
  and the canonical terminal size it should come back at;
- the installation's identity, from the settings table;
- the record of which schema migrations had run.

It does not get you a single byte of terminal scrollback, because none of it was
ever on disk. This is not a limitation of the restore: the backup never contained
it, no earlier backup contained it, and the database did not contain it while it was
being written. A session that is gone does not come back, and what was printed in it
does not come back with it.

Also not in it: the projects' files, which the restore neither reads nor touches,
and any record of who was attached or who held a lease, which were properties of a
connection that ended.

## 7. A schedule you could run

Back up daily, keep a bounded number of copies, write them outside the data
directory. Anything more elaborate is a backup system that needs its own backup.

`/usr/local/bin/agentmux-backup.sh`, mode 0755, owned by root:

```sh
#!/bin/sh
# A consistent copy of the AgentMux metadata database, and a bounded number of them.
set -eu

DATA_DIR=/var/lib/agentmux
DEST=/var/backups/agentmux
KEEP=14

stamp=$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p "$DEST"

# VACUUM INTO rather than cp: the server is running, so the copy is taken through
# SQLite rather than around it. See docs/BACKUP.md §3.
sqlite3 "$DATA_DIR/agentmux.db" "VACUUM INTO '$DEST/agentmux-${stamp}.db'"

# The service account owns the database and is the account a restored copy will
# have to be readable by.
chown agentmux:agentmux "$DEST/agentmux-${stamp}.db"
chmod 0640 "$DEST/agentmux-${stamp}.db"

# Bounded, not unbounded: keep the newest $KEEP and remove the rest. The names are
# UTC timestamps and -t sorts by modification time, so the two orders agree.
ls -1t "$DEST"/agentmux-*.db | tail -n +$((KEEP + 1)) | xargs -r rm -f
```

The deployment is systemd, so the timer is two small files rather than a crontab.

`/etc/systemd/system/agentmux-backup.service`:

```ini
[Unit]
Description=Back up the AgentMux metadata database

[Service]
Type=oneshot
ExecStart=/usr/local/bin/agentmux-backup.sh
```

`/etc/systemd/system/agentmux-backup.timer`:

```ini
[Unit]
Description=Daily AgentMux database backup

[Timer]
OnCalendar=daily
Persistent=true

[Install]
WantedBy=timers.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now agentmux-backup.timer
systemctl list-timers agentmux-backup.timer
```

`Persistent=true` is the line worth having on a workstation: a machine asleep at
the scheduled hour runs the job at the next boot instead of skipping it. The
crontab equivalent, if you would rather:

```cron
17 3 * * * /usr/local/bin/agentmux-backup.sh
```

Two things this deliberately does not do. It does not copy the configuration file;
if that file is not in your own configuration management, add one line to the script
to copy it beside the database. And it prunes by count rather than by age, because
a bounded number of copies is the property that matters and `KEEP` is easier to
reason about than an expiry.

## 8. Verifying a backup

A restore you have never tested is a hypothesis. The cheap check is not a restore:
it is opening the copy and counting the rows whose shape you already know.

```sh
# The check runs against a copy in a directory the account can write, because
# opening a WAL-mode database still needs a place for its shared-memory index.
cp /var/backups/agentmux/agentmux-20260919T031500Z.db /tmp/check.db

sqlite3 -readonly /tmp/check.db "PRAGMA integrity_check;"

sqlite3 -readonly /tmp/check.db "
  SELECT 'projects',          COUNT(*) FROM projects
  UNION ALL SELECT 'project_runtime',   COUNT(*) FROM project_runtime
  UNION ALL SELECT 'settings',          COUNT(*) FROM settings
  UNION ALL SELECT 'schema_migrations', COUNT(*) FROM schema_migrations;"

rm -f /tmp/check.db /tmp/check.db-wal /tmp/check.db-shm
```

If the backup was taken by the three-file method in §3.3, copy its `-wal` alongside
it under the same base name, or the check will read the database as of the last
checkpoint and under-report — the same failure the method exists to avoid:

```sh
cp /var/backups/agentmux/agentmux-<stamp>.db     /tmp/check.db
cp /var/backups/agentmux/agentmux-<stamp>.db-wal /tmp/check.db-wal
```

What the answers mean:

- `integrity_check` prints `ok`. Anything else, and the copy is one to take again
  rather than to keep;
- the four counts should be recognizable. `projects` is how many projects you had.
  `project_runtime` is at most that number, and is zero for a project that never had
  a runtime. `schema_migrations` is 2 today, for `0001_initial_schema` and
  `0002_project_runtime`. `settings` is small — the installation identifier at
  least;
- a `projects` count of zero in a database you know had projects is exactly the
  failure this check exists to catch, and it is the shape the §3 `cp` failure takes:
  a file that opens and is quietly short of rows.

The check above tells you the file is a database. Only a restore tells you it is
*your* database, so do that once, on a machine you do not mind disturbing, before
the day you need it.

## 9. Upgrade interaction

Take a backup immediately before an upgrade. `docs/DEPLOYMENT.md` §Upgrade has the
sequence; this is the one step in it that cannot be undone afterwards.

`install.sh` re-run is the upgrade, and it takes the backup for you: it stops the
service, copies the database to `agentmux.db.pre-upgrade-<UTC timestamp>` beside the
original, replaces the binary and the bundle, and starts the service again. §4 has
the lines. Because the copy is named with its timestamp, a run of upgrades leaves a
run of copies to choose from and none is overwritten by the next.

Upgrading by hand instead means the same thing in the same order: stop, copy,
replace, start, check `/health`.

The reason the backup belongs specifically here is that the schema only moves
forward. Migrations are additive, and there are no down migrations — `migrations`
says so at the top of its package documentation, on the grounds that a half-reverted
database is worse than a newer one. A newer binary migrates the database up on
start, and an older binary has no path back down, so after an upgrade the previous
release's database exists only in the copy taken before it. That copy is the whole
of the rollback, which is why it is worth taking even when the upgrade looks
routine.

Restoring is §5, including the part that surprises people: the database comes back,
and the runtimes do not.
