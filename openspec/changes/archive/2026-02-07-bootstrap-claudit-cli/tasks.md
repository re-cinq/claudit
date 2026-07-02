# Tasks: Bootstrap Claudit CLI

## Milestone 1: Store Conversations

**Goal:** Conversations are automatically captured and stored as Git Notes when commits are made via Claude Code. Notes sync with push/pull.

### 1.1 Foundation

- [x] **1.1.1** Initialize Go module and create project structure
  - Create `go.mod` with module path
  - Create directory structure: `cmd/`, `internal/claude/`, `internal/git/`, `internal/storage/`, `internal/web/`
  - Add dependencies:
    - `github.com/spf13/cobra` (CLI framework)
    - `github.com/onsi/ginkgo/v2` (testing framework)
    - `github.com/onsi/gomega` (assertions)
  - Verify: `go build` succeeds

- [x] **1.1.2** Set up acceptance test infrastructure
  - Create `tests/acceptance/` directory structure
  - Create `acceptance_suite_test.go` with Ginkgo bootstrap
  - Create `testutil/binary.go` - build and execute claudit binary
  - Create `testutil/git.go` - create/manage temp git repos
  - Create `testutil/fixtures.go` - sample JSONL transcripts
  - Verify: `ginkgo tests/acceptance` runs (empty suite)

- [x] **1.1.3** Implement root command with Cobra
  - Create `main.go` entry point
  - Create `cmd/root.go` with version flag
  - Verify: `claudit --version` displays version

- [x] **1.1.4** Implement git repository detection
  - Create `internal/git/repo.go`
  - Detect if cwd is inside a git repository
  - Get repository root path
  - Verify: Unit tests pass

- [x] **1.1.5** Add acceptance tests for CLI foundation
  - Test `claudit` displays help
  - Test `claudit --version` displays version
  - Verify: `ginkgo tests/acceptance` passes

### 1.2 Storage Core

- [x] **1.2.1** Implement JSONL transcript parser
  - Create `internal/claude/transcript.go`
  - Parse user, assistant, system, tool_result entries
  - Handle unknown types gracefully
  - Verify: Unit tests with sample JSONL

- [x] **1.2.2** Implement compression and encoding
  - Create `internal/storage/compress.go`
  - Implement gzip compression
  - Implement base64 encoding
  - Implement SHA256 checksum
  - Verify: Round-trip compression test

- [x] **1.2.3** Implement storage format
  - Create `internal/storage/format.go`
  - Define `StoredConversation` struct
  - JSON serialization/deserialization
  - Verify: Unit tests

- [x] **1.2.4** Implement git notes operations
  - Create `internal/git/notes.go`
  - Add note to commit
  - Read note from commit
  - List commits with notes
  - Verify: Unit tests with temp git repo

### 1.3 Store Command

- [x] **1.3.1** Implement store command
  - Create `cmd/store.go`
  - Read PostToolUse hook JSON from stdin
  - Detect git commit commands
  - Read transcript, compress, store as note
  - Verify: Unit tests with mock stdin

- [x] **1.3.2** Add acceptance tests for store command
  - Test: Pipe hook JSON with `git commit` command, verify note created
  - Test: Pipe hook JSON with non-commit command, verify silent exit
  - Test: Verify stored note contains expected metadata
  - Test: Verify transcript can be decompressed from note
  - Verify: `ginkgo tests/acceptance` passes

### 1.4 Init & Sync

- [x] **1.4.1** Implement Claude hooks configuration
  - Create `internal/claude/hooks.go`
  - Read/write `.claude/settings.local.json`
  - Merge hooks without overwriting existing config
  - Verify: Unit tests

- [x] **1.4.2** Implement sync command
  - Create `cmd/sync.go`
  - `sync push` - push notes to origin
  - `sync pull` - fetch notes from origin
  - Verify: Commands execute git push/fetch for notes ref

- [x] **1.4.3** Implement git hooks installation
  - Create `internal/git/hooks.go`
  - Install pre-push → `claudit sync push`
  - Install post-merge, post-checkout → `claudit sync pull`
  - Handle existing hooks (append, don't overwrite)
  - Verify: Hooks are executable and call claudit commands

- [x] **1.4.4** Implement init command
  - Create `cmd/init.go`
  - Configure Claude hooks
  - Install git hooks
  - Display success message
  - Verify: Command runs without error

- [x] **1.4.5** Add acceptance tests for init command
  - Test: `claudit init` in git repo creates `.claude/settings.local.json`
  - Test: Verify PostToolUse hook configuration is correct
  - Test: Verify git hooks are installed and executable
  - Test: `claudit init` outside git repo fails with error
  - Test: `claudit init` preserves existing settings
  - Verify: `ginkgo tests/acceptance` passes

- [x] **1.4.6** Add acceptance tests for sync command
  - Create `testutil/remote.go` - create local bare repos as remotes
  - Test: `claudit sync push` pushes notes to bare repo remote
  - Test: `claudit sync pull` fetches notes from bare repo remote
  - Test: Verify notes round-trip through push/pull
  - Test: Git hooks invoke `claudit sync` commands
  - Verify: `ginkgo tests/acceptance` passes

### 1.5 Milestone 1 Completion

- [x] **1.5.1** End-to-end acceptance test for store flow
  - Test: Full flow - init repo, simulate Claude hook, verify note stored
  - Test: Push/pull notes between repos
  - Verify: `ginkgo tests/acceptance` passes

- [x] **1.5.2** Create Makefile (basic)
  - `make build` - build binary
  - `make test` - run unit tests
  - `make acceptance` - run acceptance tests
  - Verify: All targets work

---

## Milestone 2: Resume Sessions

**Goal:** Users can restore and resume Claude Code sessions from any commit with a stored conversation.

### 2.1 Session Management

- [x] **2.1.1** Implement session file management
  - Create `internal/claude/session.go`
  - Compute encoded project path
  - Write JSONL to Claude's location
  - Update sessions-index.json
  - Verify: Unit tests

- [x] **2.1.2** Set up isolated Claude test environment
  - Create `testutil/claude_env.go`
  - Helper to create temp HOME directory for test isolation
  - Pre-populate `$HOME/.claude/projects/` structure for tests
  - Helper to run Claude CLI with isolated HOME
  - Skip helper when `CLAUDIT_SKIP_CLAUDE_TESTS=1` is set
  - Verify: Claude CLI runs in isolated environment without touching real config

### 2.2 Resume Command

- [x] **2.2.1** Implement resume command
  - Create `cmd/resume.go`
  - Resolve commit reference (SHA, branch, relative)
  - Read and decompress conversation from note
  - Verify checksum (warn on mismatch)
  - Restore session files to Claude location
  - Check for uncommitted changes (prompt)
  - Checkout commit
  - Launch `claude --resume`
  - Verify: Command structure in place

- [x] **2.2.2** Add acceptance tests for resume command
  - Test: `claudit resume <sha>` restores transcript to Claude location
  - Test: `claudit resume <sha>` calls `claude --resume <session-id>`
  - Test: `claudit resume` with branch name resolves correctly
  - Test: `claudit resume` with relative ref (HEAD~1) works
  - Test: `claudit resume` on commit without conversation shows error
  - Test: `claudit resume` warns about uncommitted changes
  - Verify: `ginkgo tests/acceptance` passes

### 2.3 List Command

- [x] **2.3.1** Implement list command
  - Create `cmd/list.go`
  - List commits with conversations
  - Display SHA, date, message preview
  - Verify: Command runs

- [x] **2.3.2** Add acceptance tests for list command
  - Test: `claudit list` shows commits with conversations
  - Test: `claudit list` in repo with no conversations shows empty
  - Test: Output format includes SHA, date, message
  - Verify: `ginkgo tests/acceptance` passes

### 2.4 Milestone 2 Completion

- [x] **2.4.1** End-to-end acceptance test for resume flow
  - Test: Store conversation, then resume from that commit
  - Test: Verify Claude actually loads the session
  - Verify: `ginkgo tests/acceptance` passes

---

## Milestone 3: Web Visualization

**Goal:** Users can browse commits and view conversations in a web interface, with ability to resume sessions.

### 3.1 Web Server Foundation

- [x] **3.1.1** Set up embedded static assets
  - Create `internal/web/static/` directory
  - Create basic HTML template
  - Configure Go embed
  - Verify: Assets compile into binary

- [x] **3.1.2** Implement HTTP server foundation
  - Create `internal/web/server.go`
  - Localhost binding, configurable port
  - Serve embedded assets
  - Verify: Server starts and serves index page

- [x] **3.1.3** Implement serve command
  - Create `cmd/serve.go`
  - Start server with port flag
  - Display URL in terminal
  - Auto-open browser (--no-browser flag to disable)
  - Verify: `claudit serve` starts server

### 3.2 API Endpoints

- [x] **3.2.1** Implement commits API
  - Create `internal/web/handlers.go`
  - `GET /api/commits` - list with pagination, has_conversation flag
  - `GET /api/commits/:sha` - full conversation
  - Verify: Handlers return expected JSON

- [x] **3.2.2** Implement graph API
  - Add `GET /api/graph` endpoint
  - Return commit graph structure
  - Mark commits with conversations
  - Verify: Handler returns expected JSON

- [x] **3.2.3** Implement resume API
  - Add `POST /api/resume/:sha` endpoint
  - Check for uncommitted changes (return 409 if dirty)
  - Trigger resume flow
  - Verify: Handler structure in place

- [x] **3.2.4** Add acceptance tests for serve command and APIs
  - Test: `claudit serve` starts server on default port
  - Test: `claudit serve --port 3000` uses custom port
  - Test: `GET /api/commits` returns commit list with has_conversation flag
  - Test: `GET /api/commits/:sha` returns full conversation
  - Test: `GET /api/commits/:sha` returns 404 for commit without conversation
  - Test: `GET /api/graph` returns graph structure
  - Test: `POST /api/resume/:sha` triggers resume (with real Claude CLI)
  - Verify: `ginkgo tests/acceptance` passes

### 3.3 Web UI

- [x] **3.3.1** Build commit graph UI
  - Create SVG-based graph renderer
  - Display branches and commits
  - Highlight commits with conversations
  - Implement scroll/navigation
  - Verify: Visual inspection

- [x] **3.3.2** Build conversation viewer UI
  - Create message display components
  - Style user vs assistant messages
  - Render markdown content
  - Collapsible tool uses
  - Verify: Visual inspection

- [x] **3.3.3** Integrate resume button
  - Add "Resume Session" button
  - Call resume API
  - Display status feedback
  - Verify: Visual inspection

### 3.4 Milestone 3 Completion

- [x] **3.4.1** End-to-end acceptance test for web flow
  - Test: Start server, fetch commits, view conversation, trigger resume
  - Verify: `ginkgo tests/acceptance` passes

---

## Polish (can run in parallel with any milestone)

- [ ] **P.1** Add error handling and logging
  - Consistent error messages
  - Debug logging flag (`--debug`)
  - Verify: Errors are user-friendly

- [ ] **P.2** Write README
  - Installation instructions
  - Quick start guide
  - Command reference
  - Verify: Documentation is complete

- [ ] **P.3** Final acceptance test review
  - Ensure all scenarios from specs have corresponding tests
  - Add any missing edge case tests
  - Verify: Full test coverage of specified behavior

---

## Dependencies

```
Milestone 1: Store
  1.1.1 ─► 1.1.2 ─► 1.1.3 ─► 1.1.4 ─► 1.1.5
                │
                └─► 1.2.1 ─┬─► 1.3.1 ─► 1.3.2
                    1.2.2 ─┤
                    1.2.3 ─┤
                    1.2.4 ─┘
                           │
                ┌──────────┘
                │
                └─► 1.4.1 ─┬─► 1.4.4 ─► 1.4.5
                    1.4.2 ─┤
                    1.4.3 ─┘
                           │
                           └─► 1.4.6 ─► 1.5.1 ─► 1.5.2

Milestone 2: Resume (depends on Milestone 1)
  2.1.1 ─► 2.1.2 ─► 2.2.1 ─► 2.2.2
                           │
                           └─► 2.3.1 ─► 2.3.2 ─► 2.4.1

Milestone 3: Visualize (depends on Milestone 2)
  3.1.1 ─► 3.1.2 ─► 3.1.3
                    │
                    └─► 3.2.1 ─► 3.2.2 ─► 3.2.3 ─► 3.2.4
                                 │
                                 └─► 3.3.1 ─┬─► 3.3.3 ─► 3.4.1
                                     3.3.2 ─┘
```

## External Dependencies

| Dependency | Required At | Purpose | Notes |
|------------|-------------|---------|-------|
| Git CLI | Runtime + Tests | All git operations | Must be installed on system |
| Go 1.21+ | Build | Compilation | For generics, embed improvements |
| Ginkgo CLI | Tests | Run acceptance tests | `go install github.com/onsi/ginkgo/v2/ginkgo@latest` |
| Claude CLI | Milestone 2+ | Resume sessions | Real CLI with HOME isolation; skip with `CLAUDIT_SKIP_CLAUDE_TESTS=1` |
| Anthropic API Key | Milestone 2+ | Claude CLI auth | Required for resume integration tests |

### Network Requirements

- **Milestone 1**: No network required (local bare repos for sync tests)
- **Milestone 2+**: Claude CLI makes API calls (requires network + valid API key)

### Test Isolation

Tests override `HOME` environment variable to isolate Claude config:
```go
cmd.Env = append(os.Environ(), "HOME="+tempHome)
```

## Parallelization Opportunities

- **Milestone 1**: Tasks 1.2.1-1.2.4 can be developed in parallel
- **Milestone 1**: Tasks 1.4.1-1.4.3 can be developed in parallel
- **Milestone 3**: Tasks 3.3.1 and 3.3.2 can be developed in parallel
- **Polish**: All tasks can run in parallel with any milestone
