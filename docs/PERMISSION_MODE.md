# Claude Permission Mode

How much Claude asks before it acts is now a per-project setting, chosen on the
project's card in the console and passed to Claude at launch.

```text
   the card's Permission menu ──▶ project_settings ──▶ --permission-mode <mode> ──▶ Claude
             │                            (a row per project)              (the next launch)
             └── and nothing else. No keystroke, no restart, no running agent touched.
```

This document is the long form of a deliberately small feature. §7 and §8 are
the two sections worth reading if you are about to extend it: they are the
boundary, and everything else here is inside it.

## 1. The problem it answers

Claude Code cycles its permission mode with **Shift+Tab**, inside its own
terminal UI. A person at a PC can press it. A person on an iPad or a tablet
cannot: there is no Shift and no Tab on a soft keyboard, and the console's
terminal is a screen you tap rather than a keyboard you have two of.

So the tablet got a button that *sends* Shift+Tab — `web/src/dashboard/ModeSwitch.tsx`,
the `Mode` button on a card's Runtime row. That is a real fix for the keyboard
problem and it is a different thing from what this phase built.

## 2. The two controls, and why they are not one

A card now has two controls whose names both contain the word permission, and
they do different things at different times.

| | `Mode` (Runtime row) | `Permission` (title row) |
| --- | --- | --- |
| What it is | an input device | a setting |
| What it does | sends Shift+Tab to the agent running **now** | stores the mode the **next** launch starts in |
| When it takes effect | immediately, if the agent reads the keystroke | when the agent is next started |
| Where it lives | `ModeSwitch.tsx` | `PermissionMenu.tsx` |

They are siblings rather than rivals, and neither replaces the other. `Mode` is
the keyboard moved to a screen that has none; `Permission` is what a project is
configured with. A card that renamed one to match the other would be claiming
that a keystroke is a setting, or that a setting has already applied.

The names are also load-bearing in the tests: `dashboard-terminal.mjs` and
`ProjectPanel.test.tsx` both look for a button named exactly `Mode`, unscoped,
and Playwright's strict mode throws on a second match. The new control is
`Permission` partly because of that, and mostly because it is the honest word.

## 3. The three modes

The set is closed. It is `internal/claude/permission.go`:

```go
type PermissionMode string

const (
    PermissionManual      PermissionMode = "manual"
    PermissionAcceptEdits PermissionMode = "acceptEdits"
    PermissionBypass      PermissionMode = "bypassPermissions"
)
```

| Mode | What Claude does | Why it is offered |
| --- | --- | --- |
| `manual` | asks before it acts | the default, and what every launch did before this option existed |
| `acceptEdits` | edits files without asking, asks about everything else | "stop asking me about my own project" is a real request, and it is not the same as `bypassPermissions` |
| `bypassPermissions` | does not ask | a person may genuinely want it for a project they own — which is why the setting is per project and not per installation |

The values are the CLI's own spellings, verified against Claude Code 2.1.280 on
the beta host. They are spelled here exactly as `--permission-mode` takes them
because a friendlier name would be a second vocabulary that could drift from the
one the argument is.

**The CLI accepts three more** — `auto`, `dontAsk` and `plan`. None of them is
offered, and offering one would be a product decision this build has not made.
`TestTheOfferedModesAreExactlyThree` asserts the list is exactly three values, so
adding a fourth is a test failure rather than a line somebody adds while passing.

**The mode is per project, never global.** It is precisely the decision that must
not leak from the project somebody chose it for into the next one they register.

## 4. What the setting is, and what it is not

> **It records the mode a launch is configured to start in. It does not record
> the mode any agent is running under.**

Claude reads its permission mode once, from its own command line. Nothing
afterwards changes it except Shift+Tab inside its TUI, and **no hook fires on a
Shift+Tab** — nothing AgentMux can observe reports it. So a mode stored while an
agent is running has not reached that agent, and the database does not pretend
otherwise: `migrations/0010_project_settings.sql` has one column for the mode a
launch will be given and no column for "the mode now".

That distinction is the whole of §7.

## 5. The database

One migration, `0010_project_settings.sql`, and one table:

```sql
CREATE TABLE IF NOT EXISTS project_settings (
    project_id      TEXT NOT NULL PRIMARY KEY,
    permission_mode TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
```

- **It is a table and not two columns on `projects`.** `projects` is the
  registry, and every listing in this build reads all of it. A setting is read
  by two callers — the launch path, one project at a time, and the console, all
  of them at once — and neither of them is the registry. The migration's own
  header is the long argument.
- **No row means the default.** A project nobody has configured launches under
  `manual` and never gets a row until somebody chooses something. Reading does
  not create one.
- **`permission_mode` is `TEXT` without a `CHECK`.** The vocabulary lives in
  `internal/claude`, and a constraint here would be a second copy of it. It is
  enforced where the row is written. `0009_usage_events.sql` takes the same
  position for its own vocabulary.
- **The table is additive.** Nothing existing was altered, and a database
  upgraded from any earlier phase ends up with an empty `project_settings` and
  every project on the default. `internal/storage/migrate_test.go` and
  `tasks_test.go` both assert the upgrade path, including that a project on an
  upgraded database reads as `project.ErrSettingsNotFound`.
- **It is foreign-keyed to `projects`, and it cascades.** Every other table that
  belongs to a project — `project_runtime`, `agent_events`, `tasks` — declares
  the same constraint, and for the same reason: settings for a project that does
  not exist are unreachable through every endpoint this API has, so a row left
  behind would be a row nothing could read. Nothing in this build deletes a
  project and there is no endpoint that could; the constraint is there so that the
  day something does, the schema decides the question.
  `internal/storage/projectsettings_test.go` exercises both directions — a
  setting for a project that does not exist is refused, and a project's settings
  go with it.

`DefaultPermissionMode` is `claude.PermissionManual`, and that is the same thing
as the absence of this feature: a project that has never been configured is not
a project that has been configured to ask.

## 6. The API

Two endpoints, in `internal/httpapi/settings.go`. Both answer with a named
envelope, like every other response in this API.

### `GET /api/projects/{id}/settings`

```json
{ "settings": { "permissionMode": "manual" } }
```

A project that exists and has never been configured is answered with the default
rather than a 404 or an empty object. Those are three different facts, and a
client that had to tell them apart would be a client that knew what the default
is.

An id that names no project is a `404 not_found`, so one question has one answer
whether it is asked of a project that does not exist or of a mode that does not.

### `PATCH /api/projects/{id}/settings`

```json
{ "permissionMode": "acceptEdits" }
```

The response is the same shape as the read, holding what is now stored.

| Refusal | Status | Code |
| --- | --- | --- |
| no `permissionMode` in the body, or it is `null` | 400 | `invalid_request` |
| a value that is not one of the three | 400 | `invalid_input` |

The 400 for an illegal value names the vocabulary in its message and carries
`details.field = "permissionMode"`, so a client can point at the field without
parsing prose.

**There is no mode that means "unset".** `permissionMode` is an `optionalString`
so that "not sent" and "sent as null" are told apart — a request asking to clear
the field is asking for something this resource cannot express, and saying so is
a better answer than storing the absence as a value.

### The dashboard's card carries it

`GET /api/controller` has a `settings` section on every card:

```json
"settings": { "available": true, "permissionMode": "acceptEdits" }
```

It is a value rather than a nullable object, unlike `agent` and `attention`
beside it. Those are null when a project simply has nothing in them; every
project has a permission mode. What can be missing is the *server's ability to
answer*, which is what `available` reports — and when it is false the mode is
left empty rather than filled with the default, because showing a default would
be reporting a guess as a reading.

## 7. Why there is no runtime switching

This phase deliberately does not do any of the following, and the reasons are
worth keeping together because each of them is a thing somebody will try:

- **Simulating Shift+Tab.** It is a keystroke sent on somebody's behalf into
  work that is in progress, from a menu that said it was a setting. It is also a
  *cycle*: Shift+Tab has no argument, so sending it moves the mode to whatever
  the next one is. To reach a chosen mode from an unknown one you would have to
  know where the cycle started — which is the next item.
- **Parsing Claude's TUI to see what mode it is in.** The text is Claude's, it
  changes between versions, and it is a screen rather than an API. A reader that
  guessed wrong would be wrong about the one thing this control is for.
- **Guessing the current mode.** A control that marked `manual` on a project
  configured with `acceptEdits` would be telling somebody the wrong thing about
  the next launch. There is a test asserting the menu marks what the server
  reported rather than the default.
- **`statusLine` parsing and hook extensions.** Both would be ways of observing
  a mode rather than setting one, and both are for a later phase to argue about
  on their own evidence.

> **Shift+Tab is a cycle, not a stable state API.**

That sentence is the whole of it. A state has a name you can set and a value you
can read back; a cycle has a position you can advance. Everything this phase
declines to build follows from the difference.

## 8. What saving a mode does not do

**It does not restart anything**, and the console says so:

> Permission mode updated. Restart Agent to apply.

A running agent may be in the middle of something. Killing its session because
somebody changed a preference two rows up would be the console deciding that the
preference mattered more than the work. So the mode is stored, the agent is left
alone, and the card says in as many words what would apply it.

`permission-mode.mjs` checks this against the world rather than the page: after a
save, the runtime's own `startedAt` is unchanged, its state is unchanged, the
tmux session behind it is the same session, and the card still draws it as
running.

**Any restart prompted by this setting would still go the long way round** —
Dashboard → Controller API → Runtime Manager. Nothing in the console sends a
keystroke, and no path added by this phase reaches a terminal at all: a
`PATCH` to a settings row is two statements against a local SQLite file, and
`internal/httpapi/settings.go` says so in as many words.

## 9. The launch line

`internal/claude.LaunchCommand` renders the mode as one more quoted argument:

```text
'/home/sunboy/.local/bin/claude' --session-id '0f2a…-…' --settings '/home/…/settings.json' --permission-mode 'acceptEdits'
```

- **It is quoted like every other argument.** The line is parsed by a shell
  before anything else sees it, and the rule that every argument is quoted has
  no exceptions to remember.
- **A launch with no mode is the line it always was.** The option is omitted
  rather than rendered empty, so the old behaviour is the default rather than
  something that has to be asked for. `TestALaunchWithNoModeIsTheLineItAlwaysWas`
  pins the exact string.
- **The flag appears once.** A second occurrence would let the later one be the
  one Claude read.

## 10. Why this cannot be a command injection

`permissionMode` reaches a command line that a shell parses, so the question is
fair. The answer is that the value is never a string this build accepted from
anybody:

1. **It is a closed set.** `claude.ValidPermissionMode` is a membership test
   against three constants. `manual; rm -rf /`, `$(whoami)`, `` `id` ``,
   `manual'--dangerously-skip-permissions` and `manual\n--dangerously-skip-permissions`
   are all refused — not escaped, refused — because a mode that is not one of
   three is not a mode. There is a test listing thirteen of them.
2. **The refusal is where the row is written**, `project.Service.SetPermissionMode`,
   and it is the only check. The column is `TEXT` without a constraint and the
   flag is rendered without a second check, both deliberately: one list in one
   place cannot drift from itself.
3. **Quoting is the second line of defence, not the first.** `claude.Quote`
   renders every argument as one single-quoted shell word whatever it contains,
   and `TestAModeCannotBreakOutOfItsArgument` checks that on a value that could
   never be stored.
4. **The newline is the one case quoting does not close.** A line break is legal
   inside a single-quoted shell word, and this line is not only parsed by a shell
   — it is *typed into a terminal*, where a newline is Enter. That hazard is
   closed by the vocabulary, not by the quoting, and the test says exactly that
   rather than claiming otherwise.

Adding a mode means adding a constant, and a constant cannot carry a payload.

## 11. What a client sees

`web/src/dashboard/PermissionMenu.tsx` is the whole client half.

- The button is on the title row, right-aligned, labelled `Permission`.
- Pressing it opens a `role="menu"` labelled `Claude Permission Mode`, with one
  `role="menuitemradio"` per mode, marked `●` for the one in force and `○` for
  the others. The mark is `aria-hidden`; `aria-checked` is what a screen reader
  is told.
- Choosing a mode `PATCH`es the endpoint, closes the menu, and shows the
  restart notice in a `role="status"`.
- Choosing the mode that is already in force writes nothing. A no-op request and
  a message claiming something changed would both be wrong.
- A refused save renders the server's message instead of the notice.
- When the server cannot report the setting, the button is disabled and says so.
  It does not draw the default.
- After a save the page re-reads the dashboard — `DashboardPage` passes its
  `reload` down — rather than patching the card's own copy of server state. That
  is the console's rule after every write.

## 12. Tests

| Claim | Where |
| --- | --- |
| the vocabulary is exactly three, and refuses everything else | `internal/claude/permission_test.go` |
| the launch line carries the flag, once, quoted | `internal/claude/permission_test.go` |
| a launch with no mode is byte-for-byte what it was | `internal/claude/permission_test.go` |
| the table has four columns and no fifth for "the mode now" | `internal/storage/projectsettings_test.go` |
| a setting round-trips, and re-saving keeps when it was first configured | `internal/storage/projectsettings_test.go` |
| a setting belongs to a project, and goes when it does | `internal/storage/projectsettings_test.go` |
| a project nobody configured launches `manual` | `internal/project/settings_test.go` |
| each mode stores and reads back | `internal/project/settings_test.go` |
| an illegal mode is refused, including injection attempts | `internal/project/settings_test.go` |
| a storage failure is reported rather than swallowed | `internal/project/settings_test.go` |
| an upgraded database has the table and an unconfigured project | `internal/storage/migrate_test.go`, `tasks_test.go` |
| the card carries the mode, and `available` when it cannot | `internal/controller/*_test.go` |
| both endpoints, both refusals | `internal/httpapi/*_test.go` |
| the button, the menu, the save, the notice, no restart | `web/src/dashboard/PermissionMenu.test.tsx` |
| the card marks the server's mode, and asks the page to re-read | `web/src/dashboard/ProjectPanel.test.tsx` |
| the request the client makes, and the escaping of the path | `web/src/api/client.test.ts` |
| the whole of it in a browser, against a live runtime | `web/e2e/suites/permission-mode.mjs` |

## 13. Related

- `docs/CLAUDE_RUNTIME.md` §8 — the product position this sits inside: AgentMux
  never answers a permission prompt for you. A mode decides how often you are
  asked; it never answers.
- `docs/TERMINAL_CONTROLLER.md` §9 — why the console does not type into a
  terminal on somebody's behalf.
- `docs/CONTROLLER_API.md` — the `settings` section on a card.
- `docs/ACTION_CENTER.md` — what happens when Claude *does* ask, and where the
  answer is given.
