## MODIFIED Requirements

### Requirement: Restore transcript to agent location
The resume command MUST write the transcript where the configured agent expects it.

#### Scenario: Write to Codex sessions directory
- Given a decompressed transcript with session_id and agent `"codex"`
- When resuming the session
- Then shiftlog writes the rollout JSONL to `~/.codex/sessions/` in the date-organized directory structure

### Requirement: Checkout and launch coding agent
The resume command MUST checkout the commit and launch the configured coding agent, with a force option to skip confirmation.

#### Scenario: Launch Codex with session
- Given a restored session with agent `"codex"`
- When resuming the session
- Then shiftlog launches `codex resume <session-id>`
