## MODIFIED Requirements

### Requirement: Restore transcript to Claude location
The resume command MUST write the transcript where the configured agent expects it.

#### Scenario: Write JSONL to Claude projects directory
- Given a decompressed transcript with session_id and agent `"claude"`
- When resuming the session
- Then shiftlog writes to `~/.claude/projects/<encoded-path>/<session-id>.jsonl`

#### Scenario: Write to Gemini session directory
- Given a decompressed transcript with session_id and agent `"gemini"`
- When resuming the session
- Then shiftlog writes to the Gemini session directory at `~/.gemini/tmp/<hash>/chats/<session-id>.jsonl`

#### Scenario: Write to OpenCode session storage
- Given a decompressed transcript with session_id and agent `"opencode"`
- When resuming the session
- Then shiftlog updates the OpenCode SQLite database with the session data

#### Scenario: Encode project path correctly
- Given a project at `/Users/dev/workspace/myproject`
- When computing the encoded path for Claude
- Then the result is `-Users-dev-workspace-myproject`

#### Scenario: Update sessions index
- Given a restored transcript
- When resuming the session
- Then shiftlog updates the agent-appropriate sessions index

### Requirement: Checkout and launch coding agent
The resume command MUST checkout the commit and launch the configured coding agent, with a force option to skip confirmation.

#### Scenario: Checkout commit
- Given a valid commit reference
- When resuming the session
- Then shiftlog runs `git checkout <commit>`

#### Scenario: Launch Claude with session
- Given a restored session with agent `"claude"`
- When resuming the session
- Then shiftlog launches `claude --resume <session-id>`

#### Scenario: Launch Gemini with session
- Given a restored session with agent `"gemini"`
- When resuming the session
- Then shiftlog launches the Gemini CLI with the appropriate resume flag

#### Scenario: Launch OpenCode with session
- Given a restored session with agent `"opencode"`
- When resuming the session
- Then shiftlog launches the OpenCode CLI with the appropriate resume flag

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
