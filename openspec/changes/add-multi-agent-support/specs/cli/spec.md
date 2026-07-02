## ADDED Requirements

### Requirement: Agent Selection
The `init` command SHALL accept an `--agent` flag to specify which coding agent to configure.

#### Scenario: Init with Claude (default)
- **WHEN** user runs `shiftlog init` without `--agent`
- **THEN** Claude Code hooks are configured (current behavior)
- **AND** `.shiftlog/config.json` stores `agent: "claude"`

#### Scenario: Init with Gemini
- **WHEN** user runs `shiftlog init --agent=gemini`
- **THEN** Gemini CLI hooks are configured in `.gemini/settings.json`
- **AND** `.shiftlog/config.json` stores `agent: "gemini"`

#### Scenario: Init with OpenCode
- **WHEN** user runs `shiftlog init --agent=opencode`
- **THEN** a plugin file is generated at `.opencode/plugins/shiftlog.js`
- **AND** `.shiftlog/config.json` stores `agent: "opencode"`

#### Scenario: Invalid agent name
- **WHEN** user runs `shiftlog init --agent=unknown`
- **THEN** an error message lists supported agents: claude, gemini, opencode
- **AND** the command exits with non-zero status

### Requirement: Agent-Specific Hook Configuration
Each agent SHALL have its own hook configuration mechanism.

#### Scenario: Gemini hooks
- **WHEN** configuring Gemini hooks
- **THEN** an `AfterTool` hook is added to `.gemini/settings.json`
- **AND** the hook matches `shell_exec` tool
- **AND** the hook runs `shiftlog store --agent=gemini`

#### Scenario: OpenCode plugin
- **WHEN** configuring OpenCode hooks
- **THEN** a `.opencode/plugins/shiftlog.js` file is generated
- **AND** the plugin listens for `tool.execute.after` events
- **AND** the plugin calls `shiftlog store --agent=opencode`

### Requirement: Agent-Specific Doctor Checks
The `doctor` command SHALL validate configuration for the configured agent.

#### Scenario: Doctor with Gemini
- **WHEN** user runs `shiftlog doctor` in a Gemini-configured repo
- **THEN** the command checks `.gemini/settings.json` for correct hooks
- **AND** reports OK or FAIL for each hook

#### Scenario: Doctor with OpenCode
- **WHEN** user runs `shiftlog doctor` in an OpenCode-configured repo
- **THEN** the command checks `.opencode/plugins/shiftlog.js` exists and is correct
- **AND** reports OK or FAIL

### Requirement: Gemini Session Discovery
The system SHALL discover active Gemini CLI sessions for conversation capture.

#### Scenario: Discover Gemini session
- **WHEN** `shiftlog store --agent=gemini` is invoked
- **THEN** sessions are discovered at `~/.gemini/tmp/<project_hash>/chats/`

### Requirement: OpenCode Session Discovery
The system SHALL discover active OpenCode CLI sessions from its SQLite database.

#### Scenario: Discover OpenCode session
- **WHEN** `shiftlog store --agent=opencode` is invoked
- **THEN** sessions are read from the SQLite database at `~/.local/share/opencode/storage`

## MODIFIED Requirements

### Requirement: Session Tracking
The system SHALL track active coding agent sessions to enable conversation capture for manual commits.

#### Scenario: Session start tracking
- **WHEN** a coding agent starts a session in a shiftlog-enabled repository
- **THEN** the session tracking hook writes session info to `.shiftlog/active-session.json`
- **AND** the file contains `session_id`, `transcript_path`, `started_at`, `project_path`, and `agent`

#### Scenario: Session end cleanup
- **WHEN** a coding agent ends a session in a shiftlog-enabled repository
- **THEN** the session tracking hook removes `.shiftlog/active-session.json`

#### Scenario: Stale session detection
- **WHEN** reading active session state
- **AND** the transcript file has not been modified in 10+ minutes
- **THEN** the session is considered inactive

### Requirement: Claude Code Session Hooks
The `init` command SHALL configure Claude Code hooks for session tracking when `--agent=claude` (or no agent specified).

#### Scenario: SessionStart hook installation
- **WHEN** user runs `shiftlog init` or `shiftlog init --agent=claude`
- **THEN** a `SessionStart` hook is added to `.claude/settings.local.json`
- **AND** the hook runs `shiftlog session-start`

#### Scenario: SessionEnd hook installation
- **WHEN** user runs `shiftlog init` or `shiftlog init --agent=claude`
- **THEN** a `SessionEnd` hook is added to `.claude/settings.local.json`
- **AND** the hook runs `shiftlog session-end`

### Requirement: Doctor Validation
The `doctor` command SHALL validate configuration for the configured agent.

#### Scenario: Check session hooks
- **WHEN** user runs `shiftlog doctor`
- **THEN** the command checks for the appropriate hooks based on configured agent
- **AND** reports OK or FAIL for each

#### Scenario: Check post-commit hook
- **WHEN** user runs `shiftlog doctor`
- **THEN** the command checks for `post-commit` hook in `.git/hooks/`
- **AND** reports OK or FAIL based on presence and content
