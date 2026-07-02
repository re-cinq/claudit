## ADDED Requirements

### Requirement: Incremental Conversation Display
The `show` command SHALL display only conversation entries since the last commit by default.

#### Scenario: Show incremental conversation
- **WHEN** user runs `claudit show <ref>` where `<ref>` has a stored conversation
- **AND** a parent commit also has a stored conversation
- **THEN** only entries that occurred after the parent's last entry are displayed

#### Scenario: Initial commit (no parent conversation)
- **WHEN** user runs `claudit show <ref>` where `<ref>` is the first commit with a conversation
- **OR** no parent commits have stored conversations
- **THEN** the full conversation is displayed (same as `--full`)

#### Scenario: Merge commits
- **WHEN** user runs `claudit show <ref>` where `<ref>` is a merge commit
- **THEN** the first parent with a conversation is used as the boundary
- **AND** entries after that parent's last entry are displayed

#### Scenario: Different session ID
- **WHEN** user runs `claudit show <ref>` where `<ref>` has a different session_id than parent
- **THEN** the full conversation is displayed (new session started)

### Requirement: Full History Flag
The `show` command SHALL support a `--full` flag to display the complete session history.

#### Scenario: Show full conversation
- **WHEN** user runs `claudit show --full <ref>`
- **THEN** the complete session transcript is displayed (original behavior)

#### Scenario: Short flag
- **WHEN** user runs `claudit show -f <ref>`
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
