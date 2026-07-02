# Change: Add Agent Compatibility Tracking (Concourse-style)

## Why
Shiftlog integrates with 5 agent CLIs (claude-code, copilot, gemini-cli, opencode-ai, codex). Currently the CI workflow pins fixed versions at commit time with no mechanism to detect upstream releases. When a new version breaks compatibility (e.g., claude-code 2.1.49 changed stdin behaviour), it is only discovered reactively after the fact. We need a proactive system: track external resources (npm packages), trigger tests when they change, and auto-fix failures via Claude.

## What Changes
- Add `.github/agent-versions.json` — committed JSON state file tracking last-known-good version per agent
- Add `.github/workflows/check-agent-versions.yml` — daily scheduled workflow that polls npm and triggers compat testing when new versions are detected
- Add `.github/workflows/compat-test.yml` — reusable workflow that installs all agents at latest, runs the full integration suite, updates state on success, and opens an auto-fix PR via Claude on failure
- Modify `.github/workflows/ci.yml` — add `pull-requests: write` permission so the compat test can open auto-fix PRs

## Impact
- Affected specs: new `ci` capability
- Affected code: `.github/workflows/ci.yml`
- New files: `.github/agent-versions.json`, `.github/workflows/check-agent-versions.yml`, `.github/workflows/compat-test.yml`
- **Backward compatible**: existing CI behaviour unchanged; compat test runs in parallel
