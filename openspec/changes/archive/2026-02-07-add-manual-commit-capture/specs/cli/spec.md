## ADDED Requirements

### Requirement: Session Tracking
The system SHALL track active Claude Code sessions to enable conversation capture for manual commits.

#### Scenario: Session start tracking
- **WHEN** Claude Code starts a session in a claudit-enabled repository
- **THEN** the `SessionStart` hook writes session info to `.claudit/active-session.json`
- **AND** the file contains `session_id`, `transcript_path`, `started_at`, and `project_path`

#### Scenario: Session end cleanup
- **WHEN** Claude Code ends a session in a claudit-enabled repository
- **THEN** the `SessionEnd` hook removes `.claudit/active-session.json`

#### Scenario: Stale session detection
- **WHEN** reading active session state
- **AND** the transcript file has not been modified in 10+ minutes
- **THEN** the session is considered inactive

### Requirement: Manual Commit Capture
The `store` command SHALL support a `--manual` flag for capturing conversations from manual git commits.

#### Scenario: Manual commit during active session
- **WHEN** user runs `git commit` manually (not via Claude)
- **AND** an active Claude session exists for the repository
- **THEN** the `post-commit` hook invokes `claudit store --manual`
- **AND** the conversation is stored as a git note on the new commit

#### Scenario: Manual commit after recent session
- **WHEN** user runs `git commit` manually
- **AND** no active session exists
- **AND** a session for the repository ended within 5 minutes
- **THEN** the recent session's conversation is stored as a git note

#### Scenario: Manual commit with no relevant session
- **WHEN** user runs `git commit` manually
- **AND** no active session exists
- **AND** no recent session exists for the repository
- **THEN** the commit proceeds normally with no conversation stored
- **AND** no error or warning is displayed

#### Scenario: Project path validation
- **WHEN** discovering a session for manual commit capture
- **AND** the session's project path does not match the repository
- **THEN** the session is skipped
- **AND** discovery continues to find a matching session

### Requirement: Idempotent Storage
The `store` command SHALL be idempotent when storing conversations for the same commit and session.

#### Scenario: Duplicate storage prevention (same session)
- **WHEN** `claudit store` is invoked for a commit
- **AND** a note already exists for that commit
- **AND** the existing note has the same `session_id`
- **THEN** the store operation is skipped silently
- **AND** no error is raised

#### Scenario: Different session overwrites
- **WHEN** `claudit store` is invoked for a commit
- **AND** a note already exists for that commit
- **AND** the existing note has a different `session_id`
- **THEN** the new conversation overwrites the existing note

#### Scenario: Both hooks fire for Claude-made commit
- **WHEN** Claude Code makes a git commit via Bash tool
- **THEN** the `PostToolUse` hook fires and stores the conversation
- **AND** the `post-commit` git hook fires
- **AND** the second store operation detects the existing note
- **AND** skips storage (same session already stored)

### Requirement: Git Post-Commit Hook
The `init` command SHALL install a `post-commit` git hook for manual commit capture.

#### Scenario: Hook installation
- **WHEN** user runs `claudit init`
- **THEN** a `post-commit` hook is installed in `.git/hooks/`
- **AND** the hook runs `claudit store --manual`

#### Scenario: Hook coexistence
- **WHEN** a `post-commit` hook already exists
- **THEN** the claudit section is added without removing existing content
- **AND** the existing hook functionality is preserved

### Requirement: Claude Code Session Hooks
The `init` command SHALL configure Claude Code hooks for session tracking.

#### Scenario: SessionStart hook installation
- **WHEN** user runs `claudit init`
- **THEN** a `SessionStart` hook is added to `.claude/settings.local.json`
- **AND** the hook runs `claudit session-start`

#### Scenario: SessionEnd hook installation
- **WHEN** user runs `claudit init`
- **THEN** a `SessionEnd` hook is added to `.claude/settings.local.json`
- **AND** the hook runs `claudit session-end`

### Requirement: Doctor Validation
The `doctor` command SHALL validate manual commit capture configuration.

#### Scenario: Check session hooks
- **WHEN** user runs `claudit doctor`
- **THEN** the command checks for `SessionStart` and `SessionEnd` hooks in Claude settings
- **AND** reports OK or FAIL for each

#### Scenario: Check post-commit hook
- **WHEN** user runs `claudit doctor`
- **THEN** the command checks for `post-commit` hook in `.git/hooks/`
- **AND** reports OK or FAIL based on presence and content
