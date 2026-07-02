# Change: Add GitHub Release Pipeline

## Why
Currently, shiftlog has no automated testing, versioning, or release process. Building and distributing binaries requires manual intervention, making releases error-prone and labor-intensive. Teams cannot easily install shiftlog from GitHub releases.

## What Changes
- Add GitHub Actions CI workflow for automated testing on PRs/pushes
- Add GoReleaser configuration for cross-platform binary builds
- Add manual version bump workflow (patch/minor/major)
- Add release workflow triggered by version tags
- Add integration test workflow with `CLAUDE_CODE_OAUTH_TOKEN`
- Enable local testing with `act` before pushing

## Impact
- Affected specs: New `ci-cd` capability
- Affected code: New `.github/workflows/`, `.goreleaser.yml`, `.actrc`
- New dependencies: GoReleaser, golangci-lint (CI only)
