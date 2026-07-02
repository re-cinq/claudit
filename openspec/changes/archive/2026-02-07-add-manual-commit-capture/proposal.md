# Change: Capture Conversations from Manual Git Commits

## Why
Currently, claudit only captures conversations when Claude Code makes commits via the `PostToolUse` hook. Users who make commits manually (outside of Claude's Bash tool) miss out on conversation capture entirely. This creates gaps in the conversation history and prevents teams from reviewing the AI context behind manually-committed changes.

## What Changes
- Add git `post-commit` hook to trigger `claudit store` on manual commits
- Add Claude Code `SessionStart`/`SessionEnd` hooks to track active sessions
- Implement session state tracking (active session file in `.claudit/`)
- Add conversation discovery logic to find relevant sessions for manual commits
- New `claudit store --manual` mode that discovers and stores conversations without Claude's stdin input

## Impact
- Affected specs: cli (new command mode, new hooks)
- Affected code: `cmd/store.go`, `cmd/init.go`, `internal/claude/hooks.go`, `internal/git/hooks.go`, new `internal/session/tracker.go`
- New dependencies: None (uses existing Claude Code hook system)
