# cli Specification

## Purpose
Extended CLI commands for session tracking, manual commit capture, terminal conversation viewing, and incremental display.
## Requirements
### Requirement: Session Tracking
The system SHALL track active Claude Code sessions to enable conversation capture for manual commits.

#### Scenario: Session start tracking
- **WHEN** Claude Code starts a session in a shiftlog-enabled repository
- **THEN** the `SessionStart` hook writes session info to `.shiftlog/active-session.json`
- **AND** the file contains `session_id`, `transcript_path`, `started_at`, and `project_path`

#### Scenario: Session end cleanup
- **WHEN** Claude Code ends a session in a shiftlog-enabled repository
- **THEN** the `SessionEnd` hook removes `.shiftlog/active-session.json`

#### Scenario: Stale session detection
- **WHEN** reading active session state
- **AND** the transcript file has not been modified in 10+ minutes
- **THEN** the session is considered inactive

### Requirement: Manual Commit Capture
The `store` command SHALL support a `--manual` flag for capturing conversations from manual git commits.

#### Scenario: Manual commit during active session
- **WHEN** user runs `git commit` manually (not via Claude)
- **AND** an active Claude session exists for the repository
- **THEN** the `post-commit` hook invokes `shiftlog store --manual`
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
- **WHEN** `shiftlog store` is invoked for a commit
- **AND** a note already exists for that commit
- **AND** the existing note has the same `session_id`
- **THEN** the store operation is skipped silently
- **AND** no error is raised

#### Scenario: Different session overwrites
- **WHEN** `shiftlog store` is invoked for a commit
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
- **WHEN** user runs `shiftlog init`
- **THEN** a `post-commit` hook is installed in `.git/hooks/`
- **AND** the hook runs `shiftlog store --manual`

#### Scenario: Hook coexistence
- **WHEN** a `post-commit` hook already exists
- **THEN** the shiftlog section is added without removing existing content
- **AND** the existing hook functionality is preserved

### Requirement: Claude Code Session Hooks
The `init` command SHALL configure Claude Code hooks for session tracking.

#### Scenario: SessionStart hook installation
- **WHEN** user runs `shiftlog init`
- **THEN** a `SessionStart` hook is added to `.claude/settings.local.json`
- **AND** the hook runs `shiftlog session-start`

#### Scenario: SessionEnd hook installation
- **WHEN** user runs `shiftlog init`
- **THEN** a `SessionEnd` hook is added to `.claude/settings.local.json`
- **AND** the hook runs `shiftlog session-end`

### Requirement: Doctor Validation
The `doctor` command SHALL validate manual commit capture configuration.

#### Scenario: Check session hooks
- **WHEN** user runs `shiftlog doctor`
- **THEN** the command checks for `SessionStart` and `SessionEnd` hooks in Claude settings
- **AND** reports OK or FAIL for each

#### Scenario: Check post-commit hook
- **WHEN** user runs `shiftlog doctor`
- **THEN** the command checks for `post-commit` hook in `.git/hooks/`
- **AND** reports OK or FAIL based on presence and content

### Requirement: Terminal Conversation Viewer
The CLI SHALL provide a `show` command that displays conversation history for a given commit reference in the terminal.

#### Scenario: Show conversation for commit
- **WHEN** user runs `shiftlog show <ref>` where `<ref>` is a valid commit with a stored conversation
- **THEN** the conversation is displayed in the terminal with formatted output

#### Scenario: Show conversation for HEAD
- **WHEN** user runs `shiftlog show` without arguments
- **THEN** the conversation for HEAD is displayed (if one exists)

#### Scenario: Commit has no conversation
- **WHEN** user runs `shiftlog show <ref>` where `<ref>` has no stored conversation
- **THEN** an error message is displayed indicating no conversation exists for that commit

#### Scenario: Invalid commit reference
- **WHEN** user runs `shiftlog show <ref>` where `<ref>` is not a valid commit
- **THEN** an error message is displayed indicating the commit could not be found

### Requirement: Conversation Output Format
The terminal output SHALL be formatted for readability with clear visual separation between messages.

#### Scenario: User messages displayed
- **WHEN** conversation contains user messages
- **THEN** they are displayed with a "User:" prefix and distinct formatting

#### Scenario: Assistant messages displayed
- **WHEN** conversation contains assistant messages
- **THEN** they are displayed with an "Assistant:" prefix and distinct formatting

#### Scenario: Tool use displayed
- **WHEN** conversation contains tool use entries
- **THEN** tool name is displayed with a "[tool: Name]" prefix
- **AND** tool inputs are displayed (e.g., command for Bash, file_path and content for Write)
- **AND** multi-line inputs are indented and truncated after 10 lines

#### Scenario: Tool results displayed
- **WHEN** conversation contains tool result entries
- **THEN** they are displayed with a "[tool result]" prefix
- **AND** the actual result content is shown (command output, file creation confirmations, etc.)

#### Scenario: System messages displayed
- **WHEN** conversation contains system messages
- **THEN** they are displayed with a "System:" prefix (can be filtered with flags in future)

#### Scenario: Thinking blocks displayed
- **WHEN** conversation contains assistant thinking blocks
- **THEN** they are displayed in a collapsible/summarized format
- **AND** first 3 lines are shown by default with option to expand

### Requirement: Web View Consistency
The web view SHALL use the same rendering rules as the terminal view for consistency.

#### Scenario: Web view shows tool inputs
- **WHEN** viewing conversation in web UI
- **THEN** tool inputs are shown with the same formatting as terminal (command summary, file paths)

#### Scenario: Web view shows tool results
- **WHEN** viewing conversation in web UI
- **THEN** tool results show actual content, not placeholders

#### Scenario: Web view filters internal entries
- **WHEN** viewing conversation in web UI
- **THEN** progress and file-history-snapshot entries are filtered out

### Requirement: Incremental Conversation Display
The `show` command SHALL display only conversation entries since the last commit by default.

#### Scenario: Show incremental conversation
- **WHEN** user runs `shiftlog show <ref>` where `<ref>` has a stored conversation
- **AND** a parent commit also has a stored conversation
- **THEN** only entries that occurred after the parent's last entry are displayed

#### Scenario: Initial commit (no parent conversation)
- **WHEN** user runs `shiftlog show <ref>` where `<ref>` is the first commit with a conversation
- **OR** no parent commits have stored conversations
- **THEN** the full conversation is displayed (same as `--full`)

#### Scenario: Merge commits
- **WHEN** user runs `shiftlog show <ref>` where `<ref>` is a merge commit
- **THEN** the first parent with a conversation is used as the boundary
- **AND** entries after that parent's last entry are displayed

#### Scenario: Different session ID
- **WHEN** user runs `shiftlog show <ref>` where `<ref>` has a different session_id than parent
- **THEN** the full conversation is displayed (new session started)

### Requirement: Full History Flag
The `show` command SHALL support a `--full` flag to display the complete session history.

#### Scenario: Show full conversation
- **WHEN** user runs `shiftlog show --full <ref>`
- **THEN** the complete session transcript is displayed (original behavior)

#### Scenario: Short flag
- **WHEN** user runs `shiftlog show -f <ref>`
- **THEN** the complete session transcript is displayed (same as `--full`)

### Requirement: Header Shows Scope
The conversation header SHALL indicate whether incremental or full view is being displayed.

#### Scenario: Incremental header
- **WHEN** displaying incremental conversation
- **THEN** header shows "Conversation for <sha> (since <parent-sha>)"
- **AND** shows count of entries in this increment

#### Scenario: Full header
- **WHEN** displaying full conversation (via --full or no parent)
- **THEN** header shows "Conversation for <sha> (full session)"
- **AND** shows total message count

