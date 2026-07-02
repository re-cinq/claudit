# Change: Remap notes after GitHub-side rebase merge

## Why
When a PR is merged via "Rebase and merge" on GitHub, the commits get new SHAs on the target branch. Notes remain keyed to the original branch SHAs and become orphaned once the branch is deleted. Unlike local rebase, there is no git hook that fires during GitHub's server-side rebase, so `notes.rewriteRef` does not help.

## What Changes
- A new `claudit remap` command detects rebase merges and copies notes from old commit SHAs to new ones
- Uses `git patch-id` to match old commits to their rebased counterparts by content
- The `post-merge` hook is updated to trigger remap after pulls that include rebase-merged PRs
- The README is updated with a "GitHub Rebase Merge" section documenting the behavior

## Impact
- Affected specs: `conversation-storage`
- Affected code: `cmd/remap.go` (new), `internal/git/notes.go`, `internal/git/hooks.go`, `README.md`
