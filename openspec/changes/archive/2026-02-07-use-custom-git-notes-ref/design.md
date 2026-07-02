# Design: Custom Git Notes Ref

## Decision

Use `refs/notes/claude-conversations` as a hardcoded constant for all notes operations.

**Rationale:**
- Custom ref keeps `git log` clean — notes only appear when explicitly requested
- Avoids collision with the default ref that other tools and manual notes share
- The ref name `claude-conversations` is self-documenting
- Push/fetch requires explicit ref regardless (custom or default), so no ergonomic difference there
- Users interact with claudit via its CLI (`claudit list`, `claudit resume`), not raw `git notes`

## Implementation

Single constant in the notes package:

```go
// internal/git/notes.go
const NotesRef = "refs/notes/claude-conversations"
```

All operations pass `--ref` to git:

```go
exec.Command("git", "notes", "--ref", NotesRef, "add", ...)
exec.Command("git", "notes", "--ref", NotesRef, "show", ...)
```

Push/fetch use the ref explicitly:

```go
exec.Command("git", "push", "origin", NotesRef)
exec.Command("git", "fetch", "origin", NotesRef+":"+NotesRef)
```

## Alternatives Considered

### Default ref with configurable override
Rejected: adds configuration infrastructure (config file, init prompt, flag) for no proven use case. The default ref pollutes `git log` and risks collisions.

### Configurable ref via environment variable
Rejected: same complexity without persistence. Users shouldn't need to think about this.
