# Change: Multi-Agent Support (Gemini CLI + OpenCode CLI)

## Why
A competitor (entire.io, $60M funded) supports Claude Code, Gemini CLI, and soon OpenCode CLI. shiftlog currently only supports Claude Code. Adding multi-agent support makes shiftlog the universal conversation-tracking tool for AI coding assistants.

## What Changes
- Extract Agent interface from hardcoded Claude Code integration
- Add Gemini CLI support (hooks, session discovery, transcript parsing)
- Add OpenCode CLI support (plugin generation, SQLite session reader)
- Add `--agent` flag to `init`, `store`, and other commands
- Add `Agent` field to config and stored conversation format
- Shared acceptance test framework with per-agent fixture swapping

## Impact
- Affected specs: `cli`, `conversation-storage`, `session-resume`
- Affected code: `internal/claude/`, `cmd/init.go`, `cmd/store.go`, `cmd/resume.go`, `cmd/show.go`, `cmd/doctor.go`, `internal/session/tracker.go`, `internal/config/`, `internal/storage/`
- New packages: `internal/agent/`, `internal/agent/claude/`, `internal/agent/gemini/`, `internal/agent/opencode/`
- **Backward compatible**: existing behavior unchanged when no `--agent` flag specified
