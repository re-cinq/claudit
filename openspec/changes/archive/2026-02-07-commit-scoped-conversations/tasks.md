# Tasks: Commit-Scoped Conversation History

## Implementation Tasks

- [x] Create proposal and spec files
- [x] Add `GetParentCommits` function to `internal/git/repo.go`
- [x] Add `GetLastEntryUUID` and `GetEntriesSince` to `internal/claude/transcript.go`
- [x] Update `cmd/show.go` for incremental display with `--full` flag
- [x] Update `internal/web/handlers.go` for `?incremental=true` parameter
- [x] Write unit tests for transcript diff functions
- [x] Write acceptance tests for incremental show command
- [x] Update web UI to support incremental toggle

## Testing Checklist

- [x] Incremental display shows only new entries
- [x] `--full` flag shows complete transcript
- [x] Initial commit shows full transcript
- [x] Merge commits use first parent
- [x] Different session ID shows full transcript
- [x] Web API returns correct incremental data
- [x] Header indicates incremental vs full view
- [ ] Web UI toggle switches between incremental and full view
- [ ] Web UI shows parent commit link in incremental mode
