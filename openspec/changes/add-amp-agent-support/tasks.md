## 1. Phase 1: Agent Registration and Core Types

- [ ] 1.1 Add `Amp Name = "amp"` constant to `internal/agent/agent.go`
- [ ] 1.2 Add Amp blank import to all cmd files that register agents (`init.go`, `store.go`, `show.go`, `resume.go`, `doctor.go`)
- [ ] 1.3 Update `init.go` agent flag help text to include `amp` in the list

## 2. Phase 2: Amp Agent Implementation

- [ ] 2.1 Create `internal/agent/amp/amp.go` -- Agent interface implementation (Name, DisplayName, ConfigureHooks no-op, DiagnoseHooks, ParseHookInput, IsCommitCommand, ParseTranscript, ParseTranscriptFile, DiscoverSession, RestoreSession, ResumeCommand, ToolAliases, init Register)
- [ ] 2.2 Create `internal/agent/amp/amp_test.go` -- unit tests (ParseTranscript with stream-json NDJSON, IsCommitCommand, ParseHookInput, ToolAliases)

## 3. Phase 3: Test Fixtures and Acceptance Tests

- [ ] 3.1 Create `tests/acceptance/testutil/amp_fixtures.go` -- SampleAmpTranscript, SampleAmpHookInput, SampleAmpHookInputNonShell, ampPrepareTranscript, ampReadRestoredTranscript
- [ ] 3.2 Add `AmpTestConfig()` and include it in `AllAgentConfigs()`
- [ ] 3.3 Verify shared acceptance tests pass with `IsHookless: true` for Amp (init, store, doctor)

## 4. Phase 4: Verification

- [ ] 4.1 Run `go test ./internal/agent/amp/...` -- unit tests pass
- [ ] 4.2 Run `go test ./tests/acceptance/...` -- all agent acceptance tests pass (including Amp)
- [ ] 4.3 Run `go build ./...` -- project compiles cleanly
