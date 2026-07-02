## Context
GitHub's "Rebase and merge" strategy replays commits with new SHAs on the target branch. Since this happens server-side, no local git hooks fire, and `notes.rewriteRef` has no effect. Notes remain keyed to the original (now-deleted) branch SHAs.

## Goals / Non-Goals
- Goals: automatically remap notes to new SHAs after pulling a rebase-merged PR
- Non-Goals: real-time detection of GitHub merges via webhooks; supporting squash merges (single new SHA with no 1:1 commit mapping)

## Decisions
- **Use `git patch-id`**: Each commit's diff has a stable patch-id regardless of SHA. By computing patch-ids for both orphaned-note commits and newly-arrived commits, we can match old→new with high confidence. This is the same mechanism `git rebase` uses internally.
- **Trigger via post-merge hook**: After `claudit sync pull` runs in the post-merge hook, `claudit remap` scans for orphaned notes and attempts to match them. This covers the common flow of `git pull` after a PR is merged.
- **Copy, don't move**: Notes are copied to new SHAs using `git notes copy`. The original note is left in place (it becomes harmless once the old ref is gone). This is safer than removing notes that might still be referenced.
- **Alternatives considered**: (1) GitHub API to look up PR commits — adds external dependency and auth requirements. (2) Storing a mapping table of old→new SHAs — adds state management complexity. (3) Running remap on every sync — wasteful when no rebase merge occurred.

## Risks / Trade-offs
- Patch-id matching can produce false positives if two commits have identical diffs (rare but possible with cherry-picks). Acceptable because the worst case is copying a note to an extra commit.
- Orphaned notes whose commits have been garbage-collected cannot be patch-id matched. These are reported to the user but not deleted.

## Open Questions
- None remaining.
