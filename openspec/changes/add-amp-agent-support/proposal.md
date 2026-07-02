# Change: Add Amp (Sourcegraph) Agent Support

## Why
Shiftlog supports Claude Code, Copilot, Gemini CLI, Codex, and OpenCode CLI. Amp is Sourcegraph's AI coding agent that supports multiple frontier models (Claude Opus 4.6, GPT-5.2, Gemini 3, etc.). Adding Amp extends shiftlog's multi-agent coverage to another significant player in the AI coding space.

## What Changes
- Add Amp agent implementation (`internal/agent/amp/`) following the existing Agent interface pattern
- Add `amp` as a valid agent name constant
- Register the Amp agent in all cmd files that register agents
- Parse Amp's stream-JSON NDJSON format for transcript capture (when local capture file available)
- Add shared acceptance test fixtures for Amp
- Add unit tests for Amp transcript parsing

## Impact
- Affected specs: `cli`, `conversation-storage`, `session-resume`
- Affected code: `internal/agent/agent.go`, `cmd/init.go`, `cmd/store.go`, `cmd/show.go`, `cmd/resume.go`, `cmd/doctor.go`
- New package: `internal/agent/amp/`
- New test files: `internal/agent/amp/amp_test.go`, `tests/acceptance/testutil/amp_fixtures.go`
- **Backward compatible**: no changes to existing agent behavior

## Key Design Decisions

1. **No Amp-specific hook config**: Amp has no PostToolUse-style hooks or per-tool lifecycle events. `ConfigureHooks()` is a no-op, identical to the Codex pattern. Conversation capture relies on the existing `post-commit` git hook (`shiftlog store --manual`).

2. **Cloud-first threads**: Amp stores conversations on Sourcegraph's servers (ampcode.com/threads), NOT locally. This is the biggest architectural difference from Claude Code and Codex. Phase 1 works without local transcripts (like Codex). Phase 2 can add local capture via Amp's `--stream-json` flag.

3. **Stream-JSON NDJSON parsing**: Amp's `--stream-json` flag outputs structured NDJSON to stdout. The format includes system init messages, user messages, assistant messages (with text/tool_use/thinking content blocks), and result messages with usage metrics. This maps cleanly to shiftlog's `TranscriptEntry`/`ContentBlock` types.

4. **Multi-model support**: Amp routes to different models based on mode (`smart`=Claude Opus 4.6, `rush`=faster model, `deep`=GPT-5.2 Codex). The model identifier is extracted from assistant message metadata in stream-json output.

5. **Thread-based resume**: Amp uses thread IDs (format: `T-<hex>...`) for session continuity. Resume command is `amp threads continue <threadId>`.

6. **Tool name mapping**: Amp uses tool names like `terminal`, `edit_file`, `create_file`, `search_files`, `list_files`, `read_file` which map to shiftlog's canonical names.

## Research Summary

### Amp Core Facts
- **Built by**: Sourcegraph
- **Install**: `npm i -g @sourcegraph/amp` (Node.js CLI)
- **Config**: `~/.config/amp/settings.json`
- **Credentials**: `~/.local/share/amp/secrets.json`
- **Auth**: `AMP_API_KEY` env var (from ampcode.com/settings), or interactive login
- **Enterprise**: SSO via WorkOS/Okta
- **Reads**: `AGENTS.md` (also `AGENT.md` or `CLAUDE.md`) for project context

### Amp Stream-JSON Format
```bash
amp --execute "prompt" --stream-json  # outputs NDJSON to stdout
```
Message schema includes: system init, user messages (role: "user"), assistant messages (role: "assistant" with text/tool_use/thinking content), result messages with usage metrics (input_tokens, output_tokens, cache tokens).

### Session/Thread Management
- `amp threads continue [threadId]` - resume a thread
- `amp threads new` - new thread
- `amp threads fork [threadId]` - branch a thread
- Threads have IDs like `T-7f395a45...`

### Hooks / Lifecycle Events
Amp has NO PostToolUse hooks. Available extension points:
- MCP servers (JSON config in `amp.mcpServers`)
- Skills (on-demand tool bundles, invoked by LLM)
- Toolbox scripts (deprecated, migrating to Skills)
None provide deterministic post-tool callbacks.

## Phased Approach

### Phase 1: Minimal Agent (this proposal)
Hookless agent like Codex. Post-commit git hook records that Amp made a commit. No local transcript capture. `DiscoverSession()` returns nil.

### Phase 2: Stream-JSON Transcript Capture (future)
Thin wrapper or skill that runs `amp --stream-json` and tees output to a local NDJSON file at `~/.amp/shiftlog/<project-hash>/current.ndjson`. Git post-commit hook reads this file for full transcript data.

### Phase 3: MCP Server Integration (future)
Build shiftlog as an MCP server with a `shiftlog_store` tool. AGENTS.md instructs Amp to call it after every commit. Captures context through the LLM itself.
