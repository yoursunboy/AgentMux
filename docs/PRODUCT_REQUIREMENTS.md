# AgentMux Product Requirements

## 1. Product definition

AgentMux is a self-hosted remote AI coding workstation for users running several AI coding projects in parallel on one server.

The user may be away from the development machine and needs to:

- see every active project;
- watch real AI terminal output;
- identify projects needing attention;
- send prompts or terminal input;
- create, register, open, and manage projects;
- switch the global CC Switch provider/model;
- continue the same terminal session from iPad, phone, or PC.

AgentMux must not depend on VS Code being open.

## 2. Folder organization

Default broad root:

```text
D:\AI\Projects
```

AgentMux supports organizational collection/group folders.

Example:

```text
D:\AI\Projects
└─ 2026 AgentMux
   ├─ Reference
   ├─ Design
   ├─ Notes
   ├─ Archive
   └─ AgentMux
      ├─ .git
      ├─ CLAUDE.md
      ├─ .claude
      ├─ docs
      └─ source...
```

The actual AgentMux project path is:

```text
D:\AI\Projects\2026 AgentMux\AgentMux
```

WSL:

```text
/mnt/d/AI/Projects/2026 AgentMux/AgentMux
```

## 3. Project registration

A registered Project is the unit of runtime management.

Each registered Project has:

```text
Project
→ persistent tmux session
→ Claude Code CLI
```

Closing the browser must not terminate the project session.

Automatic discovery may suggest candidate project directories, but explicit user registration is authoritative.

## 4. Supported server environments

### Windows

Preferred runtime:

```text
Windows
→ WSL2 Ubuntu
→ AgentMux Server
→ tmux
→ Claude Code
```

### Linux

```text
Linux
→ AgentMux Server
→ tmux
→ Claude Code
```

## 5. Client environments

Primary:

- iPad landscape browser/PWA
- desktop browser

Secondary:

- iPhone
- Android phone/tablet
- macOS browser
- Windows browser

No native mobile app is required for MVP.

## 6. Global top bar

Compact, approximately 44–52 px.

Target:

```text
● Online | Windows 11 / WSL | Claude → DeepSeek Flash | Switch ▼
```

It displays:

- server connectivity;
- host/runtime OS;
- active AI tool;
- current CC Switch provider/model;
- global switch action.

Model selection is global in the initial version.

## 7. Workspace grid

Below the global bar, all space belongs to project panels.

### Up to 3 total panels

One row, three columns.

### 4–6 total panels

Two rows, three columns.

The final panel is permanently reserved for Project Manager.

One page therefore contains at most:

- 5 active project panels;
- 1 Project Manager panel.

Additional projects use pagination.

Never degrade to 3×3 or 4×4.

## 8. Project panel

Each panel contains:

```text
Header
Real Terminal
Prompt/Input Bar
```

Header contains only:

- project name;
- status;
- maximize/focus control;
- more menu.

Avoid dashboard clutter.

## 9. Real terminal

The terminal must show original CLI/TUI output.

Preserve:

- ANSI color;
- cursor behavior;
- spinners;
- shell output;
- AI tool calls;
- permission prompts;
- menus;
- interactive selections.

Do not transform terminal output into chat cards.

## 10. Input modes

### Prompt mode

Fixed field below each project panel.

Sending a prompt writes:

```text
text + Enter
```

to the controlled terminal.

### Raw terminal mode

Focus terminal to send raw keys.

Required:

- arrows;
- Tab;
- Esc;
- Enter;
- Ctrl+C;
- paste.

Touch devices need:

```text
ESC | CTRL | TAB | ↑ | ↓ | ← | → | ENTER
```

## 11. Multi-device behavior

A single project may be open simultaneously on PC, iPad, and phone.

Terminal content is shared.

Input control is exclusive.

One project has one Controller at a time; all others are Viewers.

## 12. Controller rules

- explicit Take Control;
- atomic transfer;
- ~10 second disconnect grace period;
- no “last keyboard input wins”.

## 13. Terminal geometry

Each project owns one canonical PTY geometry.

Viewer dimensions cannot resize the PTY.

Only Controller actions may change canonical geometry.

Resize is debounced and thresholded.

Tablet software keyboard must not resize the PTY.

## 14. Scroll behavior

Terminal content is shared.

Scroll position is client-local.

When the user scrolls away from bottom:

- follow-output turns off locally;
- new output does not force-scroll;
- show a “N new lines” control.

## 15. Project Manager

Permanent final panel.

Actions:

- New Project
- Open/Register Project
- Manage Projects

### New Project

Choose:

- collection/group folder or root;
- project name;
- optional `git init`.

### Open/Register Project

Allow browsing within configured roots and explicit directory selection.

Automatic discovery may show likely project candidates.

## 16. CC Switch

Later integration must provide:

- current tool/provider;
- provider list;
- global provider switch;
- health status.

Clients must never receive API keys or credentials.

## 17. MVP exclusions

Do not include in V0.1:

- CC Switch live integration;
- Claude Hook state detection;
- token/cost/context dashboards;
- file explorer;
- Git diff UI;
- native apps;
- multi-user accounts;
- Codex;
- Gemini;
- OpenCode;
- push notifications;
- cloud synchronization.
