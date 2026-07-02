# Change: Commit-Scoped Conversation History

## Why
Currently, `claudit show` and the web UI display the **entire** Claude session transcript when viewing a commit's conversation. This makes it difficult to understand what conversation led to a specific commit, especially in long sessions with multiple commits. Users expect to see only the conversation that happened since the last commit.

## What Changes
- `claudit show` displays only conversation entries since the last commit (incremental by default)
- Add `--full` flag to show complete session history when needed
- Web API supports `?incremental=true` query parameter
- Git commit graph determines conversation boundaries (no external state files)

## Impact
- Affected specs: cli (modified behavior), web-visualization (new parameter)
- Affected code: `cmd/show.go`, `internal/claude/transcript.go`, `internal/git/repo.go`, `internal/web/handlers.go`
