# Change: Fix notes sync to use git notes merge

## Why
Push and fetch of `refs/notes/claude-conversations` fail silently when the notes ref has diverged between developers. The current implementation does a raw `git push`/`git fetch` with no merge step, swallowing errors in `cmd/sync.go`. This means multi-developer teams lose conversation notes without warning.

## What Changes
- `claudit sync pull` fetches remote notes to a separate tracking ref, runs `git notes merge` to combine them, then reports the result
- `claudit sync push` pushes the merged result and handles non-fast-forward errors by prompting a pull first
- A conflict strategy is chosen for the rare case of conflicting notes on the same commit SHA (concatenate both)
- The README is updated with a "Multi-Developer Sync" section documenting the behavior

## Impact
- Affected specs: `conversation-storage`
- Affected code: `internal/git/notes.go`, `cmd/sync.go`, `README.md`
