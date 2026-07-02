# Design: Manual Commit Capture

## Context
Claudit currently captures conversations only when Claude Code makes commits. The `PostToolUse` hook provides the session ID and transcript path via stdin. For manual commits, we need an alternative discovery mechanism.

Claude Code provides:
- `SessionStart` hook: fires when session begins, provides `session_id`, `transcript_path`, `cwd`
- `SessionEnd` hook: fires when session terminates, provides same fields plus `reason`
- Session index: `~/.claude/projects/{encoded-path}/sessions-index.json` with session metadata

## Goals
- Capture conversations for commits made manually during active Claude sessions
- Capture conversations for commits made shortly after a session ends
- Graceful degradation: skip capture silently when no relevant session exists
- Maintain backwards compatibility with existing `PostToolUse` flow

## Non-Goals
- Capturing conversations for commits made long after sessions end (too ambiguous)
- Matching conversations to commits made before the session started
- Supporting multiple concurrent Claude sessions (edge case, first-match is acceptable)

## Decisions

### Decision 1: Session State File
Store active session info in `.claudit/active-session.json`:
```json
{
  "session_id": "abc123",
  "transcript_path": "/home/user/.claude/projects/-path/abc123.jsonl",
  "started_at": "2024-01-15T10:30:00Z",
  "project_path": "/path/to/repo"
}
```

**Rationale**: Simple file-based state that survives process restarts. Git hooks can read this directly without needing to query Claude Code.

**Alternatives considered**:
- Environment variables: Don't persist across terminal sessions
- Querying sessions-index.json directly: Works but less precise about "active" vs "recent"
- PID-based tracking: Complex and unreliable

### Decision 2: Session Matching Strategy
When a manual commit triggers `claudit store --manual`:

1. **Active session**: If `.claudit/active-session.json` exists and the session is still active (check transcript file mtime), use it
2. **Recent session**: If no active session, check sessions-index.json for sessions matching current project with recent activity (within 5 minutes)
3. **No match**: If no relevant session found, skip storage silently (commit proceeds normally)

**Rationale**: Prioritizes precision over recall. Better to miss some captures than to associate wrong conversations.

### Decision 3: Stale Session Cleanup
The `SessionEnd` hook removes `.claudit/active-session.json`. If Claude crashes without firing `SessionEnd`, the file becomes stale.

Detection: Compare file timestamp against transcript file mtime. If transcript hasn't been modified in 10+ minutes, consider session inactive.

**Rationale**: Simple heuristic that handles common failure modes without complex process monitoring.

### Decision 4: Hook Installation
Add to existing `claudit init` flow:
- Install `SessionStart` hook in `.claude/settings.local.json`
- Install `SessionEnd` hook in `.claude/settings.local.json`
- Install `post-commit` git hook alongside existing sync hooks

**Rationale**: Keeps all hooks in sync, single initialization command.

### Decision 5: Idempotent Storage (Duplicate Prevention)
When Claude Code makes a commit, **both** the existing `PostToolUse` hook AND the new `post-commit` git hook will fire. Since `git notes add -f` silently overwrites, we need duplicate detection.

Before storing a note, `claudit store` SHALL:
1. Check if a note already exists for the commit (`git.HasNote()`)
2. If exists, read the existing note and compare `session_id`
3. If same session: skip silently (idempotent - already stored)
4. If different session: overwrite (newer conversation for same commit)
5. If no note exists: store normally

**Rationale**: Makes storage idempotent. Both hooks can fire safely - the first one stores, the second one skips. Order doesn't matter.

**Alternatives considered**:
- Track which commits were captured via PostToolUse: More complex state management
- Remove post-commit when PostToolUse succeeds: Requires coordination between hooks
- Different git notes refs: Complicates viewing and sync

## Risks / Trade-offs

### Risk: Wrong Session Association
If user switches between repos without ending session, wrong conversation could be captured.

**Mitigation**: Validate project path matches before storing. Skip if mismatch.

### Risk: Claude Code Hook Changes
Claude Code's hook format could change in future versions.

**Mitigation**: Version check in hooks, graceful degradation on parse errors.

### Risk: Multiple Terminals
User might have Claude running in multiple terminals for same repo.

**Mitigation**: Accept this limitation. Last-write-wins for active-session.json. Document behavior.

## Open Questions

1. Should there be a timeout for "recent session" matching? Current proposal: 5 minutes.
2. Should `claudit store --manual` warn when no session is found, or be completely silent? Current proposal: silent (to not interrupt git workflow).
