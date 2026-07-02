## 1. Phase 1: Agent Registration and Core Types

- [x] 1.1 Add `Codex Name = "codex"` constant to `internal/agent/agent.go`
- [x] 1.2 Add Codex blank import to all cmd files that register agents (`init.go`, `store.go`, `show.go`, `resume.go`, `doctor.go`)
- [x] 1.3 Update `init.go` agent flag help text to include `codex` in the list

## 2. Phase 2: Codex Agent Implementation

- [x] 2.1 Create `internal/agent/codex/session.go` — session discovery helpers (GetCodexHome, GetSessionsDir, FindRecentRollout, ParseSessionMeta)
- [x] 2.2 Create `internal/agent/codex/codex.go` — Agent interface implementation (Name, DisplayName, ConfigureHooks no-op, DiagnoseHooks, ParseHookInput, IsCommitCommand, ParseTranscript, ParseTranscriptFile, DiscoverSession, RestoreSession, ResumeCommand, ToolAliases, init Register)
- [x] 2.3 Create `internal/agent/codex/codex_test.go` — unit tests (ParseTranscript, IsCommitCommand, ParseHookInput, ToolAliases)

## 3. Phase 3: Agent-Aware Manual Store

- [x] 3.1 Modify `cmd/store.go:runManualStore()` — resolve agent first, call `ag.DiscoverSession(projectPath)`, fall back to `session.DiscoverSession()` for backward compat

## 4. Phase 4: Test Fixtures and Acceptance Tests

- [x] 4.1 Create `tests/acceptance/testutil/codex_fixtures.go` — SampleCodexTranscript, SampleCodexHookInput, SampleCodexHookInputNonShell, codexPrepareTranscript, codexReadRestoredTranscript, codexSessionDir
- [x] 4.2 Add `IsHookless bool` field to `AgentTestConfig` struct in `tests/acceptance/testutil/agent_config.go`
- [x] 4.3 Add `CodexTestConfig()` and include it in `AllAgentConfigs()`
- [x] 4.4 Update shared acceptance tests to handle `IsHookless` agents (skip hook/plugin file verification, add negative assertion for spurious files)

## 5. Phase 5: Integration Test

- [x] 5.1 Create `tests/integration/codex_integration_test.go` — end-to-end test with real Codex CLI (requires OPENAI_API_KEY, skippable with SKIP_CODEX_INTEGRATION=1)

## 6. Phase 6: Verification

- [x] 6.1 Run `go test ./internal/agent/codex/...` — unit tests pass
- [x] 6.2 Run `go test ./tests/acceptance/...` — all agent acceptance tests pass (including Codex)
- [x] 6.3 Run `go build ./...` — project compiles cleanly
