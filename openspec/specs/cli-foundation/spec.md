# cli-foundation Specification

## Purpose
Core CLI structure, repository initialization, sync commands, configuration management, and diagnostic tooling for Shiftlog.
## Requirements
### Requirement: CLI entry point with Cobra
The CLI MUST provide a root command with version information and subcommands.

#### Scenario: User runs shiftlog with no arguments
- Given the user has shiftlog installed
- When the user runs `shiftlog`
- Then the CLI displays help text listing available commands

#### Scenario: User checks version
- Given the user has shiftlog installed
- When the user runs `shiftlog --version`
- Then the CLI displays the version number

### Requirement: Initialize repository for shiftlog
The `shiftlog init` command MUST configure a git repository for conversation capture, including PATH validation and gitignore management.

#### Scenario: Initialize in a git repository
- Given the user is in a git repository
- When the user runs `shiftlog init`
- Then shiftlog creates/updates `.claude/settings.local.json` with PostToolUse hook
- And shiftlog installs git hooks for automatic note syncing
- And shiftlog displays a success message

#### Scenario: Initialize outside a git repository
- Given the user is not in a git repository
- When the user runs `shiftlog init`
- Then shiftlog exits with error "not inside a git repository"

#### Scenario: PATH check on init
- **WHEN** user runs `shiftlog init`
- **AND** `shiftlog` is not found in the system PATH
- **THEN** a warning is displayed that hooks may not work

#### Scenario: Gitignore management
- **WHEN** user runs `shiftlog init`
- **THEN** `.shiftlog/` is added to `.gitignore` if not already present

### Requirement: PostToolUse hook configuration
The init command MUST configure Claude Code's hook system correctly.

#### Scenario: Hook configuration structure
- Given `shiftlog init` has been run
- When examining `.claude/settings.local.json`
- Then it contains a PostToolUse hook matching "Bash" tool
- And the hook command is `shiftlog store`
- And the hook timeout is 30 seconds

#### Scenario: Preserve existing settings
- Given `.claude/settings.local.json` already exists with other settings
- When the user runs `shiftlog init`
- Then existing settings are preserved
- And the PostToolUse hook is added or updated

### Requirement: Sync commands
The CLI MUST sync git notes using `refs/notes/claude-conversations` and support configurable remotes.

#### Scenario: Push notes to remote
- **WHEN** the user runs `shiftlog sync push`
- **THEN** shiftlog pushes `refs/notes/claude-conversations` to origin

#### Scenario: Pull notes from remote
- **WHEN** the user runs `shiftlog sync pull`
- **THEN** shiftlog fetches `refs/notes/claude-conversations` from origin

#### Scenario: Custom remote
- **WHEN** the user runs `shiftlog sync push --remote upstream` or `shiftlog sync pull --remote upstream`
- **THEN** shiftlog syncs notes with the specified remote instead of origin

### Requirement: Git hooks for automatic sync
The init command MUST install git hooks that invoke shiftlog sync commands.

#### Scenario: Pre-push hook installed
- Given `shiftlog init` has been run
- When examining `.git/hooks/pre-push`
- Then it calls `shiftlog sync push`

#### Scenario: Post-merge hook installed
- Given `shiftlog init` has been run
- When examining `.git/hooks/post-merge`
- Then it calls `shiftlog sync pull`

#### Scenario: Post-checkout hook installed
- Given `shiftlog init` has been run
- When examining `.git/hooks/post-checkout`
- Then it calls `shiftlog sync pull`

### Requirement: Diagnose configuration issues
The `shiftlog doctor` command MUST check the shiftlog setup and report issues.

#### Scenario: Check git repository status
- **WHEN** user runs `shiftlog doctor`
- **THEN** the command checks if the current directory is inside a git repository
- **AND** reports OK with the repository path, or FAIL if not in a repository

#### Scenario: Check shiftlog in PATH
- **WHEN** user runs `shiftlog doctor`
- **THEN** the command checks if `shiftlog` is available in the system PATH
- **AND** reports OK with the binary path, or FAIL with install instructions

#### Scenario: Check PostToolUse hook
- **WHEN** user runs `shiftlog doctor`
- **THEN** the command checks `.claude/settings.local.json` for a PostToolUse hook containing `shiftlog store`
- **AND** reports OK or FAIL based on the hook configuration

#### Scenario: Check git hooks
- **WHEN** user runs `shiftlog doctor`
- **THEN** the command checks for `pre-push`, `post-merge`, `post-checkout`, and `post-commit` hooks
- **AND** verifies each hook contains `shiftlog` commands
- **AND** reports OK or FAIL for the overall hook state

#### Scenario: Summary with fix suggestion
- **WHEN** `shiftlog doctor` detects issues
- **THEN** it prints "Issues found. Run 'shiftlog init' to fix configuration."
- **AND** exits with non-zero status

#### Scenario: All checks pass
- **WHEN** `shiftlog doctor` finds no issues
- **THEN** it prints "All checks passed! Shiftlog is properly configured."
- **AND** exits with zero status

### Requirement: Debug logging command
The `shiftlog debug` command MUST allow users to toggle debug logging on or off.

#### Scenario: Show current debug state
- **WHEN** user runs `shiftlog debug` with no flags
- **THEN** the command prints whether debug logging is currently on or off

#### Scenario: Enable debug logging
- **WHEN** user runs `shiftlog debug --on`
- **THEN** debug logging is enabled in `.shiftlog/config`
- **AND** the command prints "debug logging is on"

#### Scenario: Disable debug logging
- **WHEN** user runs `shiftlog debug --off`
- **THEN** debug logging is disabled in `.shiftlog/config`
- **AND** the command prints "debug logging is off"

#### Scenario: Toggle debug logging
- **WHEN** user runs `shiftlog debug --toggle`
- **THEN** the debug state is flipped (on becomes off, off becomes on)
- **AND** the new state is printed

#### Scenario: Mutually exclusive flags
- **WHEN** user runs `shiftlog debug` with more than one of `--on`, `--off`, `--toggle`
- **THEN** the command exits with an error indicating the flags are mutually exclusive

#### Scenario: Require initialization
- **WHEN** user runs `shiftlog debug` in a repository without `.shiftlog/` directory
- **THEN** the command exits with an error suggesting `shiftlog init`

### Requirement: Configuration system
Shiftlog MUST store per-repository configuration in `.shiftlog/config`.

#### Scenario: Config file format
- **WHEN** examining `.shiftlog/config`
- **THEN** it is a JSON file containing `debug` (boolean) and `notes_ref` (string) fields

#### Scenario: Default config when missing
- **WHEN** reading config and `.shiftlog/config` does not exist
- **THEN** default values are used (debug: false, notes_ref: empty string)

#### Scenario: Debug logging reads config
- **WHEN** any shiftlog command runs with debug logging enabled
- **THEN** diagnostic messages are written to stderr with "shiftlog: debug:" prefix

### Requirement: Debug logging output
Shiftlog MUST support optional debug logging to stderr for troubleshooting.

#### Scenario: Debug messages on stderr
- **WHEN** debug is enabled in `.shiftlog/config`
- **THEN** shiftlog writes diagnostic messages to stderr during operations

#### Scenario: Debug disabled by default
- **WHEN** debug is not configured or set to false
- **THEN** no debug messages are written

