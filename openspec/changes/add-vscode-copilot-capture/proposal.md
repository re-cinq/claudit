# Change: Add VS Code Copilot Conversation Capture

## Why
VS Code's GitHub Copilot coding agent (agent mode) supports repository-level hooks via `.github/hooks/hooks.json`. Shiftlog can capture these conversations using the same storage pipeline as CLI agents. This extends shiftlog's value to teams using VS Code as their primary Copilot interface.

## What Changes
- Add VS Code hook file format support (`.github/hooks/hooks.json`) with `bash`/`powershell` fields
- Extend `shiftlog init --agent=copilot` to support `--vscode` flag for VS Code hook format
- Add hook read/write/update functions for the VS Code format in `internal/agent/copilot/hooks.go`
- Extend `shiftlog doctor` to validate VS Code hook configuration
- Reuse existing transcript parsing — VS Code coding agent uses `events.jsonl`, same as Copilot CLI

## Impact
- Affected specs: cli, cli-foundation, conversation-storage
- Affected code: `internal/agent/copilot/hooks.go`, `internal/agent/copilot/copilot.go`, `cmd/init.go`
- No new dependencies required
