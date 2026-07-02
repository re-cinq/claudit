# Tasks: Manual Commit Capture

## 1. Session Tracking

- [x] 1.1 Create `internal/session/tracker.go` with `ActiveSession` struct
- [x] 1.2 Implement `WriteActiveSession()` to write `.claudit/active-session.json`
- [x] 1.3 Implement `ReadActiveSession()` to read and validate session state
- [x] 1.4 Implement `ClearActiveSession()` to remove state file
- [x] 1.5 Add `IsSessionActive()` check (validates transcript mtime)

## 2. Claude Code Hooks

- [x] 2.1 Add `SessionStart` hook configuration to `internal/claude/hooks.go`
- [x] 2.2 Add `SessionEnd` hook configuration to `internal/claude/hooks.go`
- [x] 2.3 Update `AddClauditHook()` to install both session hooks
- [x] 2.4 Create `cmd/session_start.go` command (writes active session)
- [x] 2.5 Create `cmd/session_end.go` command (clears active session)

## 3. Git Hook Installation

- [x] 3.1 Add `post-commit` hook to `internal/git/hooks.go`
- [x] 3.2 Update `InstallAllHooks()` to include post-commit
- [x] 3.3 Update hook command to `claudit store --manual`

## 4. Idempotent Storage (Duplicate Prevention)

- [x] 4.1 Add duplicate detection before `git.AddNote()` in `cmd/store.go`
- [x] 4.2 Read existing note if `git.HasNote()` returns true
- [x] 4.3 Parse existing note and compare `session_id`
- [x] 4.4 Skip storage if same session (idempotent)
- [x] 4.5 Overwrite if different session

## 5. Manual Store Command

- [x] 5.1 Add `--manual` flag to `cmd/store.go`
- [x] 5.2 Implement session discovery in manual mode:
  - [x] 5.2.1 Check active session file first
  - [x] 5.2.2 Fall back to sessions-index.json lookup
  - [x] 5.2.3 Validate project path matches
- [x] 5.3 Handle no-session-found case (exit silently)
- [x] 5.4 Reuse existing storage logic for transcript compression

## 6. Init Command Updates

- [x] 6.1 Update `cmd/init.go` to show new hooks in summary
- [x] 6.2 Ensure Claude hooks are installed for SessionStart/SessionEnd

## 7. Doctor Command Updates

- [x] 7.1 Add check for SessionStart hook in `cmd/doctor.go`
- [x] 7.2 Add check for SessionEnd hook
- [x] 7.3 Add check for post-commit git hook

## 8. Testing

- [x] 8.1 Unit tests for session tracker
- [x] 8.2 Unit tests for idempotent storage (covered by acceptance tests)
- [x] 8.3 Acceptance test: manual commit during active session
- [x] 8.4 Acceptance test: manual commit after recent session (covered by 8.3)
- [x] 8.5 Acceptance test: manual commit with no session (silent skip)
- [x] 8.6 Acceptance test: both hooks fire, second skips (idempotent)
- [x] 8.7 Acceptance test: init installs all new hooks
- [x] 8.8 Acceptance test: doctor validates new hooks

## 9. Documentation

- [x] 9.1 Update README with manual commit capture feature
- [x] 9.2 Document session tracking behavior
- [x] 9.3 Document duplicate prevention behavior
