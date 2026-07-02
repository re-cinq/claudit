# conversation-storage Specification

## Purpose
Captures Claude Code conversation transcripts, compresses them, and stores them as Git Notes attached to commits.
## Requirements
### Requirement: Hook handler for commit detection
The `shiftlog store` command MUST process PostToolUse hook events and detect git commits.

#### Scenario: Detect git commit command
- Given Claude Code executed a Bash command
- When the hook JSON indicates `tool_input.command` contains `git commit`
- Then shiftlog proceeds to store the conversation

#### Scenario: Ignore non-commit commands
- Given Claude Code executed a Bash command
- When the hook JSON indicates a command that is not `git commit`
- Then shiftlog exits silently with status 0

#### Scenario: Handle malformed hook input
- Given shiftlog receives invalid JSON on stdin
- When processing the hook
- Then shiftlog logs a warning and exits with status 0
- And does not disrupt the user's workflow

### Requirement: Read Claude Code transcript
The store command MUST read the JSONL transcript from the path provided by the hook.

#### Scenario: Read transcript from hook-provided path
- Given the hook JSON contains `transcript_path`
- When shiftlog processes the hook
- Then shiftlog reads the JSONL file at that path

#### Scenario: Handle missing transcript file
- Given the hook JSON contains a `transcript_path` that doesn't exist
- When shiftlog processes the hook
- Then shiftlog logs a warning and exits with status 0

### Requirement: Parse JSONL transcript format
The storage module MUST correctly parse Claude Code's JSONL transcript format.

#### Scenario: Parse user message entry
- Given a JSONL line with `"type": "user"`
- When parsing the transcript
- Then shiftlog extracts uuid, parentUuid, timestamp, and message content

#### Scenario: Parse assistant message entry
- Given a JSONL line with `"type": "assistant"`
- When parsing the transcript
- Then shiftlog extracts the message content blocks (text, thinking, tool_use)

#### Scenario: Parse tool result entry
- Given a JSONL line with `"type": "user"` and `tool_result` content
- When parsing the transcript
- Then shiftlog links it to the source tool_use via `sourceToolAssistantUUID`

#### Scenario: Handle unknown entry types gracefully
- Given a JSONL line with an unrecognized type
- When parsing the transcript
- Then shiftlog preserves the raw JSON for future compatibility

### Requirement: Compress and encode transcript
Transcripts MUST be compressed and encoded for efficient storage in git notes.

#### Scenario: Compress with gzip
- Given a JSONL transcript
- When storing the conversation
- Then the transcript is compressed using gzip

#### Scenario: Encode as base64
- Given compressed transcript data
- When storing the conversation
- Then the data is encoded as base64 for safe text embedding

### Requirement: Store as Git Note
The compressed transcript MUST be stored as a Git Note attached to the commit using the custom ref `refs/notes/claude-conversations`.

#### Scenario: Create git note on custom ref
- **WHEN** storing a conversation for a commit
- **THEN** shiftlog attaches the note using `refs/notes/claude-conversations`
- **AND** the note does NOT appear on the default `refs/notes/commits` ref

#### Scenario: Notes invisible in default git log
- **WHEN** a commit has a shiftlog note attached
- **AND** the user runs `git log`
- **THEN** the note content is NOT displayed

#### Scenario: Sync pull fetches to tracking ref then merges
- **WHEN** user runs `shiftlog sync pull`
- **THEN** shiftlog fetches remote notes to `refs/notes/claude-conversations-remote`
- **AND** runs `git notes merge` to combine remote notes into `refs/notes/claude-conversations`
- **AND** reports the number of notes merged

#### Scenario: Sync pull with diverged notes merges cleanly
- **WHEN** two developers have each added notes to different commits
- **AND** user runs `shiftlog sync pull`
- **THEN** notes from both developers are present in the local ref
- **AND** no data is lost

#### Scenario: Sync pull with conflicting notes on same commit
- **WHEN** two developers have annotated the same commit SHA
- **AND** user runs `shiftlog sync pull`
- **THEN** the merge uses the `cat_sort_uniq` strategy
- **AND** both notes are preserved (concatenated)

#### Scenario: Sync push with non-fast-forward
- **WHEN** user runs `shiftlog sync push`
- **AND** the remote notes ref has diverged
- **THEN** the push fails with a clear error message
- **AND** shiftlog advises the user to run `shiftlog sync pull` first

#### Scenario: Sync push after successful merge
- **WHEN** user runs `shiftlog sync push`
- **AND** the local notes ref is up to date with the remote
- **THEN** notes are pushed successfully

### Requirement: Multi-Developer Sync Documentation
The README SHALL document multi-developer sync behavior.

#### Scenario: README contains sync section
- **WHEN** a user reads the README
- **THEN** there is a "Multi-Developer Sync" section at the end
- **AND** it explains that notes sync automatically on push/pull
- **AND** it explains that diverged notes are merged automatically
- **AND** it explains the conflict resolution strategy (concatenation)

### Requirement: Compute integrity checksum
A checksum MUST be stored to verify transcript integrity on retrieval.

#### Scenario: SHA256 checksum of original transcript
- Given a JSONL transcript
- When storing the conversation
- Then the checksum is computed from the original (uncompressed) transcript
- And stored in the format `sha256:<hex>`

### Requirement: Read notes from custom ref
Note retrieval operations MUST use the custom ref `refs/notes/claude-conversations`.

#### Scenario: List commits with notes
- **WHEN** running `shiftlog list`
- **THEN** shiftlog reads notes from `refs/notes/claude-conversations`

#### Scenario: Resume from commit
- **WHEN** running `shiftlog resume <commit>`
- **THEN** shiftlog reads the note from `refs/notes/claude-conversations`

### Requirement: Notes Preserved Across Local Rebase
The system SHALL preserve conversation notes when commits are rewritten by local `git rebase`.

#### Scenario: Notes follow rebased commits
- **WHEN** a user runs `git rebase` on a branch with conversation notes
- **AND** `notes.rewriteRef` is configured for `refs/notes/claude-conversations`
- **THEN** git automatically remaps notes to the new commit SHAs
- **AND** conversation notes are accessible on the rebased commits

#### Scenario: Init configures rewriteRef
- **WHEN** user runs `shiftlog init`
- **THEN** git config `notes.rewriteRef` is set to `refs/notes/claude-conversations`

#### Scenario: Doctor validates rewriteRef
- **WHEN** user runs `shiftlog doctor`
- **THEN** the command checks that `notes.rewriteRef` includes `refs/notes/claude-conversations`
- **AND** reports OK if configured, FAIL if missing

### Requirement: Local Rebase Documentation
The README SHALL document how notes are preserved during local rebase.

#### Scenario: README contains rebase section
- **WHEN** a user reads the README
- **THEN** there is a "Local Rebase" section at the end
- **AND** it explains that notes automatically follow commits during local rebase
- **AND** it explains that this is configured by `shiftlog init` via `notes.rewriteRef`

### Requirement: Remap Notes After Remote Rebase Merge
The system SHALL detect commits that were rebase-merged on GitHub and copy conversation notes from old SHAs to the new SHAs.

#### Scenario: Remap after rebase merge
- **WHEN** a PR was merged via "Rebase and merge" on GitHub
- **AND** user runs `shiftlog remap` (or pulls triggering the post-merge hook)
- **THEN** shiftlog identifies orphaned notes (notes keyed to SHAs not on any branch)
- **AND** computes `git patch-id` for both orphaned and new commits
- **AND** copies notes from old SHAs to matching new SHAs

#### Scenario: No matching commit found
- **WHEN** an orphaned note's commit cannot be matched by patch-id
- **THEN** shiftlog reports the unmatched note to the user
- **AND** does not delete the orphaned note

#### Scenario: Post-merge hook triggers remap
- **WHEN** user runs `git pull` (or `git merge`) after a rebase-merged PR
- **THEN** the post-merge hook runs `shiftlog sync pull` followed by `shiftlog remap`

#### Scenario: Notes accessible after remap
- **WHEN** notes have been remapped to new SHAs
- **THEN** `shiftlog list` shows the new commit SHAs
- **AND** `shiftlog show <new-sha>` displays the conversation
- **AND** `shiftlog resume <new-sha>` works correctly

### Requirement: Remap CLI Command
The CLI SHALL provide a `shiftlog remap` command for manual note remapping.

#### Scenario: Manual remap invocation
- **WHEN** user runs `shiftlog remap`
- **THEN** shiftlog scans for orphaned notes and attempts patch-id matching
- **AND** reports how many notes were remapped and how many remain orphaned

#### Scenario: No orphaned notes
- **WHEN** user runs `shiftlog remap`
- **AND** all notes are keyed to reachable commits
- **THEN** shiftlog reports "No orphaned notes found"

### Requirement: GitHub Rebase Merge Documentation
The README SHALL document how notes are preserved after GitHub rebase merges.

#### Scenario: README contains rebase merge section
- **WHEN** a user reads the README
- **THEN** there is a "GitHub Rebase Merge" section at the end
- **AND** it explains that notes are automatically remapped after pulling a rebase-merged PR
- **AND** it explains the `shiftlog remap` command for manual remapping
- **AND** it explains that patch-id matching is used to find corresponding commits

