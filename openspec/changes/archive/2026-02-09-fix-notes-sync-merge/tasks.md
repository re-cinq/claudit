## 1. Implementation
- [x] 1.1 Add `FetchNotesToTracking` function in `internal/git/notes.go` to fetch remote notes to `refs/notes/claude-conversations-remote`
- [x] 1.2 Add `MergeNotes` function in `internal/git/notes.go` to run `git notes merge` with `--strategy=cat_sort_uniq` (concatenate on conflict)
- [x] 1.3 Update `FetchNotes` to fetch-then-merge instead of direct overwrite
- [x] 1.4 Update `PushNotes` to detect non-fast-forward failures and return a typed error
- [x] 1.5 Update `cmd/sync.go` `runSyncPull` to use fetch-then-merge flow and report merge results
- [x] 1.6 Update `cmd/sync.go` `runSyncPush` to advise `claudit sync pull` on non-fast-forward error
- [x] 1.7 Add "Multi-Developer Sync" section to README.md

## 2. Testing
- [x] 2.1 Acceptance test: two repos push notes to same bare remote, second pull merges cleanly
- [x] 2.2 Acceptance test: conflicting notes on same commit SHA produce concatenated result
- [x] 2.3 Acceptance test: push after divergence advises pull first
