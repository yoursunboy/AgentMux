# Cross-Platform Rules

- First-class server targets are Linux and Windows + WSL2.
- Broad Windows Projects Root defaults to `D:\AI\Projects`.
- Broad WSL root defaults to `/mnt/d/AI/Projects`.
- Actual AgentMux repository is `D:\AI\Projects\2026 AgentMux\AgentMux`.
- WSL equivalent is `/mnt/d/AI/Projects/2026 AgentMux/AgentMux`.
- Paths are configurable.
- Never scatter `/mnt/d` or `D:\` assumptions through business logic.
- Use HostAdapter/PathMapper for translation.
- Do not implement fake native Windows tmux.
- Keep architecture open to future ConPTY without requiring it now.
