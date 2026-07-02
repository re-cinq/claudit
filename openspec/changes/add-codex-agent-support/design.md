## Context
Codex CLI is OpenAI's coding agent. Unlike Claude Code (PostToolUse hooks) and Gemini CLI (AfterTool hooks), Codex has no per-tool hook mechanism. Its `notify` config only fires per-turn, not per-tool-use. Rather than using a fragile, per-turn notification, we rely on the **existing `post-commit` git hook** that shiftlog already installs for all agents.

## Goals / Non-Goals
- Goals: Support Codex CLI alongside Claude Code, Gemini CLI, and OpenCode CLI; shared acceptance tests; backward compatibility
- Non-Goals: Using Codex's `notify` mechanism; supporting Codex sandbox/container mode transcript locations

## Research Findings

### Codex CLI
- Sessions: JSONL rollout files at `~/.codex/sessions/YYYY/MM/DD/rollout-<timestamp>-<uuid>.jsonl`
- First line: `session_meta` with `id`, `cwd`, `cli_version`, `model_provider`
- Subsequent lines: `response_item` (messages, function_call, function_call_output), `turn_context`, `compacted`, `event_msg`
- Tool names for shell: `container.exec`, `shell`, `shell_command`
- No PostToolUse-style hooks — `notify` config fires per-turn, not per-tool
- Resume: `codex resume <session_id>`

### Codex Rollout JSONL Format (from codex-rs/protocol)
```jsonl
{"timestamp":"...","type":"session_meta","payload":{"id":"<uuid>","timestamp":"...","cwd":"/path","originator":"cli","cli_version":"1.0.0","source":"cli","model_provider":"openai"}}
{"timestamp":"...","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"..."}]}}
{"timestamp":"...","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"..."}]}}
{"timestamp":"...","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"git\",\"commit\",\"-m\",\"msg\"]}","call_id":"call_1"}}
{"timestamp":"...","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"..."}}
```

## Decisions

1. **No-op ConfigureHooks**: Codex has no hook/plugin file to install. `ConfigureHooks()` returns nil immediately.

2. **Agent-aware manual store**: The critical fix is in `cmd/store.go:runManualStore()`. Currently it calls `session.DiscoverSession()` which is hardcoded to Claude. Change to:
   - Resolve agent from `--agent` flag or config
   - Call `ag.DiscoverSession(projectPath)` instead
   - Fall back to `session.DiscoverSession()` if agent discovery returns nil (backward compat for Claude's active-session.json)

3. **Session discovery**: Scan `~/.codex/sessions/` recursively for `.jsonl` files modified within 5 minutes, read first line for `session_meta`, match `cwd` to `projectPath`.

4. **CODEX_HOME env var**: Support `$CODEX_HOME` override (defaulting to `~/.codex`), following Codex CLI convention.

5. **IsHookless AgentTestConfig field**: New boolean for agents that produce no hook/plugin config. Allows init tests to:
   - Verify git hooks are installed
   - Verify `.shiftlog/config.json` has correct agent
   - Assert no spurious config files were created

## Risks / Trade-offs
- Codex session format may change (it's relatively new) → Mitigation: fixture-based tests, version field in session_meta
- CWD matching may fail with symlinks → Mitigation: use `filepath.EvalSymlinks` like existing `pathsEqual`
- No hook-based capture means conversations are only stored on commit → Acceptable: same as post-commit for all agents

## Testing Strategy
- Unit tests: transcript parsing, IsCommitCommand, ToolAliases
- Acceptance tests: shared framework with `CodexTestConfig()`, `IsHookless: true`
- Integration test: requires `OPENAI_API_KEY`, skippable with `SKIP_CODEX_INTEGRATION=1`
