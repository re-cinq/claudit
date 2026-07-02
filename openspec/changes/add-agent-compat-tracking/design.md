## Context

Shiftlog integrates with 5 npm-distributed agent CLIs. New agent versions are published continuously (sometimes daily for active projects like opencode-ai). The existing CI workflow uses pinned versions in shell commands with no automated tracking. When an upstream breaking change occurs, it is discovered only when a user files a bug.

The Concourse CI analogy maps cleanly to GitHub Actions:
- **Resource** → `.github/agent-versions.json` (committed version state)
- **Resource Check** → `check-agent-versions.yml` (scheduled npm poll)
- **Job** → `compat-test.yml` (reusable workflow with install → test → update/fix)

## Goals / Non-Goals

- Goals: Proactive detection of upstream agent CLI version changes; automated compatibility testing; Claude-assisted auto-fix PR on failure; committed state for auditability
- Non-Goals: Testing out-of-constraint versions (e.g., installing a 3.x when constraint is ~2.1); external CI server; per-agent incremental testing (all agents always tested together to catch cross-agent regressions)

## Decisions

1. **Committed JSON state** (`agent-versions.json`): The last-known-good version for each agent is stored in the repo as a committed JSON file. This gives full git history of version progression, is human-readable, and needs no external state store.

2. **Constraint field with npm semver tilde ranges**: Agents with tight coupling to a minor series (claude-code, gemini-cli) use `~2.1` / `~0.29` constraints. The checker queries `npm view <pkg>@<constraint> version --json` to get the latest within the range. Unconstrained agents use `null` constraint and always test against `@latest`.

3. **All agents tested every run**: Every compat test run installs and tests all five agents, not just the changed ones. This catches regressions where agent B breaks when agent A changes (e.g., shared test infra, PATH conflicts).

4. **Claude auto-fix via structured output**: On test failure, `claude --print` is called with a prompt asking it to output modified `.go` files using `=== FILE: path ===` / `=== END FILE ===` delimiters. A Python script parses and writes those files. Only `.go` files are written; path traversal is rejected. If Claude generates no files, a GitHub issue is opened for manual triage instead.

5. **Success path commits to master**: When tests pass, the workflow updates `agent-versions.json` and commits directly to master with `[skip ci]`. This is safe because: (a) the commit is a pure data change, (b) `[skip ci]` prevents infinite loops, (c) the workflow only runs on schedule/dispatch, not on push.

6. **`secrets: inherit`**: The checker workflow calls compat-test with `secrets: inherit` so all API keys (CLAUDE_CODE_OAUTH_TOKEN, GEMINI_API_KEY, etc.) are available without re-declaring them.

## Risks / Trade-offs

- **Branch protection**: Direct push to master from the workflow requires that `github-actions[bot]` is allowed to bypass branch protection rules (or that master is unprotected). If needed, the success path can be changed to open a PR instead.
- **Claude auto-fix quality**: The auto-fix may generate incorrect Go code. The PR review step acts as a human gate — the bot opens the PR but a human approves it.
- **npm availability**: If the npm registry is unavailable during the daily check, the Python script falls back to the last-known-good version, producing no false positives.
- **`[skip ci]` tag**: Relies on GitHub's convention for skipping CI on bot commits. If the project ever removes this check, the success commit would trigger a new CI run (harmless but noisy).

## Open Questions

- Should the success path open a PR instead of committing directly to master? (Safer with branch protection, but adds delay.)
- Should we detect out-of-constraint releases (e.g., claude-code 3.0) and open informational issues? (Deferred to a follow-up change.)
