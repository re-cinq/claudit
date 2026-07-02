## ADDED Requirements

### Requirement: Amp Transcript Parsing
The system SHALL parse Amp's stream-JSON NDJSON format into the common transcript type.

#### Scenario: Parse Amp stream-JSON NDJSON
- **WHEN** an Amp stream-json output file is read
- **THEN** `system` type lines are used to extract session metadata (thread ID) and otherwise skipped
- **AND** `message` lines with `role: "user"` are mapped to user transcript entries
- **AND** `message` lines with `role: "assistant"` are mapped to assistant transcript entries
- **AND** content blocks (text, tool_use, tool_result, thinking) are correctly mapped to the common ContentBlock type

#### Scenario: Parse Amp tool use as tool use content block
- **WHEN** a `message` line contains a `tool_use` content block
- **THEN** the `name` field is mapped via `ToolAliases()` (e.g., `terminal` -> `Bash`)
- **AND** the `input` JSON is preserved as tool input

#### Scenario: Extract usage metrics from result messages
- **WHEN** a `result` type line is parsed
- **THEN** usage metrics (input_tokens, output_tokens, cache read/write tokens) are extracted
- **AND** the model identifier is captured if present

### Requirement: Amp Commit Detection
The system SHALL detect git commit commands from Amp tool invocations.

#### Scenario: Detect git commit from terminal tool
- **WHEN** an Amp hook input has tool name `terminal`
- **AND** the command contains `git commit`
- **THEN** the system identifies it as a commit command

#### Scenario: Ignore non-terminal tools
- **WHEN** an Amp hook input has a tool name not in the terminal tool list
- **THEN** the system does not treat it as a commit command

## MODIFIED Requirements

### Requirement: Hook handler for commit detection
The `shiftlog store` command MUST process hook events from the configured agent and detect git commits.

#### Scenario: Agent-specific hook input parsing (Amp)
- **WHEN** `shiftlog store --agent=amp` is invoked with hook input
- **THEN** the hook input is parsed using Amp's format
- **AND** `terminal` tool is matched for commit detection
