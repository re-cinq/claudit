## MODIFIED Requirements

### Requirement: Agent Selection
The `init` command SHALL accept an `--agent` flag to specify which coding agent to configure.

#### Scenario: Init with Codex
- **WHEN** user runs `shiftlog init --agent=codex`
- **THEN** git hooks are installed (pre-push, post-merge, post-checkout, post-commit)
- **AND** no agent-specific hook or plugin file is created (Codex is hookless)
- **AND** `.shiftlog/config.json` stores `agent: "codex"`

#### Scenario: Invalid agent name (updated list)
- **WHEN** user runs `shiftlog init --agent=unknown`
- **THEN** an error message lists supported agents: claude, codex, gemini, opencode
- **AND** the command exits with non-zero status

### Requirement: Agent-Specific Hook Configuration
Each agent SHALL have its own hook configuration mechanism.

#### Scenario: Codex no-op hooks
- **WHEN** configuring Codex hooks
- **THEN** no hook or plugin files are created
- **AND** `ConfigureHooks()` returns nil (success)
- **AND** conversation capture relies on the post-commit git hook

### Requirement: Agent-Specific Doctor Checks
The `doctor` command SHALL validate configuration for the configured agent.

#### Scenario: Doctor with Codex
- **WHEN** user runs `shiftlog doctor` in a Codex-configured repo
- **THEN** the command checks that the `codex` binary is in PATH
- **AND** reports OK or FAIL

### Requirement: Codex Session Discovery
The system SHALL discover active Codex CLI sessions for conversation capture.

#### Scenario: Discover Codex session
- **WHEN** `shiftlog store --manual --agent=codex` is invoked
- **THEN** sessions are discovered by scanning `~/.codex/sessions/` recursively for recently-modified `.jsonl` rollout files
- **AND** the `session_meta` line is read to match `cwd` to the current project path

#### Scenario: Codex session with CODEX_HOME override
- **WHEN** the `CODEX_HOME` environment variable is set
- **THEN** sessions are scanned under `$CODEX_HOME/sessions/` instead of `~/.codex/sessions/`

## ADDED Requirements

### Requirement: Agent-Aware Manual Store
The `runManualStore()` function SHALL use the configured agent's `DiscoverSession()` method instead of hardcoded Claude session discovery.

#### Scenario: Manual store with Codex agent
- **WHEN** a post-commit hook fires in a Codex-configured repo
- **AND** `shiftlog store --manual` is invoked
- **THEN** the configured agent (Codex) is resolved from `.shiftlog/config.json`
- **AND** `ag.DiscoverSession(projectPath)` is called to find the active session
- **AND** the conversation is stored with `agent: "codex"`

#### Scenario: Manual store backward compatibility
- **WHEN** a post-commit hook fires in a Claude-configured repo
- **AND** `ag.DiscoverSession()` returns nil
- **THEN** the system falls back to `session.DiscoverSession()` (existing Claude-specific logic)
- **AND** behavior is unchanged from before this change
