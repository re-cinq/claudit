## ADDED Requirements

### Requirement: Codex Transcript Parsing
The system SHALL parse Codex CLI's JSONL rollout format into the common transcript type.

#### Scenario: Parse Codex rollout JSONL
- **WHEN** a Codex session rollout file is read
- **THEN** the `session_meta` and `turn_context` lines are skipped
- **AND** `response_item` lines are parsed and normalized to the common `Transcript` type
- **AND** user messages, assistant messages, function calls, and function call outputs are correctly mapped

#### Scenario: Parse Codex function call as tool use
- **WHEN** a `response_item` with `type: "function_call"` is parsed
- **THEN** the `name` field is mapped via `ToolAliases()` (e.g., `shell` -> `Bash`)
- **AND** the `arguments` JSON is preserved as tool input

### Requirement: Codex Commit Detection
The system SHALL detect git commit commands from Codex tool invocations.

#### Scenario: Detect git commit from shell tool
- **WHEN** a Codex hook input has tool name `shell`, `container.exec`, or `shell_command`
- **AND** the command contains `git commit`
- **THEN** the system identifies it as a commit command

#### Scenario: Ignore non-shell tools
- **WHEN** a Codex hook input has a tool name not in the shell tool list
- **THEN** the system does not treat it as a commit command

## MODIFIED Requirements

### Requirement: Hook handler for commit detection
The `shiftlog store` command MUST process hook events from the configured agent and detect git commits.

#### Scenario: Agent-specific hook input parsing (Codex)
- **WHEN** `shiftlog store --agent=codex` is invoked with hook input
- **THEN** the hook input is parsed using Codex's format
- **AND** `shell`, `container.exec`, or `shell_command` tools are matched for commit detection
