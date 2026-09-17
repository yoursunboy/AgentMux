# AgentMux Development Instructions

## Project identity

Project name:

```text
AgentMux
```

Actual repository / Claude working directory:

```text
D:\AI\Projects\2026 AgentMux\AgentMux
```

WSL runtime equivalent:

```text
/mnt/d/AI/Projects/2026 AgentMux/AgentMux
```

The outer directory:

```text
D:\AI\Projects\2026 AgentMux
```

is a collection/group folder that may contain Reference, Notes, Design, Archive, and other materials that are not part of the Git repository.

Do not modify files outside the actual AgentMux repository unless explicitly instructed.

## Project purpose

AgentMux is a self-hosted remote AI coding workstation.

Initial runtime:

```text
Browser
→ WebSocket
→ AgentMux Server
→ tmux
→ Claude Code CLI
```

## Core technology

- Backend: Go
- Frontend: React + TypeScript
- Terminal rendering: xterm.js
- Runtime: tmux
- Storage: SQLite
- Windows runtime: WSL2
- Linux runtime: native

## Architecture rules

1. Browser clients must never attach directly to tmux.
2. All tmux behavior must go through `SessionBackend`.
3. OS-specific behavior must go through `HostAdapter`.
4. Terminal output must remain raw and faithful to the real CLI/TUI.
5. Do not rebuild Claude output into a custom chat UI.
6. One project may have many viewers but only one input controller.
7. Client scroll position is local to each client.
8. Viewer window size must not resize the real PTY.
9. Only the active controller may request canonical PTY resize.
10. iPad software keyboard appearance must not trigger PTY resize.
11. Runtime metadata must not be stored inside user repositories.
12. Do not expose provider credentials, tokens, or API keys to clients.
13. CC Switch is external provider-management infrastructure and must be integrated through an adapter.
14. The selected provider/model is global in the initial product version.
15. Do not introduce project-specific model selection in V1.
16. Do not introduce Codex, Gemini, OpenCode, or other tools until the Claude MVP is stable.
17. Project discovery must not assume every first-level directory under `D:\AI\Projects` is a project.
18. AgentMux must support collection/group folders containing projects plus non-project materials.
19. Registered project paths are authoritative after the user selects a project.
20. Automatic project discovery may suggest candidates, but must not treat ordinary reference folders as projects.

## Project collection model

Collection root:

```text
D:\AI\Projects
```

Example:

```text
D:\AI\Projects
└─ 2026 AgentMux
   ├─ Reference
   ├─ Notes
   ├─ Design
   ├─ Archive
   └─ AgentMux   ← actual registered project
```

## Development process

Before modifying code:

1. inspect the existing repository;
2. read relevant docs/rules;
3. preserve working architecture unless there is a concrete reason to change it.

For every implementation phase:

1. implement;
2. build;
3. test;
4. run the real critical path;
5. fix introduced problems;
6. update documentation;
7. stop at the requested phase.

Do not claim completion without actual build/test evidence.

## Safety

- Never modify sibling folders outside the actual repository unless explicitly requested.
- Never delete unrelated files.
- Never use destructive Git commands unless explicitly requested.
- Never expose secrets or credentials.
- Prefer reversible operations.
