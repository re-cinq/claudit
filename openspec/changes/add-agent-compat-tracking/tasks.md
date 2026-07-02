## 1. Version State File

- [x] 1.1 Create `.github/agent-versions.json` with `package`, `constraint`, and `last-known-good` fields for all five agents (claude-code, copilot, gemini-cli, opencode-ai, codex)
- [x] 1.2 Populate `last-known-good` with current published versions queried from npm

## 2. Version Checker Workflow

- [x] 2.1 Create `.github/workflows/check-agent-versions.yml` with `schedule` (daily) and `workflow_dispatch` (manual, with `force_test` input) triggers
- [x] 2.2 Add `check-versions` job: query npm for the latest version matching each agent's constraint (or unconstrained latest), compare to `last-known-good`, output `has_new_versions` and `resolved_versions`
- [x] 2.3 Add `run-compat-test` job: call `compat-test.yml` via `workflow_call` when new versions are detected or `force_test` is set, passing resolved versions and inheriting secrets

## 3. Compatibility Test Workflow

- [x] 3.1 Create `.github/workflows/compat-test.yml` with `workflow_call` (from checker) and `workflow_dispatch` (manual) triggers
- [x] 3.2 Add `resolve` step: use provided versions or resolve npm latest when running standalone
- [x] 3.3 Add install step: install all five agents at their resolved versions via `npm install -g`
- [x] 3.4 Add build + test steps: `CGO_ENABLED=0 go build` then full integration suite with output captured to `/tmp/test-output.txt`
- [x] 3.5 Add success path: update `last-known-good` in `agent-versions.json`, commit back to master with `[skip ci]` tag
- [x] 3.6 Add failure path: write structured prompt, call `claude --print`, parse `=== FILE: ... ===` output, write fixed files, commit to `compat/fix-<sha>` branch, open PR via `gh pr create`; fall back to opening a GitHub issue if Claude generates no file changes

## 4. CI Workflow Update

- [x] 4.1 Add `pull-requests: write` to `.github/workflows/ci.yml` permissions block

## 5. OpenSpec

- [x] 5.1 Create `openspec/changes/add-agent-compat-tracking/proposal.md`
- [x] 5.2 Create `openspec/changes/add-agent-compat-tracking/tasks.md`
- [x] 5.3 Create `openspec/changes/add-agent-compat-tracking/design.md`
- [x] 5.4 Create `openspec/changes/add-agent-compat-tracking/specs/ci/spec.md` with new `ci` capability requirements
- [x] 5.5 Run `openspec validate add-agent-compat-tracking --strict` and fix any issues
