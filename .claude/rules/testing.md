# Testing Rules

For every phase:

1. build;
2. run automated tests;
3. run the real critical path;
4. report observed results;
5. fix regressions before moving on.

Important runtime properties requiring verification:

- session persists after client disconnect;
- server restart does not unintentionally terminate tmux sessions;
- multiple sessions remain isolated;
- Viewer input is rejected;
- Controller transfer is atomic;
- Viewer resize cannot alter canonical PTY size;
- client scroll states remain independent;
- collection folders are not mistakenly registered as projects;
- explicit nested project registration works correctly.

Do not claim a phase is complete from compilation alone.
