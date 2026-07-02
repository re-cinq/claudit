# Change: Retroactively spec undocumented features

## Why
Several features were implemented without corresponding specs during rapid development. This change adds specs for all undocumented functionality to bring the spec directory in sync with the codebase.

## What Changes
- Add spec for `claudit doctor` base command (git repo, PATH, hook, and git hook checks)
- Add spec for `claudit debug` command (toggle debug logging)
- Add spec for configuration system (`.claudit/config`)
- Add spec for `--remote` flag on sync commands
- Add spec for `--no-browser` flag on serve command
- Add spec for `--force` flag on resume command
- Add spec for init PATH check and gitignore management

## Impact
- Affected specs: cli-foundation, session-resume, web-visualization
- Affected code: none (retroactive documentation only)
