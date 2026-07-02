# Bootstrap Claudit CLI

## Summary
Initialize the Claudit CLI project with core capabilities for storing Claude Code conversation history as Git Notes, resuming sessions from commits, and visualizing conversations in a web interface.

## Motivation
Currently, Claude Code conversation history is ephemeral and disconnected from the codebase. Developers lose valuable context about why changes were made. By storing conversations as Git Notes attached to commits, teams can:
- Preserve AI-assisted development context alongside code
- Resume interrupted sessions from any historical commit
- Review the reasoning behind past changes
- Share conversation context across team members

## Scope
This proposal bootstraps the entire Claudit CLI with four core capabilities:

1. **CLI Foundation** - Cobra-based CLI structure and common utilities
2. **Conversation Storage** - Store conversations as Git Notes on commit
3. **Session Resume** - Restore and resume sessions from historical commits
4. **Web Visualization** - Browse commits with embedded conversation viewer

## Non-Goals
- Cloud synchronization (out of scope for v1)
- Multi-repository aggregation
- Conversation search/indexing
- Integration with other AI tools

## Risks
- **Claude Code format changes**: The JSONL format is undocumented and may change
- **Git notes adoption**: Teams unfamiliar with git notes may find syncing confusing
- **Hook timeout**: 30-second hook timeout may be insufficient for large transcripts

## Related Documents
- [PLAN.md](/PLAN.md) - Original design document
