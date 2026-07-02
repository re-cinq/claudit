<!-- OPENSPEC:START -->
# OpenSpec Instructions

These instructions are for AI assistants working in this project.

Always open `@/openspec/AGENTS.md` when the request:
- Mentions planning or proposals (words like proposal, spec, change, plan)
- Introduces new capabilities, breaking changes, architecture shifts, or big performance/security work
- Sounds ambiguous and you need the authoritative spec before coding

Use `@/openspec/AGENTS.md` to learn:
- How to create and apply change proposals
- Spec format and conventions
- Project structure and guidelines

Keep this managed block so 'openspec update' can refresh the instructions.

<!-- OPENSPEC:END -->

# Integration Tests

When running integration tests, use `CLAUDE_CODE_OAUTH_TOKEN` (not `ANTHROPIC_API_KEY`):

```bash
go build -o shiftlog . && SHIFTLOG_BINARY=./shiftlog go test ./tests/integration/... -v -timeout 600s
```

Tests skip gracefully when env vars or agent binaries are missing.

# Beads Task Tracking

Automatically manage Beads issues without requiring user commands:
- Create issues (`bd create`) when starting non-trivial work
- Update status (`bd update --status=in_progress`) when beginning a task
- Close issues (`bd close`) when work is complete
- Use `bd ready` to find and prioritize available work
- The user should not need to give `bd` commands — handle this proactively