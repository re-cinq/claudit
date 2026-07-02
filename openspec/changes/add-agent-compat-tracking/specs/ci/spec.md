## ADDED Requirements

### Requirement: Agent Version State
The system SHALL maintain a committed JSON file (`.github/agent-versions.json`) recording the last-known-good version, npm package name, and optional semver constraint for each supported agent CLI.

#### Scenario: State file contains all agents
- **WHEN** the state file is read
- **THEN** it contains entries for claude-code, copilot, gemini-cli, opencode-ai, and codex with `package`, `constraint`, and `last-known-good` fields

#### Scenario: Constraint field is a valid npm semver tilde range or null
- **WHEN** an agent has a pinned minor series
- **THEN** its `constraint` field is a tilde range string (e.g., `~2.1`)
- **WHEN** an agent tracks latest
- **THEN** its `constraint` field is `null`

### Requirement: Scheduled Version Polling
The system SHALL run a daily GitHub Actions workflow that queries the npm registry for the latest published version of each agent CLI within its declared constraint.

#### Scenario: New version detected
- **WHEN** the npm-resolved version for any agent differs from `last-known-good`
- **THEN** the workflow sets `has_new_versions=true` and triggers the compatibility test

#### Scenario: No new versions
- **WHEN** all resolved versions match `last-known-good`
- **THEN** the compatibility test is NOT triggered and the workflow exits successfully

#### Scenario: Manual force-test
- **WHEN** the checker workflow is dispatched with `force_test=true`
- **THEN** the compatibility test is triggered regardless of version changes

### Requirement: Full-Suite Compatibility Testing
The system SHALL provide a reusable GitHub Actions workflow that installs all five agent CLIs at their resolved versions, builds shiftlog, and runs the full integration test suite.

#### Scenario: All agents installed at resolved versions
- **WHEN** the compat test runs
- **THEN** all five agents are installed via `npm install -g` at the exact versions provided as inputs

#### Scenario: Integration tests pass
- **WHEN** `go test ./tests/integration/...` exits 0
- **THEN** the compat test job succeeds

#### Scenario: Integration tests fail
- **WHEN** `go test ./tests/integration/...` exits non-zero
- **THEN** the compat test job fails after attempting auto-fix

### Requirement: Automatic State Update on Success
The system SHALL update `.github/agent-versions.json` with the tested versions and commit the change back to the default branch when all integration tests pass.

#### Scenario: State file updated after passing tests
- **WHEN** the compat test passes
- **THEN** `last-known-good` for each agent is updated to the tested version
- **AND** the updated file is committed to master with a `[skip ci]` message

### Requirement: Claude Auto-Fix on Failure
The system SHALL attempt to automatically fix compatibility failures using the Claude CLI and open a pull request with the generated changes.

#### Scenario: Claude generates a fix
- **WHEN** integration tests fail
- **AND** `claude --print` produces output with `=== FILE: ... ===` blocks containing valid `.go` file content
- **THEN** those files are written, committed to a `compat/fix-<sha>` branch, and a pull request is opened against master

#### Scenario: Claude generates no fix
- **WHEN** integration tests fail
- **AND** `claude --print` produces no parseable file changes
- **THEN** a GitHub issue is opened with the test failure summary for manual triage

#### Scenario: Unsafe paths are rejected
- **WHEN** Claude output contains file paths with `..` traversal or non-`.go` extensions
- **THEN** those paths are skipped and not written to disk
