# Backend Rules

- Backend is Go.
- Keep handlers thin.
- Business logic belongs in managers/services.
- All tmux calls go through `SessionBackend`.
- All OS-specific behavior goes through `HostAdapter`.
- Keep metadata in SQLite.
- Do not store unbounded terminal logs in SQLite.
- Browser disconnect must never kill project runtime.
- Project discovery must support collection/group folders.
- Explicit registered project paths are authoritative.
- Never infer that every first-level folder is a project.
- Prefer explicit errors over silent fallbacks.
