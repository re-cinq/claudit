## 1. Infrastructure Setup (manual, outside this repo)
- [ ] 1.1 Create `re-cinq/homebrew-tap` GitHub repository with a `Formula/` directory
- [ ] 1.2 Create a GitHub personal access token (or fine-grained token) with push access to the tap repo
- [ ] 1.3 Add the token as `HOMEBREW_TAP_GITHUB_TOKEN` secret in the `shiftlog` repo's Actions settings

## 2. GoReleaser Configuration
- [ ] 2.1 Add `brews` section to `.goreleaser.yml` targeting the tap repo, filtering to macOS/Linux archives, skipping prereleases

## 3. Documentation
- [ ] 3.1 Add Homebrew install instructions to README.md (tap + install command)

## 4. Validation
- [ ] 4.1 Run `goreleaser check` to validate the updated config
- [ ] 4.2 Trigger a release and verify the formula is pushed to the tap repo
- [ ] 4.3 Test `brew install re-cinq/tap/shiftlog` on a clean machine
