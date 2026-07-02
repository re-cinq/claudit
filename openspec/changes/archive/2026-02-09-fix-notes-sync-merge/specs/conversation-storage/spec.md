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

#### Scenario: Sync pull fetches to tracking ref then merges
- **WHEN** user runs `claudit sync pull`
- **THEN** claudit fetches remote notes to `refs/notes/claude-conversations-remote`
- **AND** runs `git notes merge` to combine remote notes into `refs/notes/claude-conversations`
- **AND** reports the number of notes merged

#### Scenario: Sync pull with diverged notes merges cleanly
- **WHEN** two developers have each added notes to different commits
- **AND** user runs `claudit sync pull`
- **THEN** notes from both developers are present in the local ref
- **AND** no data is lost

#### Scenario: Sync pull with conflicting notes on same commit
- **WHEN** two developers have annotated the same commit SHA
- **AND** user runs `claudit sync pull`
- **THEN** the merge uses the `cat_sort_uniq` strategy
- **AND** both notes are preserved (concatenated)

#### Scenario: Sync push with non-fast-forward
- **WHEN** user runs `claudit sync push`
- **AND** the remote notes ref has diverged
- **THEN** the push fails with a clear error message
- **AND** claudit advises the user to run `claudit sync pull` first

#### Scenario: Sync push after successful merge
- **WHEN** user runs `claudit sync push`
- **AND** the local notes ref is up to date with the remote
- **THEN** notes are pushed successfully

## ADDED Requirements
### Requirement: Multi-Developer Sync Documentation
The README SHALL document multi-developer sync behavior.

#### Scenario: README contains sync section
- **WHEN** a user reads the README
- **THEN** there is a "Multi-Developer Sync" section at the end
- **AND** it explains that notes sync automatically on push/pull
- **AND** it explains that diverged notes are merged automatically
- **AND** it explains the conflict resolution strategy (concatenation)
