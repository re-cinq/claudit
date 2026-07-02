## 1. Implementation
- [x] 1.1 Verify `claudit init` sets `notes.rewriteRef` to `refs/notes/claude-conversations` (already done in `cmd/init.go:165`)
- [x] 1.2 Add `notes.rewriteRef` check to `claudit doctor` output
- [x] 1.3 Add "Local Rebase" section to README.md explaining automatic note preservation

## 2. Testing
- [x] 2.1 Acceptance test: notes follow commits after `git rebase` when `notes.rewriteRef` is set
- [x] 2.2 Acceptance test: `claudit doctor` reports OK for `notes.rewriteRef` when configured
- [x] 2.3 Acceptance test: `claudit doctor` reports FAIL for `notes.rewriteRef` when missing
