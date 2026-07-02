## Context
The Model Context Protocol (MCP) is a standard for AI tools to access external data. VS Code Copilot, Claude Code, and other agents support MCP servers. Shiftlog already has rich data retrieval logic in `internal/web/handlers.go` — the MCP server wraps this in JSON-RPC over stdio.

## Goals / Non-Goals
- Goals: Let MCP-aware AI tools query shiftlog conversation history; reuse existing data retrieval; simple setup via `shiftlog init --mcp`
- Non-Goals: Writable MCP tools (no creating/modifying conversations); HTTP transport; custom MCP resources or prompts

## Decisions
- **stdio transport**: MCP standard for local tools. VS Code spawns the process and communicates via stdin/stdout JSON-RPC.
- **Official Go SDK**: Use `github.com/modelcontextprotocol/go-sdk` (maintained with Google, supports 2025-11-25 MCP spec).
- **Reuse web handler logic**: The three tools map directly to existing data retrieval in `internal/web/handlers.go` (`getCommitList`, `buildNoteSet`, `getStoredOrWriteError`).
- **Three tools only**: Minimal surface area — list conversations, get conversation, list branches. More tools can be added later.
- **`--mcp` flag on init**: Generates `.vscode/mcp.json` with absolute path to shiftlog binary. Can be committed to source control.

## MCP Tools

### `shiftlog_list_conversations`
List git commits that have AI conversation history.

**Input schema:**
```json
{
  "type": "object",
  "properties": {
    "limit": {"type": "number", "default": 20},
    "offset": {"type": "number", "default": 0},
    "branch": {"type": "string", "description": "Optional branch name filter"}
  }
}
```

**Returns:** Array of `{sha, message, author, date, messageCount}` objects.

**Reuses:** `getCommitList()`, `buildNoteSet()` from `internal/web/handlers.go`

### `shiftlog_get_conversation`
Retrieve the full AI conversation for a specific commit.

**Input schema:**
```json
{
  "type": "object",
  "properties": {
    "commit_sha": {"type": "string"},
    "incremental": {"type": "boolean", "default": false}
  },
  "required": ["commit_sha"]
}
```

**Returns:** `{sha, sessionId, timestamp, messageCount, transcript, isIncremental}` object.

**Reuses:** `storage.GetStoredConversation()`, `stored.ParseTranscript()`

### `shiftlog_list_branches`
List git branches with conversation counts.

**Input schema:**
```json
{
  "type": "object",
  "properties": {}
}
```

**Returns:** Array of `{name, headSha, isCurrent, conversationCount}` objects.

**Reuses:** `git.ListBranches()`, `buildAllNoteSet()`

## Data Flow
```
VS Code Copilot Chat
  -> MCP JSON-RPC (stdio)
shiftlog mcp-server
  -> internal/mcp/server.go (tool dispatch)
  -> internal/web/handlers.go (data retrieval)
  -> internal/storage/ (conversation decompression)
  -> git notes (refs/notes/claude-conversations)
```

## Generated Configuration

`.vscode/mcp.json`:
```json
{
  "servers": {
    "shiftlog": {
      "type": "stdio",
      "command": "/absolute/path/to/shiftlog",
      "args": ["mcp-server"]
    }
  }
}
```

## Risks / Trade-offs
- New dependency (`go-sdk`) increases binary size slightly — acceptable for MCP support
- Web handler logic may need minor refactoring to decouple from HTTP response writing — extract shared data functions
- MCP server gets `repoDir` from `git.GetRepoRoot()` at startup — must be run from within a git repo

## Open Questions
- Should `shiftlog_get_conversation` return parsed messages or raw transcript?
- Should the MCP server support MCP resources (e.g., `shiftlog://conversation/{sha}`) in addition to tools?
