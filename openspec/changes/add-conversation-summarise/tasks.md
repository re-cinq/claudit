## 1. Core Implementation
- [x] 1.1 Create `internal/agent/summariser.go` with `Summariser` interface and `BuildSummaryPrompt`
- [x] 1.2 Add `SummariseCommand()` to Claude Code agent
- [x] 1.3 Add `SummariseCommand()` to Codex agent
- [x] 1.4 Create `internal/cli/spinner.go` with TTY-aware spinner
- [x] 1.5 Create `cmd/summarise.go` with command, `--agent` flag, alias `tldr`

## 2. Tests
- [x] 2.1 Unit tests for prompt builder (`internal/agent/summariser_test.go`)
- [x] 2.2 Acceptance tests (`tests/acceptance/summarise_test.go`)

## 3. Validation
- [x] 3.1 `go vet ./...` passes
- [x] 3.2 `go test ./internal/agent/...` passes
- [x] 3.3 `go test ./tests/acceptance/...` passes
