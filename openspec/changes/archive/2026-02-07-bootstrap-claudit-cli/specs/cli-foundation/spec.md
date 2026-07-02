# CLI Foundation

Provides the core CLI structure, common utilities, and project initialization for Claudit.

## ADDED Requirements

### Requirement: CLI entry point with Cobra
The CLI MUST provide a root command with version information and subcommands.

#### Scenario: User runs claudit with no arguments
- Given the user has claudit installed
- When the user runs `claudit`
- Then the CLI displays help text listing available commands

#### Scenario: User checks version
- Given the user has claudit installed
- When the user runs `claudit --version`
- Then the CLI displays the version number

### Requirement: Initialize repository for claudit
The `claudit init` command MUST configure a git repository for conversation capture.

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

### Requirement: PostToolUse hook configuration
The init command MUST configure Claude Code's hook system correctly.

#### Scenario: Hook configuration structure
- Given `claudit init` has been run
- When examining `.claude/settings.local.json`
- Then it contains a PostToolUse hook matching "Bash" tool
- And the hook command is `claudit store`
- And the hook timeout is 30 seconds

#### Scenario: Preserve existing settings
- Given `.claude/settings.local.json` already exists with other settings
- When the user runs `claudit init`
- Then existing settings are preserved
- And the PostToolUse hook is added or updated

### Requirement: Sync commands
The CLI MUST provide commands for syncing git notes with remotes.

#### Scenario: Push notes to remote
- Given the repository has a remote configured
- When the user runs `claudit sync push`
- Then claudit pushes `refs/notes/claude-conversations` to origin

#### Scenario: Pull notes from remote
- Given the repository has a remote configured
- When the user runs `claudit sync pull`
- Then claudit fetches `refs/notes/claude-conversations` from origin

### Requirement: Git hooks for automatic sync
The init command MUST install git hooks that invoke claudit sync commands.

#### Scenario: Pre-push hook installed
- Given `claudit init` has been run
- When examining `.git/hooks/pre-push`
- Then it calls `claudit sync push`

#### Scenario: Post-merge hook installed
- Given `claudit init` has been run
- When examining `.git/hooks/post-merge`
- Then it calls `claudit sync pull`

#### Scenario: Post-checkout hook installed
- Given `claudit init` has been run
- When examining `.git/hooks/post-checkout`
- Then it calls `claudit sync pull`
