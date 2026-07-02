## MODIFIED Requirements
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

#### Scenario: Check VS Code Copilot hooks
- **WHEN** user runs `shiftlog doctor`
- **AND** the repository is configured with `--agent=copilot`
- **THEN** the command checks for `.github/hooks/hooks.json` or `.github/hooks/shiftlog.json`
- **AND** validates that the detected hook file contains shiftlog entries
- **AND** reports OK or FAIL for Copilot hook configuration

#### Scenario: Summary with fix suggestion
- **WHEN** `shiftlog doctor` detects issues
- **THEN** it prints "Issues found. Run 'shiftlog init' to fix configuration."
- **AND** exits with non-zero status

#### Scenario: All checks pass
- **WHEN** `shiftlog doctor` finds no issues
- **THEN** it prints "All checks passed! Shiftlog is properly configured."
- **AND** exits with zero status
