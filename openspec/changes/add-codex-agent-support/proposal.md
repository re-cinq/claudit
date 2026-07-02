# Change: Add OpenAI Codex CLI Agent Support

## Why
Shiftlog supports Claude Code, Gemini CLI, and OpenCode CLI. OpenAI's Codex CLI is a growing competitor in the AI coding assistant space. Adding Codex as a fourth agent extends shiftlog's multi-agent coverage.

## What Changes
- Add Codex agent implementation (`internal/agent/codex/`) following the existing Agent interface pattern
- Add `codex` as a valid agent name constant
- Register the Codex agent in all cmd files that register agents
- Make `runManualStore()` agent-aware so the post-commit git hook discovers sessions for ALL agents (not just Claude)
- Parse Codex's JSONL rollout session format (`~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`)
- Add shared acceptance test fixtures for Codex
- Add integration test for Codex CLI

## Impact
- Affected specs: `cli`, `conversation-storage`, `session-resume`
- Affected code: `internal/agent/agent.go`, `cmd/init.go`, `cmd/store.go`, `cmd/show.go`, `cmd/resume.go`, `cmd/doctor.go`
- New package: `internal/agent/codex/`
- New test files: `internal/agent/codex/codex_test.go`, `tests/acceptance/testutil/codex_fixtures.go`, `tests/integration/codex_integration_test.go`
- **Backward compatible**: no changes to existing agent behavior

## Key Design Decisions

1. **No Codex-specific hook config**: Codex CLI has no PostToolUse-style hooks or plugin system. `ConfigureHooks()` is a no-op. Conversation capture relies entirely on the existing `post-commit` git hook (`shiftlog store --manual`).

2. **Fix manual store to be agent-aware**: Currently `runManualStore()` hardcodes Claude session discovery via `session.DiscoverSession()`. Changing it to use the configured agent's `DiscoverSession()` method unlocks post-commit support for all agents, not just Claude.

3. **Rollout JSONL parsing**: Codex uses a rollout file format where each line is `{"timestamp":"...","type":"<variant>","payload":{...}}`. We parse `response_item` payloads (messages, function calls, function call outputs) and skip `session_meta`, `turn_context`, `compacted`, and `event_msg` variants.

4. **Session discovery by CWD match**: Codex doesn't have per-project session directories. Sessions are organized by date at `~/.codex/sessions/YYYY/MM/DD/`. We scan rollout files and match by the `cwd` field in the `session_meta` line.

5. **Resume command**: `codex resume <session_id>` using the UUID from the rollout filename/SessionMeta.

6. **IsHookless test config**: New `AgentTestConfig` field `IsHookless bool` for agents that produce no hook/plugin files. Init tests verify git hooks are installed and `.shiftlog/config` exists, but actively assert no agent-specific config files were created.
