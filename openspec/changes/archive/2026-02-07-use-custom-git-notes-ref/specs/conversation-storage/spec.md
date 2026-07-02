# Conversation Storage

Delta: Use custom git notes ref to avoid polluting git log and colliding with other notes.

## MODIFIED Requirements

### Requirement: Store as Git Note
The compressed transcript MUST be stored as a Git Note attached to the commit using the custom ref `refs/notes/claude-conversations`.

#### Scenario: Create git note on custom ref
- **WHEN** storing a conversation for a commit
- **THEN** claudit attaches the note using `refs/notes/claude-conversations`
- **AND** the note does NOT appear on the default `refs/notes/commits` ref

#### Scenario: Notes invisible in default git log
- **WHEN** a commit has a claudit note attached
- **AND** the user runs `git log`
- **THEN** the note content is NOT displayed

## ADDED Requirements

### Requirement: Read notes from custom ref
Note retrieval operations MUST use the custom ref `refs/notes/claude-conversations`.

#### Scenario: List commits with notes
- **WHEN** running `claudit list`
- **THEN** claudit reads notes from `refs/notes/claude-conversations`

#### Scenario: Resume from commit
- **WHEN** running `claudit resume <commit>`
- **THEN** claudit reads the note from `refs/notes/claude-conversations`
