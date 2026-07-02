# Change: Add Homebrew distribution

## Why
Users currently must download release archives manually from GitHub and place the binary in their PATH. Homebrew is the standard package manager on macOS (and widely used on Linux), so publishing a tap lets users install with `brew install` and get automatic upgrades via `brew upgrade`.

## What Changes
- Add a `brews` section to `.goreleaser.yml` that auto-publishes a Homebrew formula on each release
- Create a new GitHub repository (`re-cinq/homebrew-tap`) to host the formula
- Update README with Homebrew install instructions

## Impact
- Affected specs: none (new capability)
- Affected code: `.goreleaser.yml`, `README.md`
- Requires creating an external repo (`homebrew-tap`) and a `HOMEBREW_TAP_GITHUB_TOKEN` secret in CI
