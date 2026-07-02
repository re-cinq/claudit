# Change: Preserve notes across local rebase

## Why
When a user rebases locally, git rewrites commit SHAs. Notes remain keyed to the old (pre-rebase) SHAs and become orphaned. Git has built-in support for automatic note remapping via the `notes.rewriteRef` config, which `claudit init` already sets — but users who initialized before this config was added, or who configure manually, may not have it. This spec formalizes the behavior and ensures it is documented.

## What Changes
- The existing `notes.rewriteRef` configuration in `claudit init` is formally specified as a requirement
- `claudit doctor` validates that `notes.rewriteRef` is set correctly
- The README is updated with a "Local Rebase" section documenting automatic note preservation

## Impact
- Affected specs: `conversation-storage`
- Affected code: `cmd/init.go` (already implemented), `cmd/doctor.go` (validation), `README.md`
