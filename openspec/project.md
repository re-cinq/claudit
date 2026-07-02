# Project Context

## Purpose
Shiftlog is a Golang CLI that stores Claude Code conversation history as Git Notes and provides visualization. It enables teams to:
- Auto-capture AI conversations when commits are made
- Resume Claude Code sessions from historical commits
- Visualize commit history with embedded conversations

## Tech Stack
- Go (Golang) for CLI implementation
- Cobra for CLI framework
- Go standard library for compression, HTTP, templates
- Shell-based git operations (not go-git, due to limited notes support)
- Vanilla JS + CSS for web visualization
- HTMX for dynamic content loading

## Project Conventions

### Code Style
- Standard Go formatting (gofmt)
- Packages organized by domain: `cmd/`, `internal/claude/`, `internal/git/`, `internal/storage/`, `internal/web/`

### Architecture Patterns
- CLI commands in `cmd/` package using Cobra
- Internal packages for domain logic
- Embedded static assets for single binary distribution
- Shell execution for git commands

### Testing Strategy
- **Acceptance tests (Ginkgo/Gomega)**: Outside-in tests against compiled binary
  - Test against real git repositories (created in temp directories)
  - Test real CLI invocations via `exec.Command`
  - Use local bare repos as "remotes" for sync testing
  - **Real Claude CLI integration** with HOME isolation for test environments
  - Skip Claude tests with `CLAUDIT_SKIP_CLAUDE_TESTS=1` when API unavailable
- **Unit tests**: For internal parsing and compression logic
- All features MUST have acceptance test coverage before considered complete
- **Test isolation**: Override `HOME` env var to prevent polluting real `~/.claude/`

### Git Workflow
- Standard feature branch workflow
- Git notes stored in `refs/notes/claude-conversations`

## Domain Context

### Claude Code Conversations
- Stored as JSONL files at `~/.claude/projects/<encoded-path>/<session-id>.jsonl`
- Entry types: `user`, `assistant`, `system`, `file-history-snapshot`, `summary`
- Messages form a tree via `uuid` and `parentUuid` fields

### Git Notes
- Separate ref namespace: `refs/notes/claude-conversations`
- Notes attached to commit SHAs
- Require explicit push/fetch (not included in regular git operations)

## Important Constraints
- Localhost-only web server (security by default)
- Single binary distribution (embedded assets)
- Must work with Claude Code's hook system (PostToolUse)
- Timeout limit of 30 seconds for hooks

## External Dependencies
- Claude Code CLI (`claude` command)
- Git CLI
- Claude Code's settings and transcript file formats
