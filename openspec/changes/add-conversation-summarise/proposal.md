# Change: Add Conversation Summarise Command

## Why
Users want quick summaries of stored coding conversations without reading full transcripts. Shiftlog stores conversations in git notes but has no LLM access itself. By delegating to the user's coding agent in non-interactive mode, we can provide on-demand summaries with no new dependencies.

## What Changes
- Add `shiftlog summarise [ref]` command (alias `tldr`) that pipes a transcript to a coding agent and returns a summary
- Add optional `Summariser` interface for agents that support non-interactive mode (Claude Code, Codex)
- Add prompt builder that filters/truncates transcripts to fit agent context windows
- Add spinner for TTY feedback during LLM inference

## Impact
- Affected specs: cli (new capability)
- Affected code: `cmd/summarise.go`, `internal/agent/summariser.go`, `internal/agent/claude/claude.go`, `internal/agent/codex/codex.go`, `internal/cli/spinner.go`
