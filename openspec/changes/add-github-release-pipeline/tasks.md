# Tasks: GitHub Release Pipeline

## 1. CI Workflow

- [x] 1.1 Create `.github/workflows/ci.yml`
- [x] 1.2 Add `lint` job with golangci-lint
- [x] 1.3 Add `test` job running unit + acceptance tests
- [x] 1.4 Add `build` job with matrix for 5 platforms
- [x] 1.5 Add `release` job that auto-bumps patch version on main

## 2. GoReleaser Configuration

- [x] 2.1 Create `.goreleaser.yml`
- [x] 2.2 Configure builds for darwin/linux/windows x amd64/arm64
- [x] 2.3 Configure archives (tar.gz for Unix, zip for Windows)
- [x] 2.4 Configure checksums and changelog
- [x] 2.5 Validate with `goreleaser check`
- [x] 2.6 Test with `goreleaser release --snapshot --clean`

## 3. Version Bump Workflow (major/minor only)

- [x] 3.1 Create `.github/workflows/bump-version.yml`
- [x] 3.2 Add workflow_dispatch with minor/major dropdown (patch is automatic)
- [x] 3.3 Implement version calculation from latest tag
- [x] 3.4 Create and push annotated git tag

## 4. Release Workflow

- [x] 4.1 Create `.github/workflows/release.yml`
- [x] 4.2 Trigger on `v*` tag push
- [x] 4.3 Run GoReleaser to build + publish

## 5. Integration Test Workflow

- [x] 5.1 Create `.github/workflows/integration.yml`
- [x] 5.2 Trigger on main branch push + weekly schedule + manual
- [x] 5.3 Use `CLAUDE_CODE_OAUTH_TOKEN` secret
- [x] 5.4 Skip on fork PRs (no secrets)

## 6. Local Testing Setup

- [x] 6.1 Create `.actrc` with default settings
- [x] 6.2 GoReleaser config validated locally
- [x] 6.3 Snapshot build tested successfully

## 7. Verification (on GitHub)

- [ ] 7.1 Push to main and verify CI + auto-release triggers
- [ ] 7.2 Verify GitHub Release created with binaries
- [ ] 7.3 Test manual bump-version workflow for minor/major
- [ ] 7.4 Download and test a release binary
