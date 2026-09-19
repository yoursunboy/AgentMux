# AgentMux Workspace

This document describes the multi-project workspace: how a project gets a panel,
where that panel sits, what happens when there are more projects than fit, and
what the browser does and does not remember.

It is written for someone who has to change this code, or to explain to a user
why their grid looks the way it does.

`docs/UI_SPEC.md` describes the workspace as it was designed. This describes it
as it was built, including the parts where the two differ and why.

## 1. Layout model

```text
┌──────────────┬──────────────┬──────────────┐
│ Project A    │ Project B    │ Project C    │
│ Terminal     │ Terminal     │ Terminal     │
├──────────────┼──────────────┼──────────────┤
│ Project D    │ Project E    │ PROJECTS     │
│ Terminal     │ Terminal     │ Manager      │
└──────────────┴──────────────┴──────────────┘
```

One page of the workspace is a grid of at most two rows. The projects fill it in
slot order and the **Project Manager is always the last cell** - on every page,
not only the final one.

The grid is a monitoring view, not a work surface. Five terminals on a desktop
are each about 65-90 columns by 15-20 rows at the grid's text size, and that is
enough to see what five projects are doing and not much more. `Focus` is where
somebody types for a while; §7 below has the measurements.

## 2. Slots

**A slot is a reservation, and the workspace is the set of projects that hold
one.** That single sentence is the whole model, and everything else follows from
it:

- a project is in the workspace exactly when its `pinnedSlot` is not null;
- adding a project to the workspace reserves it the lowest free slot;
- removing a project gives its slot up;
- the *rendered* position is the index in the resolved order, not the stored
  number, so two projects that claim one slot are still drawn in a defined
  order and the grid never has a hole in it.

`pinnedSlot` lives in the server's database, which is why a workspace survives a
reload, a second browser and a server restart with nothing to keep in step. It is
also the only piece of workspace layout the server keeps.

**Why the server and not the browser.** Which projects are open is a question two
devices have to agree on: a person who opens eight panels on their desktop
expects their tablet to show those eight, not the last eight they happened to
open there. Everything that is genuinely local - which page is showing, where a
terminal is scrolled, which one has focus - is not stored anywhere, and §11 says
why.

**Positions do not move on their own.** The order depends on reservations and on
registration order, and on nothing else:

- not on runtime state, so a project that stops does not leave its panel;
- not on the name, so two projects called "alpha" and "Alpha" are not adjacent
  by accident;
- not on when a project was last opened, which changes every time somebody
  clicks on it.

A grid that reordered itself when a runtime stopped would move the panel the
person was reading, which is the one thing a wall of terminals must never do.
`web/src/workspace/slots.test.ts` states it as a test, and the browser suite
states it as a measurement.

**"Move left" and "Move right" exchange reservations with the neighbour**, so
both projects are written and neither is renumbered. "Pin to slot N" is a single
write when N is free, and a swap when it is not.

## 3. Pages

A page holds at most two rows of cells. How many that is depends on the width,
because a terminal that cannot be read is not a panel:

| Columns | Viewport | Projects per page | One page is |
| --- | --- | --- | --- |
| 3 | ≥ 1340px | 5 | 2 rows of 3, the last cell being the manager |
| 2 | 900–1339px | 3 | 2 rows of 2 |
| 1 | < 900px | 1 | one project, with the manager on a page of its own |

Seven projects on a desktop are therefore two pages: five with the manager, then
two with the manager. On a phone they are eight: seven projects and the manager,
one per page.

**Beyond the viewport, a page is not mounted at all.** A page nobody is looking
at holds no subscriptions and no xterm instances, which is what keeps a
workspace of seven projects to five live terminals and five renderers. What it
does *not* do is stop anything: the runtimes keep running, as §10 shows.

**Focus does not subscribe twice.** Focusing a project that is already in the
grid is that panel in a larger box - one subscription, not two.

## 4. The Project Manager

The manager is a cell of the workspace, not a sidebar, not a floating button and
not a header menu. The UI spec has no permanent sidebar, and a workspace whose
control panel ate a column of every page would be a workspace with one fewer
terminal on it.

It holds two lists rather than one, and the distinction is the product's:

- **In the workspace** - the projects that hold a slot, in order, each with its
  position, move buttons and a way out;
- **Registered** - everything else AgentMux knows about, each with a way in.

Fifty registered projects and eight panels is the ordinary case. A single list
with checkmarks would say that registering a project and watching one are the
same act, and they are not.

From here a project can be created, registered from a scan, opened into the
workspace, moved, or taken out of it. There is no archive toggle, no rename, and
no delete: those are project-model operations with their own consequences, and
this panel is about what is on screen.

## 5. Grid, focus and full screen

```text
Grid        the workspace, up to five terminals and the manager
Focus       one project, filling the workspace, with a way back
Full screen the browser's own full screen, in focus
```

All three draw the same `ProjectPanel` around the same subscription. What
changes is the box it is in and the size of its text:

| Mode | Text | What it is for |
| --- | --- | --- |
| Grid | 11px | watching several projects at once |
| Focus | 12.5px | working in one of them |
| Full screen | 13px | working in one of them on a small screen |

**Nothing about the runtime changes.** No mode restarts a session, re-creates
one, or takes a new subscription. What does change is the terminal's *size*: a
panel that grows from a 67-column cell to a 189-column focus view is a real
change of pty geometry, the program inside redraws for it, and the resize goes
through the same debounce as a window resize.

The browser suite measures this by taking the Claude process's pid before
entering focus and after leaving it, and asserting it is the same number.

**Re-entering the grid takes a fresh screen.** Focus draws a new xterm - the
panel is a different box, and React cannot move a mounted terminal between
parents - so coming back subscribes again and is sent the screen as it is. That
is a snapshot, not a restart: the session, its scrollback in tmux and whatever
was running in it are untouched. The one thing it costs is the *browser's*
scroll position, which §11 explains is not worth keeping.

## 6. Responsive design

The breakpoints are not round numbers; they are the widths at which a grid panel
still holds a usable number of terminal columns. A monospace cell at the grid's
font size is about 6.6px wide, and a panel's usable width is roughly its column
minus the chrome around it:

| Viewport | Columns | Terminal | Verdict |
| --- | --- | --- | --- |
| 1920×1080 | 3 | ~92 × 40 | comfortable |
| 1440×900 | 3 | ~67 × 18 | the design target |
| 1366×768 | 3 | ~64 × 15 | the narrowest three-column layout |
| 1180×820 (tablet, landscape) | 2 | ~84 × 11 | see below |
| 1024×768 (tablet, landscape) | 2 | ~72 × 12 | see below |
| 820×1180 (tablet, portrait) | 1 | ~119 × 56 | one project, full height |
| 390×844 (phone) | 1 | ~40 × 40 | one project per page |

**Why a landscape tablet gets two columns rather than three.** At 1180px, three
columns give each terminal about 54 columns - under the sixty that a full-screen
program needs to be worth looking at - so the grid gives up the third column and
each terminal gets 84. That is a measurement, not a preference, and the browser
suite reports it.

**Why the rows are short on a tablet.** Two rows of panels in 820px of height is
about eleven rows per terminal, which is a monitoring view and not a work
surface. Focus exists for the second thing. This is the honest shape of a
two-row grid on a small screen, and the suite prints the numbers rather than
asserting a threshold that would only be a number the suite agreed with.

## 7. Terminal lifecycle

```text
page load          → the current page's panels mount, each subscribing
page change        → the old page's panels unmount and release
                     the new page's panels mount and subscribe
focus              → the grid unmounts; one panel mounts, larger
leaving focus      → the reverse
```

A panel that unmounts disposes its xterm, removes its listeners and its
ResizeObserver, and releases its subscription. A panel that mounts takes a
snapshot. Nothing in that path touches a runtime.

The subscription is per project and keyed by project id, which is what makes
`TerminalView` safe to mount and unmount: a project's terminal is a different
terminal rather than a change to the one that happened to be there.

**One WebSocket, however many panels.** The client is created once for the page
and every panel subscribes through it; the server multiplexes the subscriptions
on one connection. A workspace of seven projects opens one socket, and the
browser suite counts sockets by wrapping the page's own `WebSocket` before it
runs - because that is the only place a second socket would be visible.

## 8. Subscription lifecycle

| Where | Subscribed? | Why |
| --- | --- | --- |
| The current page's projects | yes | they are on screen |
| Another page's projects | no | nothing is drawing them |
| A project in focus | yes, once | focus is the same subscription |
| A project whose runtime is stopped | no | the server refuses it |
| A project not in the workspace | no | it has no panel |

**A hidden page keeps working.** Leaving a page releases its subscriptions and
the runtimes carry on: the session, the process in it, and tmux's scrollback are
all on the other side of the transport. Coming back asks for a screen and is
sent the terminal as it is now, which is why output produced while the page was
hidden appears when it returns. The browser suite proves it by typing into a
session while its page is hidden and looking for the text after paging back.

**Paging does not drop the connection.** It briefly looks as though it should:
React unmounts the old page's panels before mounting the new page's, so for an
instant nothing is subscribed. A client that closed its socket on "the last
release" closed it on every page change and opened another - measured, with the
WebSocket count going to three during one page turn. The socket is now kept for
a moment after the last release, so a page change is a change of subscriptions
on one connection rather than a reconnection.

## 9. What a browser cannot affect

The whole of `docs/TERMINAL.md` §12 applies, and the workspace adds two:

- **Removing a project from the workspace does not stop it.** It writes one
  column of one row. The runtime, the session and whatever is running in it are
  untouched, and the panel comes back subscribed to the same session.
- **A panel that fails to render takes only itself off the page.** Each cell is
  wrapped in a boundary, so one project's exception does not unmount the four
  terminals beside it - which would lose their scrollback as well as their
  picture. Retrying remounts the panel; it does not restart anything.

## 10. Workspace persistence

| What | Where | Why |
| --- | --- | --- |
| Which projects are in the workspace, and in what order | the server (`projects.pinned_slot`) | two devices have to agree |
| Which page was showing | `localStorage`, `agentmux.workspace.page.v1` | a property of this browser |
| Focus | nowhere | it does not survive a reload |
| Scroll position, unread marks | nowhere | see §11 |
| Terminal contents, prompts | nowhere | `docs/TERMINAL.md` §13 |

The page number is read and written defensively: a value that is not a small
non-negative integer is treated as the first page, and a storage that throws -
a private window, blocked site data - is answered the same way. A preference is
not worth a workspace that refuses to open.

## 11. Local UI state

Scroll position, follow-output, the unread marker and the terminal's focus are
held by one `TerminalView` for as long as it is mounted, which means per panel,
per browser, per moment. There is no message in the protocol that could carry
any of them, and no store in the client that holds them globally: two people
watching one project are looking at different lines, and a server that knew one
of them would have to pick it over the other's.

The consequence worth stating: **scroll position does not survive paging away
and back.** The panel is a new xterm, and the screen it is given is the current
one. That is the deliberate trade - a workspace that kept seven terminals'
scrollback alive off-screen would be paying for it in memory and in renderers
for something a person rarely wants back.

## 12. Known multi-device limitation

> **Resolved in Phase 6.** What is described below is why Phase 6 exists, and it
> is kept as the record of what Phase 5 was rather than as a description of the
> product now. A client is a viewer until it asks for the lease;
> `docs/MULTI_DEVICE.md` is the model.

Phase 5 makes a workspace visible on several devices. It does not make it safe
for them to type at once.

**Every client that can reach the server can type into every terminal it is
subscribed to**, and there is no lease, no controller and no viewer role. Two
people typing into one project interleave their keystrokes exactly as two people
typing at one keyboard would. That is not a regression - Phase 4 had the same
property - but it is the thing Phase 5 makes easy to run into, because a
workspace invites a second screen.

`docs/ROADMAP.md` has this as Phase 6: explicit controller and viewer ownership,
so the same project can be open on a PC, a tablet and a phone while exactly one
of them controls input and resize.

Until then: the workspace is as safe as the network it is on. It has no
authentication of its own, and `docs/ARCHITECTURE.md` §14 says the same thing
about the terminal.

## 13. Known issues

**This host does not reliably keep two Claude processes alive at once.** Both
Claude-hosting projects are started by the browser suite's fixture, and whether
the second survives its first seconds is a property of the machine rather than of
AgentMux: measured, one of the two is sometimes gone before it has drawn a screen.
The suites report what they actually found rather than asserting what they hoped
for — the workspace suite says `this host kept 1 of 2 Claude processes alive` and
skips the one check that needs both, and the terminal suite skips its three
Claude-facing checks with the reason.

The fixture starts them one at a time and types into a project that has not come
up again rather than giving up on it (`CLAUDE_ATTEMPTS` in `web/e2e/run.mjs`),
which is what turns most of these into a slower green run: the retry shows in the
run's own output as `attempt 1 did not come up` followed by the project coming up
on the next one. What it cannot fix is a host that will not keep both.

**The terminal suite's scroll-back check was measuring the machine rather than
the terminal.** It types `seq 1 200` into the session, waited a fixed 1500ms, and
then scrolled up with the wheel to assert that the way back to the bottom is
offered. 1500ms is several times the round trip on an idle machine and
occasionally not enough on one running five other suites, and the failure that
produced was a thirty-second wait for an element that could not appear: with a
buffer no taller than the screen there is nothing above the viewport, so the
wheel scrolls nowhere and the indicator is *correctly* absent. It now waits for
the buffer to hold more lines than the terminal can show — the condition the
wheel actually needs, and one this run's own numbers put at 186 lines of
scrollback against a 45-row screen — and reports the viewport, the pty size, the
buffer length, the scroll position and the age of the last paint and the last
output when they do not arrive.

Those numbers are printed before and after the wheel on every run, not only on
failure, so a failure that does reappear names which of the three it is: output
that never arrived, output that arrived and was not painted, or a wheel that
never moved the viewport. It also counts the terminals on the page, because the
fourth possibility — this section measuring a different terminal from the one it
typed into — is the one the other three would otherwise be mistaken for.
