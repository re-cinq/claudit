## Context
shiftlog's git layer (notes, compression, sync, remap) is already agent-agnostic. The coupling is in 4 areas: hook configuration, session discovery, transcript parsing, and resume. An interface pattern cleanly separates these.

## Goals / Non-Goals
- Goals: Support Gemini CLI and OpenCode CLI alongside Claude Code; shared acceptance tests; backward compatibility
- Non-Goals: Renaming "shiftlog"; auto-detecting which agent is running; supporting agents beyond these three initially

## Research Findings

### Gemini CLI
- Sessions: `~/.gemini/tmp/<project_hash>/chats/` (JSONL files)
- Hooks: 11 event types in `~/.gemini/settings.json` under `hooks` key. `AfterTool` with `shell_exec` matcher
- Very similar to Claude — easiest to add

### OpenCode CLI
- Sessions: SQLite database at `~/.local/share/opencode/storage`
- Hooks: Plugin system (JS/TS files in `.opencode/plugins/`), not JSON config
- Events: `tool.execute.after`, `session.created`, etc.
- Most different — needs SQLite reader + plugin generator

## Decisions
- **Single notes ref**: `refs/notes/claude-conversations` stores all agents' conversations. `Agent` field in `StoredConversation` enables filtering. No ref proliferation.
- **Normalize transcripts**: Each agent's parser converts to a common `Transcript` type. Storage layer never changes.
- **Explicit `--agent` flag**: `shiftlog init --agent=gemini` sets agent in config. Hook commands include `--agent=X`. No runtime binary sniffing.
- **Plugin file for OpenCode**: Since OpenCode uses plugins not JSON hooks, `shiftlog init --agent=opencode` generates a JS plugin file.
- **Pure Go SQLite**: Use `modernc.org/sqlite` (no CGO) for OpenCode support.

## Risks / Trade-offs
- Adding SQLite dependency increases binary size → Mitigation: pure Go driver is ~5MB, acceptable
- Gemini/OpenCode APIs may change → Mitigation: version-specific parsers, fixture-based tests
- Shared test framework adds complexity → Mitigation: worth it for feature parity guarantees

## Testing: Shared Acceptance Tests

Each agent implements an `AgentFixtures` interface:
```go
type AgentFixtures interface {
    AgentName() string
    SampleTranscript() []byte
    SampleHookInput(sessionID, transcriptPath string) []byte
    SetupSessionDir(tmpHome, projectPath, sessionID string, transcript []byte) string
    ExpectedToolNames() []string
}
```

Shared test functions (init, store, list, show, resume) accept fixtures and run identical scenarios. Each agent's test file is thin — just providing fixture data.

## Open Questions
- Should `shiftlog list` default to showing all agents or filter by configured agent?
- Should we rename the notes ref from `claude-conversations` to something generic?
