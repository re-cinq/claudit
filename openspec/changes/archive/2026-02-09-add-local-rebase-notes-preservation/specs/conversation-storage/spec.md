## ADDED Requirements
### Requirement: Notes Preserved Across Local Rebase
The system SHALL preserve conversation notes when commits are rewritten by local `git rebase`.

#### Scenario: Notes follow rebased commits
- **WHEN** a user runs `git rebase` on a branch with conversation notes
- **AND** `notes.rewriteRef` is configured for `refs/notes/claude-conversations`
- **THEN** git automatically remaps notes to the new commit SHAs
- **AND** conversation notes are accessible on the rebased commits

#### Scenario: Init configures rewriteRef
- **WHEN** user runs `claudit init`
- **THEN** git config `notes.rewriteRef` is set to `refs/notes/claude-conversations`

#### Scenario: Doctor validates rewriteRef
- **WHEN** user runs `claudit doctor`
- **THEN** the command checks that `notes.rewriteRef` includes `refs/notes/claude-conversations`
- **AND** reports OK if configured, FAIL if missing

### Requirement: Local Rebase Documentation
The README SHALL document how notes are preserved during local rebase.

#### Scenario: README contains rebase section
- **WHEN** a user reads the README
- **THEN** there is a "Local Rebase" section at the end
- **AND** it explains that notes automatically follow commits during local rebase
- **AND** it explains that this is configured by `claudit init` via `notes.rewriteRef`
