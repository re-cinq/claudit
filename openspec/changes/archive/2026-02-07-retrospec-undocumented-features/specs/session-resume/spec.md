## MODIFIED Requirements

### Requirement: Checkout and launch Claude
The resume command MUST checkout the commit and launch Claude Code, with a force option to skip confirmation.

#### Scenario: Checkout commit
- Given a valid commit reference
- When resuming the session
- Then claudit runs `git checkout <commit>`

#### Scenario: Launch Claude with session
- Given a restored session
- When resuming the session
- Then claudit launches `claude --resume <session-id>`

#### Scenario: Warn about uncommitted changes
- Given the working directory has uncommitted changes
- When the user runs `claudit resume <commit>`
- Then claudit warns about uncommitted changes
- And prompts for confirmation before proceeding

#### Scenario: Force skip confirmation
- **WHEN** the working directory has uncommitted changes
- **AND** the user runs `claudit resume --force <commit>` or `claudit resume -f <commit>`
- **THEN** claudit skips the confirmation prompt
- **AND** proceeds with checkout and resume
