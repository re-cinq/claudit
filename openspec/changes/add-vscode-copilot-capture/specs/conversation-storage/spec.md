## ADDED Requirements
### Requirement: VS Code Coding Agent Hook Input
The `shiftlog store` command SHALL process hook events from VS Code coding agent hooks using the same input format as Copilot CLI hooks.

#### Scenario: Parse VS Code hook input
- **WHEN** `shiftlog store --agent=copilot` receives JSON on stdin from a VS Code coding agent hook
- **AND** the JSON contains `timestamp`, `cwd`, `toolName`, and `toolArgs` fields
- **THEN** the existing `ParseHookInput()` processes it without changes

#### Scenario: Detect commit from VS Code hook
- **WHEN** the hook input indicates a Bash tool execution containing `git commit`
- **THEN** `IsCommitCommand()` detects it and triggers conversation storage

#### Scenario: VS Code transcript format compatibility
- **WHEN** the VS Code coding agent writes `events.jsonl` transcript files
- **THEN** `ParseTranscript()` handles them identically to Copilot CLI transcripts
