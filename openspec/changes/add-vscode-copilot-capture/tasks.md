## 1. VS Code Hook File Support
- [ ] 1.1 Add `VSCodeHookEntry` struct with `Bash`, `Powershell`, `Cwd`, `TimeoutSec` fields
- [ ] 1.2 Add `VSCodeHooksFile` struct with `Version` and `Hooks` map
- [ ] 1.3 Implement `ReadVSCodeHooksFile()` to parse `.github/hooks/hooks.json`
- [ ] 1.4 Implement `WriteVSCodeHooksFile()` to write `.github/hooks/hooks.json`
- [ ] 1.5 Implement `AddShiftlogVSCodeHooks()` to merge shiftlog hooks into existing file

## 2. Init Command Integration
- [ ] 2.1 Add `--vscode` flag to `shiftlog init` when `--agent=copilot`
- [ ] 2.2 Modify `ConfigureHooks()` to accept `vscode` parameter
- [ ] 2.3 Add auto-detection logic (check which hook file already exists)
- [ ] 2.4 Add warning when `--vscode` used that hooks.json must be on default branch

## 3. Doctor Validation
- [ ] 3.1 Add VS Code hook file validation to `shiftlog doctor`
- [ ] 3.2 Detect which hook format is in use and validate accordingly

## 4. Tests
- [ ] 4.1 Unit tests for VS Code hook file read/write/merge (`internal/agent/copilot/hooks_test.go`)
- [ ] 4.2 Acceptance tests for `shiftlog init --agent=copilot --vscode`
- [ ] 4.3 Acceptance tests for `shiftlog doctor` with VS Code hooks
- [ ] 4.4 Regression test: `shiftlog init --agent=copilot` still creates CLI format

## 5. Validation
- [ ] 5.1 `go vet ./...` passes
- [ ] 5.2 `go test ./internal/agent/copilot/...` passes
- [ ] 5.3 `go test ./tests/acceptance/...` passes
