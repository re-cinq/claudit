# Change: Add Terminal Conversation Viewer

## Why
We currently have `claudit serve` that starts a webserver with an interactive UI for viewing conversation history for a given commit. However, launching a browser adds friction to the development/testing cycle. A terminal-based viewer would enable faster iteration when debugging or reviewing conversation history.

## What Changes
- Add `claudit show <ref>` command to display conversation history in the terminal
- Non-interactive output suitable for piping and quick review
- Future: Interactive terminal UI (separate task)

## Impact
- Affected specs: cli (new capability)
- Affected code: `cmd/show.go`, `internal/claude/render.go`
