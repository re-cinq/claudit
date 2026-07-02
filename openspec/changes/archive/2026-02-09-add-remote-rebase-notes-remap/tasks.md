## 1. Implementation
- [ ] 1.1 Add `PatchID` function in `internal/git/` to compute patch IDs for a set of commits
- [ ] 1.2 Add `CopyNote` function in `internal/git/notes.go` to copy a note from one commit to another
- [ ] 1.3 Add `FindOrphanedNotes` function to identify notes whose commits are no longer on any branch
- [ ] 1.4 Create `cmd/remap.go` with `claudit remap` command that matches orphaned notes to new commits via patch-id
- [ ] 1.5 Update post-merge hook in `internal/git/hooks.go` to run `claudit remap` after `claudit sync pull`
- [ ] 1.6 Add "GitHub Rebase Merge" section to README.md

## 2. Testing
- [ ] 2.1 Acceptance test: `claudit remap` matches rebased commits by patch-id and copies notes
- [ ] 2.2 Acceptance test: notes are accessible on new commit SHAs after remap
- [ ] 2.3 Acceptance test: orphaned notes with no patch-id match are reported but not deleted
- [ ] 2.4 Acceptance test: post-merge hook triggers remap automatically
