# Shiftlog: Claude Code Conversation History for Git

A Golang CLI that stores Claude Code conversation history as Git Notes and provides visualization.

## Claude Code Conversation File Format

Claude Code stores conversation transcripts as **JSONL** (JSON Lines) files at:
```
~/.claude/projects/<encoded-path>/<session-id>.jsonl
```

Where `<encoded-path>` replaces `/` with `-` (e.g., `/Users/deejay/workspace/foo` → `-Users-deejay-workspace-foo`)

### Entry Types

Each line is a JSON object with a `type` field. Common types:

#### `user` - User message
```json
{
  "type": "user",
  "uuid": "f782fa84-aa0a-455c-a8d3-70ddb866439e",
  "parentUuid": null,
  "sessionId": "7c6b617c-ec99-4b6a-8c4c-de0cfadc27e8",
  "timestamp": "2026-01-21T16:27:22.780Z",
  "cwd": "/Users/deejay/workspace/devcontainer-sync-cli",
  "gitBranch": "master",
  "version": "2.1.14",
  "userType": "external",
  "isSidechain": false,
  "message": {
    "role": "user",
    "content": "Your message here..."
  }
}
```

#### `assistant` - Assistant response (text, thinking, or tool_use)
```json
{
  "type": "assistant",
  "uuid": "034f4b03-34a1-4f15-9814-4838dbc034df",
  "parentUuid": "f782fa84-aa0a-455c-a8d3-70ddb866439e",
  "sessionId": "...",
  "timestamp": "...",
  "requestId": "req_011CXLqXWXNcShze8pXtVoSd",
  "message": {
    "model": "claude-opus-4-5-20251101",
    "id": "msg_01Sunzzbuit75UU3ddfmgssx",
    "type": "message",
    "role": "assistant",
    "content": [
      {"type": "thinking", "thinking": "..."},
      {"type": "text", "text": "..."},
      {"type": "tool_use", "id": "toolu_...", "name": "Glob", "input": {...}}
    ],
    "stop_reason": "end_turn",
    "usage": {
      "input_tokens": 10,
      "output_tokens": 5,
      "cache_read_input_tokens": 13539
    }
  }
}
```

#### `user` with `tool_result` - Tool execution result
```json
{
  "type": "user",
  "uuid": "0d15dfd6-c33b-4d62-a885-63a15de07171",
  "parentUuid": "034f4b03-34a1-4f15-9814-4838dbc034df",
  "sourceToolAssistantUUID": "034f4b03-34a1-4f15-9814-4838dbc034df",
  "message": {
    "role": "user",
    "content": [
      {"tool_use_id": "toolu_...", "type": "tool_result", "content": "..."}
    ]
  },
  "toolUseResult": {
    "filenames": ["..."],
    "durationMs": 44,
    "numFiles": 1,
    "truncated": false
  }
}
```

#### `system` - System events (turn duration, etc.)
```json
{
  "type": "system",
  "subtype": "turn_duration",
  "durationMs": 161634,
  "uuid": "...",
  "parentUuid": "...",
  "timestamp": "..."
}
```

#### `file-history-snapshot` - File state snapshots
```json
{
  "type": "file-history-snapshot",
  "messageId": "f782fa84-aa0a-455c-a8d3-70ddb866439e",
  "snapshot": {
    "messageId": "...",
    "trackedFileBackups": {},
    "timestamp": "..."
  },
  "isSnapshotUpdate": false
}
```

#### `summary` - Conversation summaries (after compaction)
```json
{
  "type": "summary",
  "summary": "Debug Postgres init race condition",
  "leafUuid": "8e1a8345-9147-4286-9a60-5a226e4446e8"
}
```

### Common Fields

| Field | Description |
|-------|-------------|
| `uuid` | Unique identifier for this entry |
| `parentUuid` | UUID of the parent message (forms a tree) |
| `sessionId` | Session UUID (filename without .jsonl) |
| `timestamp` | ISO 8601 timestamp |
| `cwd` | Working directory |
| `gitBranch` | Current git branch |
| `version` | Claude Code version |
| `isSidechain` | Whether this is part of a sidechain |
| `userType` | "external" for main user |
| `slug` | Human-readable session name |

---

## Overview

- **Phase 1**: Auto-capture conversations when commits are made via Claude Code hooks
- **Phase 2**: Resume Claude Code sessions from historical commits
- **Phase 3**: Web visualization of commit history with embedded conversations

## Project Structure

```
shiftlog/
├── cmd/
│   ├── root.go           # Cobra root command
│   ├── init.go           # shiftlog init
│   ├── store.go          # shiftlog store (hook handler)
│   ├── resume.go         # shiftlog resume <commit>
│   ├── serve.go          # shiftlog serve
│   └── sync.go           # shiftlog sync push/pull
├── internal/
│   ├── claude/
│   │   ├── transcript.go # JSONL parsing
│   │   ├── hooks.go      # Hook config management
│   │   └── session.go    # Session file management
│   ├── git/
│   │   ├── notes.go      # Git notes operations
│   │   ├── graph.go      # Commit graph traversal
│   │   └── repo.go       # Repository operations
│   ├── storage/
│   │   ├── compress.go   # gzip + base64
│   │   └── format.go     # StoredConversation struct
│   └── web/
│       ├── server.go     # HTTP server
│       ├── handlers.go   # API endpoints
│       └── static/       # Embedded HTML/JS/CSS
├── main.go
├── go.mod
└── Makefile
```

## Phase 1: Store Conversations on Commit

### `shiftlog init`

1. Detect git repository
2. Add PostToolUse hook to `.claude/settings.local.json`:
   ```json
   {
     "hooks": {
       "PostToolUse": [{
         "matcher": "Bash",
         "hooks": [{ "type": "command", "command": "shiftlog store", "timeout": 30 }]
       }]
     }
   }
   ```
3. Install Git hooks for automatic note syncing (see below)

### `shiftlog store` (called by hook)

1. Read PostToolUse JSON from stdin
2. Check if `tool_input.command` matches `git commit`
3. If not a commit, exit 0 silently
4. Get commit SHA via `git rev-parse HEAD`
5. Read JSONL transcript from `transcript_path`
6. Compress with gzip, encode as base64
7. Store in Git Note: `git notes --ref=refs/notes/claude-conversations add -f`

### Storage Format

```json
{
  "version": 1,
  "session_id": "abc123",
  "timestamp": "2025-02-02T10:30:00Z",
  "project_path": "/path/to/project",
  "git_branch": "feature-branch",
  "message_count": 42,
  "checksum": "sha256...",
  "transcript": "H4sIAAAA... (base64 gzipped JSONL)"
}
```

## Phase 2: Resume from Commit

### `shiftlog resume <commit>`

1. Resolve commit reference to SHA
2. Read Git Note: `git notes --ref=refs/notes/claude-conversations show <commit>`
3. Decompress and verify checksum
4. Write JSONL to Claude's expected location: `~/.claude/projects/<encoded-path>/<session-id>.jsonl`
5. Update `sessions-index.json`
6. Checkout commit: `git checkout <commit>`
7. Launch: `claude --resume <session-id>`

## Phase 3: Web Visualization

### `shiftlog serve [--port 8080]`

**API Endpoints:**
- `GET /` - Main visualization page
- `GET /api/commits` - List commits with conversation metadata
- `GET /api/commits/:sha` - Get full conversation for commit
- `GET /api/graph` - Git graph data for visualization
- `POST /api/resume/:sha` - Checkout and launch Claude session

**UI Features:**
- Git commit graph (left panel) - branches, commits, highlighting those with conversations
- Conversation viewer (right panel) - human-readable messages, collapsible tool uses
- "Resume Session" button per commit

**Tech Stack:**
- Go `net/http` + `embed` for static assets
- Vanilla JS + CSS for graph rendering (SVG)
- HTMX for dynamic content loading

## Automatic Git Notes Sync via Git Hooks

### `shiftlog init` also configures Git hooks:

**`.git/hooks/pre-push`** - Auto-push notes with commits:
```bash
#!/bin/bash
# Push claude-conversations notes alongside commits
remote="$1"
git push "$remote" refs/notes/claude-conversations 2>/dev/null || true
```

**`.git/hooks/post-merge`** - Auto-fetch notes after pull:
```bash
#!/bin/bash
# Fetch notes after merging
git fetch origin refs/notes/claude-conversations:refs/notes/claude-conversations 2>/dev/null || true
```

**`.git/hooks/post-checkout`** - Auto-fetch notes on checkout (for clone/switch):
```bash
#!/bin/bash
# Fetch notes when switching branches or after clone
git fetch origin refs/notes/claude-conversations:refs/notes/claude-conversations 2>/dev/null || true
```

### Manual fallback: `shiftlog sync push|pull`

- `push`: `git push origin refs/notes/claude-conversations`
- `pull`: `git fetch origin refs/notes/claude-conversations:refs/notes/claude-conversations`

Useful when hooks aren't installed or for explicit control.

## Dependencies

```
github.com/spf13/cobra  # CLI framework
```

All other functionality uses Go standard library:
- `compress/gzip`, `encoding/base64`, `encoding/json`
- `embed`, `net/http`, `html/template`
- `os/exec` for git commands

## Key Design Decisions

1. **Shell out to git** (not go-git) - go-git has limited notes support
2. **gzip + base64** - safe embedding in JSON, good compression for text
3. **Embedded static assets** - single binary distribution
4. **localhost-only server** - security by default
5. **Git hooks for auto-sync** - seamless note syncing with push/pull/checkout

## Verification

1. **Phase 1 Test:**
   - Run `shiftlog init` in a repo
   - Start Claude Code, make changes, commit
   - Verify: `git notes --ref=refs/notes/claude-conversations show HEAD`

2. **Phase 2 Test:**
   - Run `shiftlog resume HEAD~1`
   - Verify Claude starts with conversation history loaded

3. **Phase 3 Test:**
   - Run `shiftlog serve`
   - Open http://localhost:8080
   - Verify commit graph displays, click commit shows conversation
   - Click "Resume" and verify Claude launches

## Files to Create/Modify

- `go.mod` - Initialize module
- `main.go` - Entry point
- `cmd/*.go` - All CLI commands
- `internal/**/*.go` - Core logic
- `internal/web/static/*` - Web UI assets
- `.claude/settings.local.json` - Hook configuration (via init command)
- `.git/hooks/pre-push` - Auto-push notes
- `.git/hooks/post-merge` - Auto-fetch notes on pull
- `.git/hooks/post-checkout` - Auto-fetch notes on checkout
