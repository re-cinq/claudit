# Design: GitHub Release Pipeline

## Context
Shiftlog is a Go CLI tool distributed via `go install`. Adding GitHub releases enables binary distribution for users who don't have Go installed. The project needs automated testing to catch regressions and automated releases to reduce manual effort.

## Goals / Non-Goals

**Goals:**
- Automated testing on every PR and push
- One-click version bumping (patch/minor/major)
- Cross-platform binary releases (5 platforms)
- Local workflow testing with `act`

**Non-Goals:**
- Homebrew distribution (deferred)
- Docker image publishing
- homebrew-core submission

## Decisions

### GoReleaser vs Custom Scripts
**Decision:** Use GoReleaser
**Rationale:** Industry standard for Go releases. Handles cross-compilation, checksums, changelog, and archives in one tool. Eliminates ~200 lines of custom scripting.

### Version Bumping Strategy
**Decision:** Continuous delivery with auto patch bump; manual trigger for major/minor only
**Rationale:** Every push to main should release automatically (CD). Patch versions bump automatically. Major/minor bumps are rare and intentional, so they use a manual workflow to signal breaking changes or new features.

### Integration Test Secret
**Decision:** Use `CLAUDE_CODE_OAUTH_TOKEN` (not `ANTHROPIC_API_KEY`)
**Rationale:** OAuth token is what Claude Code users typically have. Can be obtained via `claude setup-token`.

### Local Testing
**Decision:** Support `act` for local workflow testing
**Rationale:** Faster feedback loop, catch issues before pushing, reduce CI costs.

## Risks / Trade-offs

- **Risk:** GoReleaser adds a build dependency
  - **Mitigation:** Only used in CI, not required for local development

- **Risk:** Integration tests need real API token
  - **Mitigation:** Optional workflow, can skip in CI with `SKIP_CLAUDE_INTEGRATION=1`

## Open Questions
None - all decisions made.
