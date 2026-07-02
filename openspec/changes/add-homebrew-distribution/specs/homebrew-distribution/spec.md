## ADDED Requirements

### Requirement: Homebrew Tap Installation
The project SHALL publish a Homebrew formula to a tap repository so that users can install shiftlog via `brew install re-cinq/tap/shiftlog`.

#### Scenario: Fresh install via Homebrew
- **WHEN** a user runs `brew install re-cinq/tap/shiftlog`
- **THEN** the latest released version of the shiftlog binary is installed and available on their PATH

#### Scenario: Upgrade via Homebrew
- **WHEN** a new release is published and a user runs `brew upgrade shiftlog`
- **THEN** the binary is updated to the latest version

### Requirement: Automatic Formula Publishing
GoReleaser SHALL automatically generate and push an updated Homebrew formula to the tap repository on every non-prerelease GitHub release.

#### Scenario: Release triggers formula update
- **WHEN** GoReleaser creates a new stable GitHub release
- **THEN** it pushes an updated `shiftlog.rb` formula to the `homebrew-tap` repository with correct archive URLs and SHA256 checksums

#### Scenario: Prerelease skips formula update
- **WHEN** GoReleaser creates a prerelease GitHub release
- **THEN** the Homebrew formula is NOT updated

### Requirement: macOS and Linux Support
The Homebrew formula SHALL support both macOS and Linux across amd64 and arm64 architectures.

#### Scenario: macOS arm64 install
- **WHEN** a user installs via Homebrew on macOS with Apple Silicon
- **THEN** the darwin/arm64 binary is installed

#### Scenario: Linux amd64 install
- **WHEN** a user installs via Homebrew on Linux x86_64
- **THEN** the linux/amd64 binary is installed
