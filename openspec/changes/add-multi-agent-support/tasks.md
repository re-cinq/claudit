## 1. Phase 1: Extract Agent Interface (refactor only)

- [ ] 1.1 Create `internal/agent/agent.go` with Agent interface and Name type
- [ ] 1.2 Create `internal/agent/transcript.go` — move transcript types from `internal/claude/transcript.go`
- [ ] 1.3 Create `internal/agent/render.go` — move renderer, parameterize tool aliases
- [ ] 1.4 Create `internal/agent/registry.go` — Get/All/Default functions
- [ ] 1.5 Create `internal/agent/claude/` implementing Agent interface (hooks, session, parser)
- [ ] 1.6 Update commands to use Agent interface (`init`, `store`, `resume`, `show`, `doctor`)
- [ ] 1.7 Add `--agent` flag to commands
- [ ] 1.8 Add `Agent` field to config and `StoredConversation`
- [ ] 1.9 Verify all existing tests pass (pure refactoring)

## 2. Phase 2: Add Gemini CLI Support

- [ ] 2.1 Implement Gemini agent (`internal/agent/gemini/`)
- [ ] 2.2 Gemini hooks configuration (`.gemini/settings.json`)
- [ ] 2.3 Gemini session discovery (`~/.gemini/tmp/<hash>/chats/`)
- [ ] 2.4 Gemini transcript parser (JSONL → common Transcript)
- [ ] 2.5 Create shared acceptance test framework (`AgentFixtures` interface)
- [ ] 2.6 Acceptance tests for Gemini (init, store, list, show)
- [ ] 2.7 Refactor existing Claude tests to use shared framework

## 3. Phase 3: Add OpenCode CLI Support

- [ ] 3.1 Add `modernc.org/sqlite` dependency
- [ ] 3.2 Implement OpenCode agent (`internal/agent/opencode/`)
- [ ] 3.3 OpenCode plugin generator (`.opencode/plugins/shiftlog.js`)
- [ ] 3.4 OpenCode SQLite session reader
- [ ] 3.5 OpenCode transcript parser (SQLite → common Transcript)
- [ ] 3.6 Acceptance tests for OpenCode using shared framework
