## ADDED Requirements

### Requirement: Continuous Integration
The project SHALL have automated testing via GitHub Actions on every PR and push to main.

#### Scenario: PR triggers CI
- **WHEN** a pull request is opened or updated
- **THEN** the CI workflow runs lint, test, and build jobs
- **AND** no secrets are required (safe for fork PRs)

#### Scenario: Push to main triggers CI
- **WHEN** code is pushed to the main branch
- **THEN** the CI workflow runs lint, test, and build jobs

#### Scenario: Lint check
- **WHEN** the lint job runs
- **THEN** golangci-lint analyzes the codebase
- **AND** the job fails if linting errors are found

#### Scenario: Test execution
- **WHEN** the test job runs
- **THEN** unit tests run via `go test ./internal/...`
- **AND** acceptance tests run via `go test ./tests/acceptance/...`

#### Scenario: Build verification
- **WHEN** the build job runs
- **THEN** binaries are built for 5 platforms (darwin/amd64, darwin/arm64, linux/amd64, linux/arm64, windows/amd64)
- **AND** all builds complete successfully

### Requirement: Continuous Delivery
The project SHALL automatically release on every push to main using semantic versioning.

#### Scenario: Auto patch bump on push to main
- **WHEN** code is pushed to the main branch
- **AND** CI passes
- **THEN** the patch version is automatically incremented (e.g., v2.2.0 -> v2.2.1)
- **AND** an annotated git tag is created and pushed
- **AND** a GitHub Release is created with binaries

#### Scenario: Skip release for version tags
- **WHEN** a push to main is a version tag push (from bump workflow)
- **THEN** the auto-release workflow does not create a duplicate tag

#### Scenario: Manual minor version bump
- **WHEN** a maintainer triggers the bump-version workflow with "minor"
- **THEN** the minor version increments and patch resets (e.g., v2.2.5 -> v2.3.0)
- **AND** an annotated git tag is created and pushed

#### Scenario: Manual major version bump
- **WHEN** a maintainer triggers the bump-version workflow with "major"
- **THEN** the major version increments and minor/patch reset (e.g., v2.3.0 -> v3.0.0)
- **AND** an annotated git tag is created and pushed

### Requirement: Automated Releases
The project SHALL automatically create GitHub releases when version tags are pushed.

#### Scenario: Tag triggers release
- **WHEN** a version tag (v*) is pushed
- **THEN** the release workflow triggers
- **AND** GoReleaser builds binaries for all platforms

#### Scenario: Release artifacts
- **WHEN** GoReleaser completes
- **THEN** a GitHub Release is created with:
  - Archives for each platform (tar.gz for Unix, zip for Windows)
  - SHA256 checksums file
  - Auto-generated changelog from commits

#### Scenario: Version injection
- **WHEN** binaries are built
- **THEN** the version is injected via ldflags
- **AND** `shiftlog --version` shows the correct version

### Requirement: Integration Testing
The project SHALL optionally run integration tests that require Claude Code authentication.

#### Scenario: Integration tests on main
- **WHEN** code is pushed to the main branch
- **AND** the repository has `CLAUDE_CODE_OAUTH_TOKEN` secret configured
- **THEN** integration tests run against real Claude Code CLI

#### Scenario: Weekly scheduled tests
- **WHEN** the weekly schedule triggers (Monday 6 AM UTC)
- **THEN** integration tests run to catch regressions

#### Scenario: Skip on forks
- **WHEN** a PR is opened from a fork
- **THEN** integration tests are skipped (no secrets available)

### Requirement: Local Testing with Act
Workflows SHALL be testable locally using nektos/act before pushing.

#### Scenario: Local CI test
- **WHEN** a developer runs `act push -W .github/workflows/ci.yml`
- **THEN** the CI workflow executes locally
- **AND** issues are caught before pushing

#### Scenario: Local release test
- **WHEN** a developer runs `goreleaser release --snapshot --clean`
- **THEN** binaries are built locally without publishing
- **AND** the GoReleaser configuration is validated
