# Design: Bootstrap Claudit CLI

## Overview
Claudit is a single-binary Go CLI that integrates with Claude Code via hooks to capture conversation history and store it as Git Notes. This enables conversation replay, session resumption, and web-based visualization.

## Architecture

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│   Claude Code   │────▶│  claudit store  │────▶│   Git Notes     │
│   (PostToolUse) │     │  (hook handler) │     │ (refs/notes/    │
└─────────────────┘     └─────────────────┘     │  claude-conv)   │
                                                └─────────────────┘
                                                        │
                        ┌─────────────────┐             │
                        │ claudit resume  │◀────────────┘
                        │ (restore+launch)│
                        └─────────────────┘
                                                        │
                        ┌─────────────────┐             │
                        │  claudit serve  │◀────────────┘
                        │  (web viewer)   │
                        └─────────────────┘
```

## Data Flow

### Store Flow (on commit)
1. Claude Code executes `git commit` via Bash tool
2. PostToolUse hook triggers `claudit store`
3. Hook JSON received on stdin contains `transcript_path`
4. Read JSONL transcript, compress with gzip, encode as base64
5. Attach to HEAD commit as Git Note

### Resume Flow
1. User runs `claudit resume <commit>`
2. Read Git Note, decompress transcript
3. Write JSONL to Claude's expected location
4. Update sessions-index.json
5. Checkout commit and launch `claude --resume`

### Sync Flow
Git notes require explicit push/fetch. The `claudit sync` commands are the implementation:
- `claudit sync push` - pushes notes to remote
- `claudit sync pull` - fetches notes from remote

Git hooks installed by `claudit init` simply invoke these commands:
- `pre-push` → `claudit sync push`
- `post-merge` → `claudit sync pull`
- `post-checkout` → `claudit sync pull`

Users can also run sync commands manually when needed (e.g., initial clone, troubleshooting).

## Key Decisions

### Shell out to git (not go-git)
**Decision**: Use `os/exec` to call git CLI directly.
**Rationale**: go-git has limited/buggy support for git notes. The git CLI is ubiquitous and well-tested.
**Trade-off**: Requires git to be installed (acceptable for developer tool).

### gzip + base64 encoding
**Decision**: Compress transcripts with gzip, then base64 encode for storage.
**Rationale**:
- gzip provides good compression for text (typically 5-10x)
- base64 ensures safe embedding in JSON and git notes
- Both are standard library, no dependencies

### Embedded static assets
**Decision**: Use Go's `embed` package for web UI assets.
**Rationale**: Single binary distribution, no runtime dependencies on external files.

### Localhost-only server
**Decision**: Web server binds to 127.0.0.1 by default.
**Rationale**: Security by default. Conversations may contain sensitive information.

### Hook-based capture
**Decision**: Use Claude Code's PostToolUse hook to detect commits.
**Rationale**: Non-invasive integration, no modification to Claude Code itself.
**Constraint**: 30-second timeout for hooks.

## Storage Format

```json
{
  "version": 1,
  "session_id": "abc123",
  "timestamp": "2025-02-02T10:30:00Z",
  "project_path": "/path/to/project",
  "git_branch": "feature-branch",
  "message_count": 42,
  "checksum": "sha256:...",
  "transcript": "H4sIAAAA... (base64 gzipped JSONL)"
}
```

Version field enables future format evolution without breaking existing notes.

## Package Structure

```
claudit/
├── cmd/                    # Cobra commands
│   ├── root.go            # Root command, version, help
│   ├── init.go            # claudit init
│   ├── store.go           # claudit store (hook)
│   ├── resume.go          # claudit resume
│   ├── serve.go           # claudit serve
│   └── sync.go            # claudit sync push|pull
├── internal/
│   ├── claude/            # Claude Code integration
│   │   ├── transcript.go  # JSONL parsing
│   │   ├── hooks.go       # Hook config management
│   │   └── session.go     # Session file management
│   ├── git/               # Git operations
│   │   ├── notes.go       # Git notes CRUD
│   │   ├── graph.go       # Commit graph traversal
│   │   └── repo.go        # Repository detection
│   ├── storage/           # Data encoding
│   │   ├── compress.go    # gzip + base64
│   │   └── format.go      # StoredConversation struct
│   └── web/               # Web server
│       ├── server.go      # HTTP server setup
│       ├── handlers.go    # API endpoints
│       └── static/        # Embedded HTML/JS/CSS
├── main.go
├── go.mod
└── Makefile
```

## API Design

### Web API Endpoints
| Endpoint | Method | Description |
|----------|--------|-------------|
| `/` | GET | Main visualization page |
| `/api/commits` | GET | List commits with conversation metadata |
| `/api/commits/:sha` | GET | Full conversation for a commit |
| `/api/graph` | GET | Git graph data for visualization |
| `/api/resume/:sha` | POST | Checkout and launch Claude session |

## Security Considerations

- **Transcript content**: May contain file contents, secrets accidentally pasted. Users must be aware.
- **Localhost binding**: Prevents remote access by default.
- **No authentication**: Appropriate for localhost; would need auth if exposed.

## Testing Strategy

### Outside-In Acceptance Tests (Ginkgo/Gomega)

All features are tested against the compiled `claudit` binary using real git repositories.

```
tests/
├── acceptance/
│   ├── acceptance_suite_test.go  # Ginkgo bootstrap
│   ├── init_test.go              # claudit init tests
│   ├── store_test.go             # claudit store tests
│   ├── resume_test.go            # claudit resume tests
│   ├── sync_test.go              # claudit sync tests
│   ├── serve_test.go             # claudit serve API tests
│   └── testutil/
│       ├── binary.go             # Build and run claudit binary
│       ├── git.go                # Create temp git repos
│       ├── claude_mock.go        # Mock claude CLI
│       └── fixtures.go           # Sample JSONL transcripts
```

### Test Environment Setup

Each test suite:
1. Builds `claudit` binary to temp location (once per suite)
2. Creates isolated temp directory for git repos
3. Initializes fresh git repo per test case
4. Cleans up after each test

### External Dependencies

| Dependency | Required | Notes |
|------------|----------|-------|
| Git CLI | Yes | Must be installed, used for real git operations |
| Local bare repo | No | Created in temp dir as "remote" for sync tests |
| Claude CLI | Yes | Real CLI for integration tests; skip with `CLAUDIT_SKIP_CLAUDE_TESTS=1` |
| Anthropic API | Yes | Claude CLI requires valid credentials for session resume |
| Network | Yes | Claude CLI makes API calls; git sync tests remain local |

### Real Claude CLI Integration

Acceptance tests run against the real Claude CLI to catch actual integration issues:
- Session file format compatibility
- Correct flag usage (`--resume`, `--session-id`)
- Path encoding matching Claude's expectations

```go
// Tests verify Claude actually loads the restored session
// by checking Claude's output or session state
result := runClaudit("resume", commitSHA)
Expect(result.ExitCode).To(Equal(0))

// Verify Claude was launched with correct session
// (Claude will output session info on startup)
Expect(result.Stdout).To(ContainSubstring("Resuming session"))
```

**Test Isolation Options:**

Tests must not pollute the user's real Claude config or sessions. Two approaches:

**Option A: Override HOME (Recommended)**
```go
// Claude Code uses ~/.claude for all data
// Override HOME to isolate test environment
tempHome := t.TempDir()
cmd := exec.Command("claude", "--resume", sessionID)
cmd.Env = append(os.Environ(), "HOME="+tempHome)
```
- Simple, no containers needed
- Claude Code respects `HOME` for `~/.claude` path resolution
- Tests pre-populate `$HOME/.claude/projects/...` with test data

**Option B: Containers (heavier isolation)**
```yaml
# docker-compose.test.yml
services:
  acceptance-tests:
    build: .
    environment:
      - ANTHROPIC_API_KEY
    volumes:
      - ./tests:/tests
```
- Full isolation from host system
- Better for CI environments
- Heavier setup, slower test runs

**Recommendation:** Use HOME override for local development and CI. Reserve containers for release testing if needed.

**Test Environment Requirements:**
- Claude CLI must be installed and in PATH
- Claude CLI must be authenticated (`ANTHROPIC_API_KEY` set)
- Tests that require Claude are skipped if `CLAUDIT_SKIP_CLAUDE_TESTS=1`

**CI Considerations:**
- CI environments need Claude CLI installed and `ANTHROPIC_API_KEY` secret
- Tests use isolated `HOME` directory to avoid state leakage between runs

### Sync Testing with Local Remotes

```go
// Create a bare repo as "remote"
bareRepo := createBareRepo(t)
// Clone it as "local"
localRepo := cloneRepo(t, bareRepo)
// Run claudit commands in localRepo
// Verify notes appear in bareRepo after push
```
