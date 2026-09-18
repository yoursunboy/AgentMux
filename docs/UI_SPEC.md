# AgentMux UI Specification

> **As built.** This is the design document, and it is left as it was written.
> The workspace it describes — the grid, Focus, full screen, the Project Manager
> in the last cell, one project per page on a phone — is implemented, and
> `docs/WORKSPACE.md` describes it as built, including the places where the two
> differ and why. Today's persistent state is `docs/ROADMAP.md`.

## 1. Design language

AgentMux should feel like a compact modern IDE and control room, not an enterprise dashboard.

Principles:

- terminal-first;
- dense but readable;
- minimal chrome;
- dark-mode first;
- touch-friendly;
- iPad landscape first.

## 2. Global bar

Fixed top bar, approximately 44–52 px.

```text
● Online | Windows 11 / WSL | Claude → DeepSeek Flash | Switch ▼
```

No permanent second navigation bar.

## 3. Main workspace

Everything below the global bar belongs to project panels.

No permanent left sidebar.

## 4. Grid

### Up to 3 total panels

```text
┌────────────┬────────────┬────────────┐
│     A      │     B      │  Projects  │
└────────────┴────────────┴────────────┘
```

### 4–6 total panels

```text
┌────────────┬────────────┬────────────┐
│     A      │     B      │     C      │
├────────────┼────────────┼────────────┤
│     D      │     E      │  Projects  │
└────────────┴────────────┴────────────┘
```

Maximum active projects per workspace page:

```text
5
```

## 5. Project panel

```text
┌────────────────────────────┐
│ Image Compressor       ● ⛶ │
│ Running                    │
├────────────────────────────┤
│                            │
│     REAL TERMINAL          │
│                            │
├────────────────────────────┤
│ Message Claude...       ➤  │
└────────────────────────────┘
```

## 6. Terminal presentation

Use xterm.js.

Do not reinterpret terminal output.

Suggested font ranges:

Grid:

```text
9.5–11 px
```

Focus:

```text
11–12.5 px
```

Full screen:

```text
12–14 px
```

## 7. Display modes

- Grid = monitoring
- Focus = interaction
- Full screen = sustained terminal work

Changing modes never restarts the session.

## 8. Project state

V0.1:

```text
● Running
○ Stopped
◌ Reconnecting
```

Later:

```text
⚠ Waiting
✓ Completed
× Error
```

Do not reorder panels automatically.

## 9. Prompt bar

Always visible at bottom.

Long prompts may open a larger composer.

## 10. Terminal raw-input mode

Touch toolbar:

```text
ESC | CTRL | TAB | ↑ | ↓ | ← | → | ENTER
```

Viewer trying to type sees:

```text
Controlled by PC
[Take Control]
```

**As built in Phase 6, and this sketch is the one place the product deliberately reads differently.**
The label is the device's own name rather than "PC" — derived from the User-Agent, and shown to
everyone else as `Chrome on Windows` or `Safari on iPad` — and the button is **Request control**, not
*Take Control*. §十一 of the phase directive is explicit about the second: on a project somebody else
is typing into, the terminal is not taken, and a label that says it can be would be wrong about what
the button does. The viewer is told why it cannot type in the Prompt Bar's own placeholder, and the
touch keys it has no other way to send are disabled alongside it. `docs/MULTI_DEVICE.md` §14 is the
whole of what the interface shows.

## 11. Project Manager panel

```text
┌────────────────────────────┐
│ PROJECTS                   │
│                            │
│          + New             │
│                            │
│     Open / Register        │
│                            │
│      Manage Projects       │
└────────────────────────────┘
```

## 12. New Project dialog

Support choosing a collection/group.

Example:

```text
Collection
D:\AI\Projects\2026 AgentMux

Project Name
AgentMux

Final Path
D:\AI\Projects\2026 AgentMux\AgentMux

☑ Initialize Git

[Cancel] [Create]
```

## 13. Open / Register Project

Show:

- registered projects;
- discovered candidate projects;
- browse/select directory.

Example:

```text
2026 AgentMux
└─ AgentMux

Client Work
└─ BankSystem

Experiments
└─ ImageCompressor
```

Do not display ordinary Reference/Notes folders as projects unless manually selected.

## 14. Mobile

Phone layout uses one project panel per page.

Do not attempt multi-column terminals on phones.

## 15. Reconnect UX

Preserve the last terminal frame.

Show:

```text
Reconnecting…
```

then resynchronize and resume live output.
