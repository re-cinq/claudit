## ADDED Requirements
### Requirement: Conversation Summarise Command
The CLI SHALL provide a `summarise` command (alias `tldr`) that produces an LLM-generated summary of a stored conversation by delegating to the user's coding agent in non-interactive mode.

#### Scenario: Basic summarise
- **WHEN** user runs `shiftlog summarise` and HEAD has a stored conversation
- **THEN** the transcript is extracted, filtered, and sent as a prompt to the configured agent
- **AND** the agent's summary output is printed to stdout

#### Scenario: Summarise with alias
- **WHEN** user runs `shiftlog tldr`
- **THEN** it behaves identically to `shiftlog summarise`

#### Scenario: Summarise specific commit
- **WHEN** user runs `shiftlog summarise <ref>` where `<ref>` is a valid commit with a conversation
- **THEN** the summary is generated for that commit's conversation

#### Scenario: Agent override
- **WHEN** user runs `shiftlog summarise --agent=claude`
- **THEN** the specified agent is used for summarisation instead of the stored conversation's agent

#### Scenario: Unsupported agent error
- **WHEN** the resolved agent does not implement the `Summariser` interface
- **THEN** an error is returned suggesting `--agent=claude` as an alternative

#### Scenario: Agent binary not found
- **WHEN** the agent binary is not in PATH
- **THEN** an error is returned indicating the binary was not found

#### Scenario: No conversation found
- **WHEN** user runs `shiftlog summarise <ref>` and no conversation exists for that commit
- **THEN** an error message indicates no conversation was found

#### Scenario: Empty transcript
- **WHEN** the stored conversation has no user or assistant messages
- **THEN** an error indicates the transcript is empty

#### Scenario: Timeout
- **WHEN** the agent process takes longer than 120 seconds
- **THEN** the process is killed and a timeout error is returned

### Requirement: Summarise Spinner
The `summarise` command SHALL display a spinner on stderr while waiting for the agent to respond.

#### Scenario: Spinner on TTY
- **WHEN** stderr is a TTY
- **THEN** an animated spinner is displayed during agent execution

#### Scenario: Spinner suppressed in pipes
- **WHEN** stderr is not a TTY (piped or redirected)
- **THEN** no spinner is displayed

### Requirement: Summarise Debug Mode
The `summarise` command SHALL support debug output via `shiftlog debug --on`.

#### Scenario: Debug mode streams agent stderr
- **WHEN** debug mode is enabled
- **THEN** the agent's stderr is piped to shiftlog's stderr
- **AND** diagnostic messages are printed showing the agent command and prompt size

### Requirement: Summariser Interface
Agents that support non-interactive summarisation SHALL implement the optional `Summariser` interface.

#### Scenario: Claude Code supports summarisation
- **WHEN** Claude Code agent is checked for `Summariser` interface
- **THEN** it implements `SummariseCommand()` returning `"claude", ["-p", "--output-format", "text"]`

#### Scenario: Codex supports summarisation
- **WHEN** Codex agent is checked for `Summariser` interface
- **THEN** it implements `SummariseCommand()` returning `"codex", ["-q"]`

#### Scenario: Non-supporting agents
- **WHEN** Copilot, Gemini, or OpenCode agents are checked for `Summariser` interface
- **THEN** they do not implement it

### Requirement: Summary Prompt Construction
The prompt builder SHALL extract relevant content from transcripts within a character budget.

#### Scenario: Content filtering
- **WHEN** building a summary prompt from a transcript
- **THEN** user text, assistant text, and tool use names are included
- **AND** thinking blocks, tool results, tool inputs, and system messages are excluded

#### Scenario: Truncation from beginning
- **WHEN** the filtered transcript exceeds the character budget (50,000 chars)
- **THEN** earlier entries are dropped and a note indicates truncation
- **AND** the most recent entries are preserved
