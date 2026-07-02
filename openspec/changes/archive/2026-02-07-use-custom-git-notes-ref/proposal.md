# Proposal: Keep Custom Git Notes Ref

## Problem

Claudit currently stores conversation notes on the default git notes ref (`refs/notes/commits`). This pollutes `git log` output with large compressed JSON blobs, and risks collision with other tools or manual notes (git allows only one note per commit per ref).

## Proposed Solution

Use the custom ref `refs/notes/claude-conversations` for all notes operations. No configuration — just hardcode the custom ref.

This was the original design. It was changed to use the default ref for ergonomics, but the downsides outweigh the convenience:
- **Log pollution**: `git log` shows large compressed blobs on every annotated commit
- **Collision risk**: Other tools or manual `git notes add` will overwrite claudit data (or vice versa)
- **No real ergonomics win**: Users interact with notes via `claudit list`/`claudit resume`, not raw `git notes` commands

## Benefits

- Clean `git log` output by default
- No collision with other note-using tools or manual notes
- Simpler implementation — no config system needed
- Users who want to see notes in the log can opt in: `git log --notes=claude-conversations`

## Non-Goals

- Configurable ref selection (unnecessary complexity for no proven use case)
- Migration tooling (no production users on default ref yet)

## What Changes

- Hardcode `refs/notes/claude-conversations` as the notes ref
- All git notes operations (add, show, list, push, fetch) use this ref
- No `.claudit/config` file or ref selection prompt needed
