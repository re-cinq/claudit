## ADDED Requirements
### Requirement: Remap Notes After Remote Rebase Merge
The system SHALL detect commits that were rebase-merged on GitHub and copy conversation notes from old SHAs to the new SHAs.

#### Scenario: Remap after rebase merge
- **WHEN** a PR was merged via "Rebase and merge" on GitHub
- **AND** user runs `claudit remap` (or pulls triggering the post-merge hook)
- **THEN** claudit identifies orphaned notes (notes keyed to SHAs not on any branch)
- **AND** computes `git patch-id` for both orphaned and new commits
- **AND** copies notes from old SHAs to matching new SHAs

#### Scenario: No matching commit found
- **WHEN** an orphaned note's commit cannot be matched by patch-id
- **THEN** claudit reports the unmatched note to the user
- **AND** does not delete the orphaned note

#### Scenario: Post-merge hook triggers remap
- **WHEN** user runs `git pull` (or `git merge`) after a rebase-merged PR
- **THEN** the post-merge hook runs `claudit sync pull` followed by `claudit remap`

#### Scenario: Notes accessible after remap
- **WHEN** notes have been remapped to new SHAs
- **THEN** `claudit list` shows the new commit SHAs
- **AND** `claudit show <new-sha>` displays the conversation
- **AND** `claudit resume <new-sha>` works correctly

### Requirement: Remap CLI Command
The CLI SHALL provide a `claudit remap` command for manual note remapping.

#### Scenario: Manual remap invocation
- **WHEN** user runs `claudit remap`
- **THEN** claudit scans for orphaned notes and attempts patch-id matching
- **AND** reports how many notes were remapped and how many remain orphaned

#### Scenario: No orphaned notes
- **WHEN** user runs `claudit remap`
- **AND** all notes are keyed to reachable commits
- **THEN** claudit reports "No orphaned notes found"

### Requirement: GitHub Rebase Merge Documentation
The README SHALL document how notes are preserved after GitHub rebase merges.

#### Scenario: README contains rebase merge section
- **WHEN** a user reads the README
- **THEN** there is a "GitHub Rebase Merge" section at the end
- **AND** it explains that notes are automatically remapped after pulling a rebase-merged PR
- **AND** it explains the `claudit remap` command for manual remapping
- **AND** it explains that patch-id matching is used to find corresponding commits
