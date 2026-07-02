## MODIFIED Requirements

### Requirement: Agent Selection
The `init` command SHALL accept an `--agent` flag to specify which coding agent to configure.

#### Scenario: Init with Amp
- **WHEN** user runs `shiftlog init --agent=amp`
- **THEN** git hooks are installed (pre-push, post-merge, post-checkout, post-commit)
- **AND** no agent-specific hook or plugin file is created (Amp is hookless)
- **AND** `.shiftlog/config.json` stores `agent: "amp"`

#### Scenario: Invalid agent name (updated list)
- **WHEN** user runs `shiftlog init --agent=unknown`
- **THEN** an error message lists supported agents: claude, codex, copilot, gemini, opencode, amp
- **AND** the command exits with non-zero status

### Requirement: Agent-Specific Hook Configuration
Each agent SHALL have its own hook configuration mechanism.

#### Scenario: Amp no-op hooks
- **WHEN** configuring Amp hooks
- **THEN** no hook or plugin files are created
- **AND** `ConfigureHooks()` returns nil (success)
- **AND** conversation capture relies on the post-commit git hook

### Requirement: Agent-Specific Doctor Checks
The `doctor` command SHALL validate configuration for the configured agent.

#### Scenario: Doctor with Amp
- **WHEN** user runs `shiftlog doctor` in an Amp-configured repo
- **THEN** the command checks that the `amp` binary is in PATH
- **AND** reports OK or FAIL
