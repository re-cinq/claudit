# Multi-Agent Test Plan: Gemini CLI + OpenCode CLI

## Status: Draft

This document covers what we need to test, what we need to fix first, and what
credentials/setup the user needs to provide.

---

## Part 0: Implementation Fixes Required Before Testing

Research into the actual Gemini CLI and OpenCode CLI source code revealed that
our current agent implementations are based on incorrect assumptions about their
formats. These must be fixed before tests can pass.

### Gemini CLI — Format Mismatches

**1. Transcript format is single JSON, not JSONL.**

Gemini stores sessions as single JSON files at:
```
~/.gemini/tmp/{project_slug}/chats/session-{timestamp}-{sessionId}.json
```

Each file is a JSON object with a `messages` array:
```json
{
  "sessionId": "a1b2c3d4-...",
  "projectHash": "my-project",
  "startTime": "2026-02-10T14:30:00.000Z",
  "lastUpdated": "2026-02-10T14:35:22.000Z",
  "summary": "Fixed login bug",
  "directories": ["/home/user/myproject"],
  "messages": [
    {
      "id": "msg-uuid-1",
      "timestamp": "2026-02-10T14:30:01.000Z",
      "type": "user",
      "content": "Fix the login bug",
      "displayContent": "Fix the login bug"
    },
    {
      "id": "msg-uuid-2",
      "timestamp": "2026-02-10T14:30:05.000Z",
      "type": "gemini",
      "content": "I'll look at auth.ts.",
      "model": "gemini-2.0-flash",
      "tokens": { "input": 1250, "output": 340 },
      "toolCalls": [
        {
          "id": "tc-uuid-1",
          "name": "read_file",
          "args": { "file_path": "src/auth.ts" },
          "result": [{ "text": "..." }],
          "status": "success"
        }
      ]
    }
  ]
}
```

Our parser currently assumes JSONL. The `ParseTranscript` and `ParseTranscriptFile`
methods need to be rewritten to handle this single-JSON-with-messages-array format.

**2. Shell tool name is `run_shell_command`, not `shell`/`shell_exec`/etc.**

The only shell tool in Gemini CLI is `run_shell_command`. Our `shellToolNames` map
and `IsCommitCommand` check the wrong names.

Full tool name list from Gemini CLI source:
- `run_shell_command` (the ONLY shell tool)
- `read_file`, `write_file`, `replace` (edit), `glob`, `grep_search`
- `list_directory`, `read_many_files`, `google_web_search`, `web_fetch`

**3. AfterTool hook input format differs.**

Gemini sends more fields than we parse:
```json
{
  "session_id": "a1b2c3d4-...",
  "transcript_path": "/home/user/.gemini/tmp/.../session-....json",
  "cwd": "/home/user/myproject",
  "hook_event_name": "AfterTool",
  "timestamp": "2026-02-10T14:30:06.123Z",
  "tool_name": "run_shell_command",
  "tool_input": {
    "command": "git commit -m 'fix'",
    "description": "Committing changes"
  },
  "tool_response": {
    "llmContent": "...",
    "error": null
  }
}
```

Our parser handles the core fields but `tool_input.command` should still work.
The hook configuration `matcher` field uses regex matching against tool names, so
`matcher: "run_shell_command"` (not `"shell"`) is what we need.

**4. Session discovery path format.**

Session filenames use: `session-{timestamp}-{sessionId}.json` where timestamp is
ISO 8601 with colons replaced by hyphens (first 16 chars). Our `DiscoverSession`
looks for plain `*.jsonl` files — needs to look for `session-*.json` instead.

**5. ToolAliases map uses wrong names.**

Must be updated to match actual Gemini tool names.

### OpenCode CLI — Fundamental Architecture Mismatch

**1. Storage is SQLite, not JSON files.**

OpenCode stores everything in `.opencode/opencode.db` (SQLite), not in JSON files
under `~/.local/share/opencode/`. The database schema:

```sql
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    message_count INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE messages (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    role TEXT NOT NULL,          -- "user", "assistant", "tool"
    parts TEXT NOT NULL DEFAULT '[]',  -- JSON array of typed parts
    model TEXT,
    created_at INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id)
);
```

The `parts` field is a JSON string containing typed entries:
```json
[
  {"type": "text", "data": {"text": "I'll help with that."}},
  {"type": "tool_call", "data": {"id": "tc1", "name": "bash", "input": "{...}"}},
  {"type": "finish", "data": {"reason": "end_turn"}}
]
```

This means our entire session.go is wrong — we need a SQLite reader.
We'll need `modernc.org/sqlite` (pure Go, no CGO) as a dependency.

**2. No external plugin/hook system exists.**

OpenCode has no JavaScript plugin API, no `.opencode/plugins/` directory, no
hook registration mechanism. Extension is only via:
- MCP servers (configured in `.opencode.json`)
- LSP servers
- Context path files

This means our `plugin.go` (which generates a JS plugin) is fiction. We need a
completely different approach for OpenCode hooks — likely an MCP server that
shiftlog runs, or relying on the git post-commit hook exclusively (manual store
mode).

**3. The only shell tool is `bash`.**

Not `shell`, `terminal`, `execute`, `run`, or `command`. Just `bash`.

**4. Messages use `parts` array, not `content`.**

The transcript parser needs to understand the `parts` schema with typed wrappers
(`text`, `tool_call`, `tool_result`, `reasoning`, `finish`).

### Fix Priority

1. **Gemini**: Moderate fixes. The architecture is roughly right (file-based
   sessions, JSON hooks), but formats/names are wrong. Estimated: 1-2 hours.

2. **OpenCode**: Major rework. SQLite storage, no plugin system, different message
   format. The `ConfigureHooks` approach needs to be completely rethought.
   Estimated: 3-4 hours.

---

## Part 1: Acceptance Tests (No Credentials Needed)

Acceptance tests use fixture data to test shiftlog's internal handling of each
agent's formats. They validate: parsing, storage, hook handling, init, show, list.

### Fixture Data Strategy

**Do NOT invent synthetic fixtures.** Instead:

1. **Gemini**: Capture a real session JSON file by running Gemini CLI once and
   copying the file from `~/.gemini/tmp/*/chats/session-*.json`. Trim it to
   ~4 messages to keep the fixture small. Store as a Go string constant in
   `testutil/gemini_fixtures.go`.

2. **OpenCode**: Capture a real `.opencode/opencode.db` by running OpenCode once,
   then dump the relevant tables to SQL or extract messages as JSON. Store as
   fixture data in `testutil/opencode_fixtures.go`. For acceptance tests that
   don't need SQLite, we can also test the transcript-from-reader path with
   a JSONL representation of the parts format.

3. **Alternative**: If capturing real sessions is impractical before writing tests,
   generate fixtures that match the source-verified schemas documented above.
   This is acceptable ONLY because the schemas come from reading actual source
   code, not from guessing.

### Acceptance Test Matrix

Each agent needs these test scenarios (mirroring existing Claude tests):

#### Init Tests (`tests/acceptance/gemini_init_test.go`, `opencode_init_test.go`)
- `shiftlog init --agent=gemini` creates correct `.gemini/settings.json`
- Hook configuration has `AfterTool` with `matcher: "run_shell_command"`
- Hook command is `shiftlog store --agent=gemini`
- Idempotent: running init twice doesn't duplicate hooks
- Preserves existing settings.json content

- `shiftlog init --agent=opencode` — TBD based on hook strategy (may configure
  git post-commit hook only, or set up an MCP server config)

#### Store Tests (`tests/acceptance/gemini_store_test.go`, `opencode_store_test.go`)
- Hook input with `tool_name: "run_shell_command"` and `command: "git commit -m ..."`
  triggers note storage
- Hook input with non-shell tool is ignored
- Note content has correct `agent: "gemini"` field
- Transcript is compressed and stored
- Duplicate detection works (same session, same commit = skip)
- Malformed hook input logs warning

#### Show Tests
- `shiftlog show` renders Gemini transcript entries correctly
- Tool aliases map Gemini/OpenCode tool names to display names

#### Doctor Tests
- `shiftlog doctor` with agent=gemini checks `.gemini/settings.json`
- Reports missing hooks, suggests `shiftlog init --agent=gemini`

### Shared Test Infrastructure

Add to `testutil/`:

```go
// testutil/gemini_fixtures.go
func SampleGeminiTranscript() string           // Real or schema-accurate JSON
func SampleGeminiHookInput(sessionID, transcriptPath, command string) string
func SampleGeminiHookInputNonShell(sessionID string) string

// testutil/opencode_fixtures.go
func SampleOpenCodeTranscript() string         // JSONL of parts-format messages
func SampleOpenCodeHookInput(...) string       // TBD based on hook strategy
```

---

## Part 2: Integration Tests (Credentials Required)

Integration tests run the actual CLI tools and verify the full hook → store → note
pipeline works end-to-end.

### Gemini CLI Integration Test

**File**: `tests/integration/gemini_integration_test.go`

**Prerequisites**:
- Gemini CLI installed: `npm install -g @google/gemini-cli`
- Google AI API key: `GOOGLE_API_KEY` or `GEMINI_API_KEY` environment variable
- shiftlog binary built

**Skip condition**: `SKIP_GEMINI_INTEGRATION=1`

**Test flow** (mirrors `TestClaudeCodeIntegration`):
1. Create temp git repo with initial commit
2. Run `shiftlog init --agent=gemini`
3. Verify `.gemini/settings.json` has correct hooks
4. Create a test file for Gemini to commit
5. Run `gemini` CLI in non-interactive/print mode with a commit prompt
   - Need to verify: does Gemini have a `--print` or `--non-interactive` flag?
   - If not, may need to use stdin piping or a different invocation pattern
6. Verify git note was created with valid content
7. Verify note has `agent: "gemini"` field

**What you need to provide**:
- `GOOGLE_API_KEY` or `GEMINI_API_KEY` (Google AI Studio API key)
- Confirm Gemini CLI is installed in the dev environment

**Open question**: What is Gemini CLI's non-interactive mode? Claude uses
`--print`. We need to find the equivalent for Gemini, or the integration test
can't automate the full flow.

### OpenCode CLI Integration Test

**File**: `tests/integration/opencode_integration_test.go`

**Prerequisites**:
- OpenCode installed: `go install github.com/opencode-ai/opencode@latest` or npm
- API key for the LLM provider OpenCode is configured to use (likely
  `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` depending on config)
- shiftlog binary built

**Skip condition**: `SKIP_OPENCODE_INTEGRATION=1`

**Test flow**:
1. Create temp git repo with initial commit
2. Run `shiftlog init --agent=opencode`
3. Verify hook configuration (whatever approach we settle on)
4. Create a test file for OpenCode to commit
5. Run `opencode` in non-interactive mode
   - Need to verify: does OpenCode have a non-interactive mode?
   - OpenCode is primarily a TUI — may need `--non-interactive` or similar
6. Verify git note was created

**What you need to provide**:
- API key for OpenCode's configured LLM provider
- Confirm OpenCode is installed in the dev environment
- Confirm OpenCode has a non-interactive/CI mode

**Open question**: OpenCode is a TUI application. It may not have a simple
`--print` mode like Claude Code. If it doesn't, the integration test may need
to either:
- Skip the "run OpenCode and have it commit" step
- Use a different testing approach (e.g., manually trigger the hook with
  real transcript data from a prior manual session)

---

## Part 3: What You Need to Provide

### For Acceptance Tests (no external deps)
- [ ] Nothing — these run with fixture data only

### For Gemini Integration Tests
- [ ] `GOOGLE_API_KEY` — from https://aistudio.google.com/apikey
- [ ] Gemini CLI installed: `npm install -g @google/gemini-cli`
- [ ] Confirm the non-interactive invocation mode (check `gemini --help`)

### For OpenCode Integration Tests
- [ ] API key for OpenCode's LLM backend (check `.opencode.json` config)
- [ ] OpenCode installed: check install method
- [ ] Confirm non-interactive invocation mode (check `opencode --help`)

### Environment Variables Summary

| Variable | Used By | Required For |
|----------|---------|-------------|
| `ANTHROPIC_API_KEY` | Claude integration | Claude integration tests |
| `GOOGLE_API_KEY` | Gemini integration | Gemini integration tests |
| `SKIP_GEMINI_INTEGRATION` | Test runner | Set to `1` to skip |
| `SKIP_OPENCODE_INTEGRATION` | Test runner | Set to `1` to skip |
| `SHIFTLOG_BINARY` | All integration | Path to shiftlog binary (optional) |

---

## Part 4: Implementation Order

1. **Fix Gemini agent implementation** — correct transcript format, tool names,
   session paths, hook config
2. **Write Gemini acceptance tests** — using source-verified fixture data
3. **Run acceptance tests** — verify everything passes
4. **Fix OpenCode agent implementation** — add SQLite support, rethink hook
   strategy, correct message format
5. **Write OpenCode acceptance tests**
6. **Write integration tests** for both (once credentials and CLI tools available)
7. **Update CI** — add integration test jobs with secrets

---

## Appendix: Source References

- Gemini CLI source: https://github.com/google-gemini/gemini-cli
  - Session format: `packages/core/src/services/chatRecordingService.ts`
  - Tool names: `packages/core/src/tools/tool-names.ts`
  - Hook system: `packages/core/src/hooks/hookEventHandler.ts`
  - Settings: `packages/core/src/settings/settings.ts`

- OpenCode source: https://github.com/opencode-ai/opencode
  - DB schema: `internal/db/migrations/20250424200609_initial.sql`
  - Message format: `internal/message/message.go`
  - Tool names: `internal/llm/tools/bash.go` (and siblings)
  - Config: `internal/config/config.go`
