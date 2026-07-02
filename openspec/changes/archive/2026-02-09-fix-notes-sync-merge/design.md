## Context
When multiple developers work on the same repo, each produces conversation notes keyed to different commits. The current code fetches directly into the local notes ref (`refs/notes/claude-conversations:refs/notes/claude-conversations`), which fails with a non-fast-forward error when refs have diverged. The error is swallowed.

## Goals / Non-Goals
- Goals: reliable multi-developer notes sync with automatic merge
- Non-Goals: interactive conflict resolution UI; supporting notes refs other than `claude-conversations`

## Decisions
- **Fetch to a tracking ref**: Fetch remote notes to `refs/notes/claude-conversations-remote`, then merge into `refs/notes/claude-conversations`. This mirrors the fetch-then-merge pattern Git uses for branches.
- **Merge strategy**: Use `git notes merge --strategy=cat_sort_uniq` for conflicts. This concatenates both notes on the same commit, which is safe because claudit notes are self-contained base64-encoded blobs. In practice, conflicts are extremely rare (two developers annotating the same commit SHA).
- **Alternatives considered**: (1) Force-push/force-fetch — loses data. (2) Manual conflict resolution — too complex for automated hooks. (3) `union` strategy — `cat_sort_uniq` is more predictable for binary-like data.

## Risks / Trade-offs
- Concatenated notes on the same commit will contain two separate encoded conversations. The `show` and `serve` commands will need to handle multi-note blobs if this ever happens in practice, but this can be deferred since it requires two developers to annotate the exact same commit SHA.

## Open Questions
- None remaining.
