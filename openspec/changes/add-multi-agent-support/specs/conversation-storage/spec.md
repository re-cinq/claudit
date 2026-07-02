## ADDED Requirements

### Requirement: Agent Field in Stored Conversation
The `StoredConversation` format SHALL include an `Agent` field identifying which coding agent produced the conversation.

#### Scenario: Store with agent identifier
- **WHEN** storing a conversation
- **THEN** the `StoredConversation` JSON includes an `agent` field (e.g., `"claude"`, `"gemini"`, `"opencode"`)

#### Scenario: Backward compatibility
- **WHEN** reading a stored conversation without an `agent` field
- **THEN** the agent defaults to `"claude"`
- **AND** no error is raised

### Requirement: Gemini Transcript Parsing
The system SHALL parse Gemini CLI's JSONL transcript format into the common transcript type.

#### Scenario: Parse Gemini JSONL
- **WHEN** a Gemini session transcript is read
- **THEN** the JSONL entries are parsed and normalized to the common `Transcript` type
- **AND** tool uses, text messages, and tool results are correctly mapped

### Requirement: OpenCode Transcript Parsing
The system SHALL parse OpenCode CLI's SQLite session data into the common transcript type.

#### Scenario: Parse OpenCode SQLite
- **WHEN** an OpenCode session is read
- **THEN** messages are extracted from the SQLite database
- **AND** normalized to the common `Transcript` type

## MODIFIED Requirements

### Requirement: Hook handler for commit detection
The `shiftlog store` command MUST process hook events from the configured agent and detect git commits.

#### Scenario: Detect git commit command
- Given a coding agent executed a shell command
- When the hook JSON indicates a command containing `git commit`
- Then shiftlog proceeds to store the conversation

#### Scenario: Ignore non-commit commands
- Given a coding agent executed a shell command
- When the hook JSON indicates a command that is not `git commit`
- Then shiftlog exits silently with status 0

#### Scenario: Handle malformed hook input
- Given shiftlog receives invalid JSON on stdin
- When processing the hook
- Then shiftlog logs a warning and exits with status 0
- And does not disrupt the user's workflow

#### Scenario: Agent-specific hook input parsing
- **WHEN** `shiftlog store --agent=gemini` is invoked
- **THEN** the hook input is parsed using Gemini's format
- **AND** `shell_exec` tool is matched for commit detection

### Requirement: Read Claude Code transcript
The store command MUST read the transcript from the path provided by the hook, using the agent-appropriate parser.

#### Scenario: Read transcript from hook-provided path
- Given the hook JSON contains `transcript_path`
- When shiftlog processes the hook
- Then shiftlog reads the file at that path using the configured agent's parser

#### Scenario: Handle missing transcript file
- Given the hook JSON contains a `transcript_path` that doesn't exist
- When shiftlog processes the hook
- Then shiftlog logs a warning and exits with status 0
