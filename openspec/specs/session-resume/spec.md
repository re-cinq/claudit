# session-resume Specification

## Purpose
Restores Claude Code sessions from Git Notes and launches Claude Code with the historical conversation context.
## Requirements
### Requirement: Resolve commit reference
The resume command MUST accept various git commit references.

#### Scenario: Resume from full SHA
- Given a commit with a stored conversation
- When the user runs `shiftlog resume abc123def456...`
- Then shiftlog resolves the full SHA and retrieves the conversation

#### Scenario: Resume from short SHA
- Given a commit with a stored conversation
- When the user runs `shiftlog resume abc123`
- Then shiftlog resolves the short SHA to full SHA

#### Scenario: Resume from branch name
- Given a branch with a stored conversation on its tip
- When the user runs `shiftlog resume feature-branch`
- Then shiftlog resolves the branch to its commit SHA

#### Scenario: Resume from relative reference
- Given commits with stored conversations
- When the user runs `shiftlog resume HEAD~2`
- Then shiftlog resolves the relative reference correctly

### Requirement: Retrieve and decompress conversation
The resume command MUST read and decode the stored conversation.

#### Scenario: Read git note
- Given a commit with a stored conversation
- When resuming the session
- Then shiftlog reads the note from `refs/notes/claude-conversations`

#### Scenario: Decompress transcript
- Given an encoded, compressed transcript in the note
- When resuming the session
- Then shiftlog base64-decodes and gzip-decompresses the transcript

#### Scenario: Verify checksum
- Given a stored conversation with checksum
- When resuming the session
- Then shiftlog verifies the transcript matches the stored checksum
- And warns if verification fails (but continues)

### Requirement: Handle missing conversation
The resume command MUST handle commits without stored conversations gracefully.

#### Scenario: No conversation for commit
- Given a commit without a stored conversation
- When the user runs `shiftlog resume <commit>`
- Then shiftlog displays "no conversation found for commit <sha>"
- And exits with non-zero status

#### Scenario: Invalid commit reference
- Given an invalid commit reference
- When the user runs `shiftlog resume invalid-ref`
- Then shiftlog displays "could not resolve commit: invalid-ref"
- And exits with non-zero status

### Requirement: Restore transcript to Claude location
The resume command MUST write the transcript where Claude Code expects it.

#### Scenario: Write JSONL to Claude projects directory
- Given a decompressed transcript with session_id
- When resuming the session
- Then shiftlog writes to `~/.claude/projects/<encoded-path>/<session-id>.jsonl`

#### Scenario: Encode project path correctly
- Given a project at `/Users/dev/workspace/myproject`
- When computing the encoded path
- Then the result is `-Users-dev-workspace-myproject`

#### Scenario: Update sessions index
- Given a restored transcript
- When resuming the session
- Then shiftlog updates `~/.claude/projects/<encoded-path>/sessions-index.json`

### Requirement: Checkout and launch Claude
The resume command MUST checkout the commit and launch Claude Code, with a force option to skip confirmation.

#### Scenario: Checkout commit
- Given a valid commit reference
- When resuming the session
- Then shiftlog runs `git checkout <commit>`

#### Scenario: Launch Claude with session
- Given a restored session
- When resuming the session
- Then shiftlog launches `claude --resume <session-id>`

#### Scenario: Warn about uncommitted changes
- Given the working directory has uncommitted changes
- When the user runs `shiftlog resume <commit>`
- Then shiftlog warns about uncommitted changes
- And prompts for confirmation before proceeding

#### Scenario: Force skip confirmation
- **WHEN** the working directory has uncommitted changes
- **AND** the user runs `shiftlog resume --force <commit>` or `shiftlog resume -f <commit>`
- **THEN** shiftlog skips the confirmation prompt
- **AND** proceeds with checkout and resume

### Requirement: List available conversations
Users MUST be able to discover which commits have conversations.

#### Scenario: List commits with conversations
- Given a repository with stored conversations
- When the user runs `shiftlog list`
- Then shiftlog displays commits that have associated conversations
- And shows commit SHA, date, and message preview

