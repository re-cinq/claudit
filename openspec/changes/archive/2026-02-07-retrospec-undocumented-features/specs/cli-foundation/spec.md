## ADDED Requirements

### Requirement: Diagnose configuration issues
The `claudit doctor` command MUST check the claudit setup and report issues.

#### Scenario: Check git repository status
- **WHEN** user runs `claudit doctor`
- **THEN** the command checks if the current directory is inside a git repository
- **AND** reports OK with the repository path, or FAIL if not in a repository

#### Scenario: Check claudit in PATH
- **WHEN** user runs `claudit doctor`
- **THEN** the command checks if `claudit` is available in the system PATH
- **AND** reports OK with the binary path, or FAIL with install instructions

#### Scenario: Check PostToolUse hook
- **WHEN** user runs `claudit doctor`
- **THEN** the command checks `.claude/settings.local.json` for a PostToolUse hook containing `claudit store`
- **AND** reports OK or FAIL based on the hook configuration

#### Scenario: Check git hooks
- **WHEN** user runs `claudit doctor`
- **THEN** the command checks for `pre-push`, `post-merge`, `post-checkout`, and `post-commit` hooks
- **AND** verifies each hook contains `claudit` commands
- **AND** reports OK or FAIL for the overall hook state

#### Scenario: Summary with fix suggestion
- **WHEN** `claudit doctor` detects issues
- **THEN** it prints "Issues found. Run 'claudit init' to fix configuration."
- **AND** exits with non-zero status

#### Scenario: All checks pass
- **WHEN** `claudit doctor` finds no issues
- **THEN** it prints "All checks passed! Claudit is properly configured."
- **AND** exits with zero status

### Requirement: Debug logging command
The `claudit debug` command MUST allow users to toggle debug logging on or off.

#### Scenario: Show current debug state
- **WHEN** user runs `claudit debug` with no flags
- **THEN** the command prints whether debug logging is currently on or off

#### Scenario: Enable debug logging
- **WHEN** user runs `claudit debug --on`
- **THEN** debug logging is enabled in `.claudit/config`
- **AND** the command prints "debug logging is on"

#### Scenario: Disable debug logging
- **WHEN** user runs `claudit debug --off`
- **THEN** debug logging is disabled in `.claudit/config`
- **AND** the command prints "debug logging is off"

#### Scenario: Toggle debug logging
- **WHEN** user runs `claudit debug --toggle`
- **THEN** the debug state is flipped (on becomes off, off becomes on)
- **AND** the new state is printed

#### Scenario: Mutually exclusive flags
- **WHEN** user runs `claudit debug` with more than one of `--on`, `--off`, `--toggle`
- **THEN** the command exits with an error indicating the flags are mutually exclusive

#### Scenario: Require initialization
- **WHEN** user runs `claudit debug` in a repository without `.claudit/` directory
- **THEN** the command exits with an error suggesting `claudit init`

### Requirement: Configuration system
Claudit MUST store per-repository configuration in `.claudit/config`.

#### Scenario: Config file format
- **WHEN** examining `.claudit/config`
- **THEN** it is a JSON file containing `debug` (boolean) and `notes_ref` (string) fields

#### Scenario: Default config when missing
- **WHEN** reading config and `.claudit/config` does not exist
- **THEN** default values are used (debug: false, notes_ref: empty string)

#### Scenario: Debug logging reads config
- **WHEN** any claudit command runs with debug logging enabled
- **THEN** diagnostic messages are written to stderr with "claudit: debug:" prefix

### Requirement: Debug logging output
Claudit MUST support optional debug logging to stderr for troubleshooting.

#### Scenario: Debug messages on stderr
- **WHEN** debug is enabled in `.claudit/config`
- **THEN** claudit writes diagnostic messages to stderr during operations

#### Scenario: Debug disabled by default
- **WHEN** debug is not configured or set to false
- **THEN** no debug messages are written

## MODIFIED Requirements

### Requirement: Sync commands
The CLI MUST sync git notes using `refs/notes/claude-conversations` and support configurable remotes.

#### Scenario: Push notes to remote
- **WHEN** the user runs `claudit sync push`
- **THEN** claudit pushes `refs/notes/claude-conversations` to origin

#### Scenario: Pull notes from remote
- **WHEN** the user runs `claudit sync pull`
- **THEN** claudit fetches `refs/notes/claude-conversations` from origin

#### Scenario: Custom remote
- **WHEN** the user runs `claudit sync push --remote upstream` or `claudit sync pull --remote upstream`
- **THEN** claudit syncs notes with the specified remote instead of origin

### Requirement: Initialize repository for claudit
The `claudit init` command MUST configure a git repository for conversation capture, including PATH validation and gitignore management.

#### Scenario: Initialize in a git repository
- Given the user is in a git repository
- When the user runs `claudit init`
- Then claudit creates/updates `.claude/settings.local.json` with PostToolUse hook
- And claudit installs git hooks for automatic note syncing
- And claudit displays a success message

#### Scenario: Initialize outside a git repository
- Given the user is not in a git repository
- When the user runs `claudit init`
- Then claudit exits with error "not inside a git repository"

#### Scenario: PATH check on init
- **WHEN** user runs `claudit init`
- **AND** `claudit` is not found in the system PATH
- **THEN** a warning is displayed that hooks may not work

#### Scenario: Gitignore management
- **WHEN** user runs `claudit init`
- **THEN** `.claudit/` is added to `.gitignore` if not already present
