# CLI Foundation

Delta: Sync commands use custom git notes ref.

## MODIFIED Requirements

### Requirement: Sync commands
The CLI MUST sync git notes using `refs/notes/claude-conversations`.

#### Scenario: Push notes to remote
- **WHEN** the user runs `claudit sync push`
- **THEN** claudit pushes `refs/notes/claude-conversations` to origin

#### Scenario: Pull notes from remote
- **WHEN** the user runs `claudit sync pull`
- **THEN** claudit fetches `refs/notes/claude-conversations` from origin
