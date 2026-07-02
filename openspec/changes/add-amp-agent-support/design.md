## Context
Amp is Sourcegraph's AI coding agent. Unlike Claude Code (PostToolUse hooks), Gemini CLI (AfterTool hooks), and Copilot (post-tool hooks), Amp has no per-tool hook mechanism. Its extension points (MCP servers, Skills) don't provide deterministic post-tool callbacks. Additionally, Amp stores conversations on Sourcegraph's servers (cloud-first), not locally like Claude Code or Codex.

## Goals / Non-Goals
- Goals: Support Amp alongside existing agents; shared acceptance tests; backward compatibility; transcript parsing for stream-json format
- Non-Goals: Implementing stream-json capture wrapper (Phase 2); MCP server integration (Phase 3); supporting Amp's VS Code extension mode

## Research Findings

### Amp CLI
- Install: `npm i -g @sourcegraph/amp`
- Config: `~/.config/amp/settings.json`
- Auth: `AMP_API_KEY` env var or interactive login
- Threads stored on Sourcegraph servers, synced to ampcode.com/threads
- Thread IDs: `T-<hex>` format (e.g., `T-7f395a45...`)
- Resume: `amp threads continue <threadId>`
- Modes: `smart` (Opus 4.6), `rush` (fast/cheap), `deep` (GPT-5.2 extended thinking)

### Amp Stream-JSON Format
When run with `--stream-json`, Amp outputs NDJSON to stdout:
```jsonl
{"type":"system","content":{"version":"1.0","session_id":"T-abc123"}}
{"type":"message","role":"user","content":[{"type":"text","text":"fix the bug"}]}
{"type":"message","role":"assistant","content":[{"type":"text","text":"I'll look at the code."}]}
{"type":"message","role":"assistant","content":[{"type":"tool_use","name":"terminal","input":{"command":"git diff"}}]}
{"type":"result","role":"assistant","content":[{"type":"tool_result","output":"..."}],"usage":{"input_tokens":1500,"output_tokens":200}}
```

### Amp Tool Names
Based on research, Amp uses these tool names:
- `terminal` - Shell/command execution
- `edit_file` - File editing
- `create_file` - File creation
- `search_files` - Content search (grep-like)
- `list_files` - File listing (glob-like)
- `read_file` - File reading

**Note**: Tool names need verification against a live Amp instance. The tool alias mapping should be updated if actual names differ.

## Decisions

1. **No-op ConfigureHooks**: Amp has no hook/plugin file to install. `ConfigureHooks()` returns nil immediately. Same pattern as Codex.

2. **Post-commit git hook only (Phase 1)**: Like Codex, conversation capture relies on `shiftlog store --manual` in the post-commit hook. Without a local capture file, `DiscoverSession()` returns nil and `shiftlog store --manual` stores a commit record without conversation data.

3. **Stream-JSON transcript parsing**: `ParseTranscript()` handles Amp's NDJSON format:
   - `system` type lines: extract session_id for thread ID, skip otherwise
   - `message` with `role: "user"`: map to `TranscriptEntry{Type: "user"}`
   - `message` with `role: "assistant"`: map to `TranscriptEntry{Type: "assistant"}`
   - Content blocks: `text` -> text, `tool_use` -> tool_use, `tool_result` -> tool_result, `thinking` -> thinking
   - `result` type lines: extract usage metrics (input_tokens, output_tokens, cache tokens)
   - Extract model from assistant message metadata

4. **IsCommitCommand matching**: Match tool name `terminal` (and any future aliases like `shell`, `bash`) with command containing `git commit`.

5. **Thread ID for resume**: Extract thread ID from stream-json `system` init message or from stored session metadata. Resume command: `amp threads continue <threadId>`.

6. **IsHookless test config**: Reuse `AgentTestConfig.IsHookless = true` from the Codex implementation.

## Risks / Trade-offs
- **Amp tool names unverified**: Tool names are based on research, not live testing. May need adjustment. Mitigation: fixture-based tests make updates easy.
- **Cloud-only threads**: Without the stream-json wrapper (Phase 2), there's no local transcript data. Phase 1 only records that Amp made a commit. Acceptable: same limitation as Codex Phase 1.
- **Thread ID format**: `T-<hex>` format is based on documentation. Needs verification. Mitigation: thread ID is stored as opaque string.
- **Stream-JSON schema stability**: Amp is relatively new; the stream-json format may evolve. Mitigation: fixture-based tests, version field in system init message.

## Testing Strategy
- Unit tests: transcript parsing (stream-json NDJSON), IsCommitCommand, ToolAliases, ParseHookInput
- Acceptance tests: shared framework with `AmpTestConfig()`, `IsHookless: true`
- Integration test (future): requires `AMP_API_KEY`, skippable
