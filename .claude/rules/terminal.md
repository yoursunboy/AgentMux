# Terminal Rules

- Preserve raw ANSI/TUI output.
- Browser clients never attach directly to tmux.
- One project has one canonical PTY geometry.
- Viewer dimensions never resize the PTY.
- Only the current Controller may request PTY resize.
- Debounce/threshold PTY resize.
- iPad software keyboard appearance must never resize PTY.
- Scroll position is independent per client.
- Scrolling away from bottom disables follow-output only for that client.
- Prompt Bar and raw terminal input share the same controller lock.
- Multiple devices must never concurrently write to one project terminal.
