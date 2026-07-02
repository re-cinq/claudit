## ADDED Requirements
### Requirement: VS Code Copilot Hook Configuration
The `init` command SHALL configure VS Code coding agent hooks when `--agent=copilot --vscode` is specified.

#### Scenario: VS Code hook installation
- **WHEN** user runs `shiftlog init --agent=copilot --vscode`
- **THEN** shiftlog creates or updates `.github/hooks/hooks.json` with `postToolUse`, `sessionStart`, and `sessionEnd` hooks
- **AND** each hook entry includes both `bash` and `powershell` command fields

#### Scenario: Merge with existing hooks.json
- **WHEN** `.github/hooks/hooks.json` already exists with other hooks
- **THEN** shiftlog merges its hooks without removing existing entries
- **AND** existing hook entries for other tools are preserved

#### Scenario: Default branch warning
- **WHEN** user runs `shiftlog init --agent=copilot --vscode`
- **THEN** a message is displayed warning that `.github/hooks/hooks.json` must be committed to the default branch for hooks to activate

#### Scenario: Auto-detect VS Code format
- **WHEN** user runs `shiftlog init --agent=copilot` without `--vscode`
- **AND** `.github/hooks/hooks.json` already exists
- **THEN** shiftlog uses the VS Code hook format automatically

#### Scenario: CLI format backward compatibility
- **WHEN** user runs `shiftlog init --agent=copilot` without `--vscode`
- **AND** `.github/hooks/hooks.json` does not exist
- **THEN** shiftlog uses the CLI hook format (`.github/hooks/shiftlog.json`) as before
