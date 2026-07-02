## Context
VS Code's GitHub Copilot coding agent supports hooks via `.github/hooks/hooks.json` (must be on the default branch). The format differs from the Copilot CLI hook format (`shiftlog.json`), but the transcript format (`events.jsonl`) is identical. This means we only need to add hook file handling — the storage pipeline is fully reusable.

## Goals / Non-Goals
- Goals: Capture VS Code Copilot coding agent conversations using existing storage pipeline; auto-detect or flag-select hook format; backward compatibility with CLI hooks
- Non-Goals: Parsing VS Code's internal chat session files (`workspaceStorage/`); intercepting non-agent Copilot chat (no extension API for this); building a VS Code extension

## Decisions
- **Extend existing Copilot agent**: The VS Code coding agent uses the same transcript format as Copilot CLI. Adding a `--vscode` flag to the existing agent avoids code duplication.
- **Separate hook file format**: VS Code uses `.github/hooks/hooks.json` with `bash`/`powershell` fields, not `shiftlog.json` with a `command` field. New read/write functions handle this.
- **Auto-detection with override**: `ConfigureHooks()` checks which hook file exists; `--vscode` flag forces VS Code format.
- **All 6 lifecycle events**: VS Code hooks support `sessionStart`, `sessionEnd`, `userPromptSubmitted`, `preToolUse`, `postToolUse`, `errorOccurred`. We use `postToolUse`, `sessionStart`, and `sessionEnd`.

## Hook Format Comparison

| Aspect | Copilot CLI | VS Code Coding Agent |
|--------|-------------|---------------------|
| File | `.github/hooks/shiftlog.json` | `.github/hooks/hooks.json` |
| Command field | `"command": "..."` | `"bash": "...", "powershell": "..."` |
| Events | `postToolUse` only | All 6 lifecycle events |
| Extra fields | none | `cwd`, `env` |

## Hook stdin format
Both formats receive the same JSON on stdin:
```json
{"timestamp": N, "cwd": "...", "toolName": "...", "toolArgs": {...}}
```

The existing `ParseHookInput()` and `IsCommitCommand()` work without changes.

## Hook detection logic
```go
func (a *Agent) ConfigureHooks(repoRoot string, vscode bool) error {
    // If --vscode flag: use .github/hooks/hooks.json
    // Else if .github/hooks/hooks.json exists: VS Code mode
    // Else: CLI mode (.github/hooks/shiftlog.json)
}
```

## VS Code hook file structure
```json
{
  "version": 1,
  "hooks": {
    "postToolUse": [{
      "type": "command",
      "bash": "shiftlog store --agent=copilot",
      "powershell": "shiftlog store --agent=copilot",
      "cwd": ".",
      "timeoutSec": 30
    }],
    "sessionStart": [{
      "type": "command",
      "bash": "shiftlog session-start",
      "powershell": "shiftlog session-start",
      "timeoutSec": 10
    }],
    "sessionEnd": [{
      "type": "command",
      "bash": "shiftlog session-end",
      "powershell": "shiftlog session-end",
      "timeoutSec": 10
    }]
  }
}
```

## Risks / Trade-offs
- `.github/hooks/hooks.json` must be on the default branch — `shiftlog init --vscode` will need to warn about this
- VS Code hook format may evolve — version field provides forward compatibility
- Shared `hooks.json` with other tools — read/merge existing entries, don't overwrite

## Open Questions
- Should `shiftlog init --agent=copilot` auto-detect VS Code vs CLI, or always require `--vscode`?
- What is the exact session discovery path for VS Code coding agent sessions?
