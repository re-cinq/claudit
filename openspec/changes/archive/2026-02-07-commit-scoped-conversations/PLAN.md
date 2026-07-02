# Plan: Commit-Scoped Conversation History

## Problem Statement

Currently, `claudit` stores the **entire** Claude session transcript when a commit is made. When viewing a conversation via `claudit show` or the web UI, users see all messages from the session start, not just the conversation that led to that specific commit.

**Goal**: Show only the conversation since the last commit, using Git as the sole source of truth (no external state files).

## Current Architecture

```
Session JSONL (full transcript)
    ↓
[claudit store hook on git commit]
    ↓
Git Note (compressed full transcript + metadata)
    ↓
[claudit show / web view]
    ↓
Full transcript rendered
```

### Key Data Structures

**StoredConversation** (in Git Note):
```go
type StoredConversation struct {
    Version      int    `json:"version"`
    SessionID    string `json:"session_id"`
    Timestamp    string `json:"timestamp"`
    ProjectPath  string `json:"project_path"`
    GitBranch    string `json:"git_branch"`
    MessageCount int    `json:"message_count"`
    Checksum     string `json:"checksum"`
    Transcript   string `json:"transcript"` // base64-encoded gzipped JSONL
}
```

**TranscriptEntry** (each JSONL line):
```go
type TranscriptEntry struct {
    UUID       string      `json:"uuid"`       // Unique identifier
    ParentUUID string      `json:"parentUuid"` // Parent message
    Type       MessageType `json:"type"`       // user|assistant|system
    Timestamp  string      `json:"timestamp"`  // RFC3339 timestamp
    ...
}
```

## Design Options

### Option A: Store Incremental (Delta Storage)
Store only new entries since parent commit.

- **Pros**: Smaller storage per commit
- **Cons**:
  - Complex reconstruction for full history
  - Harder to handle merges (multiple parents)
  - Resume functionality becomes complex
  - Can't verify checksum of full transcript

### Option B: Store Full, Display Incremental ✓ (Recommended)
Continue storing full transcripts, but calculate the diff at display time by comparing with parent commit.

- **Pros**:
  - Simpler storage model
  - Resume still works (full transcript available)
  - Git remains source of truth
  - No schema changes needed
  - Handles merges gracefully (intersection of parent conversations)
- **Cons**:
  - Slightly more computation at display time
  - Storage size unchanged (but compression mitigates this)

### Option C: Store Full + Mark Boundary
Add `parent_commit_sha` and `entries_since_parent` metadata to stored conversation.

- **Pros**: Pre-computed boundary makes display faster
- **Cons**:
  - Schema change required
  - Must update version
  - Boundary calculation still needed at store time

## Recommended Approach: Option B

Store full transcripts (no changes to storage), compute incremental display at query time.

## Implementation Plan

### Phase 1: Add Parent Commit Resolution

**File: `internal/git/repo.go`**

Add function to get parent commit(s):
```go
// GetParentCommits returns the parent commit SHA(s) for a given commit
func GetParentCommits(commitSHA string) ([]string, error) {
    cmd := exec.Command("git", "rev-parse", commitSHA+"^@")
    // Parse output, handle initial commit (no parents)
}
```

### Phase 2: Add Transcript Diff Logic

**File: `internal/claude/transcript.go`**

Add functions to compute transcript differences:
```go
// GetLastEntryUUID returns the UUID of the last entry in a transcript
func (t *Transcript) GetLastEntryUUID() string

// GetEntriesSince returns entries that come after the given UUID
// If uuid is empty, returns all entries (handles initial commit)
func (t *Transcript) GetEntriesSince(lastUUID string) []TranscriptEntry

// FindEntryIndex finds the index of an entry by UUID
func (t *Transcript) FindEntryIndex(uuid string) int
```

### Phase 3: Modify Show Command

**File: `cmd/show.go`**

Update `runShow` to:
1. Get parent commit(s) of the target commit
2. Find the most recent parent with a conversation note
3. Extract last entry UUID from parent's transcript
4. Filter current transcript to entries after that UUID
5. Render filtered transcript

```go
func runShow(cmd *cobra.Command, args []string) error {
    // ... existing ref resolution ...

    // Get parent commit conversation boundary
    parentCommits, _ := git.GetParentCommits(fullSHA)
    var lastEntryUUID string

    for _, parent := range parentCommits {
        if git.HasNote(parent) {
            parentNote, _ := git.GetNote(parent)
            parentStored, _ := storage.UnmarshalStoredConversation(parentNote)
            parentTranscript, _ := parentStored.GetTranscript()
            parsed, _ := claude.ParseTranscript(parentTranscript)
            lastEntryUUID = parsed.GetLastEntryUUID()
            break  // Use first parent with conversation
        }
    }

    // Filter transcript to entries since parent
    entries := transcript.GetEntriesSince(lastEntryUUID)

    // Render filtered entries
    renderer.RenderEntries(entries)
}
```

### Phase 4: Modify Web API

**File: `internal/web/handlers.go`**

Update `handleCommitDetail` to:
1. Accept optional `?incremental=true` query param
2. When incremental, apply same parent-based filtering
3. Return filtered transcript in response

```go
type ConversationResponse struct {
    SHA              string                   `json:"sha"`
    SessionID        string                   `json:"session_id"`
    Timestamp        string                   `json:"timestamp"`
    MessageCount     int                      `json:"message_count"`
    Transcript       []claude.TranscriptEntry `json:"transcript"`
    IsIncremental    bool                     `json:"is_incremental"`    // New
    ParentCommitSHA  string                   `json:"parent_commit_sha"` // New
}
```

### Phase 5: Add Full History Flag

For cases where users want the complete history:

```bash
claudit show --full          # Show full transcript (current behavior)
claudit show                 # Show incremental (new default)
claudit show --since abc123  # Show since specific commit
```

**File: `cmd/show.go`**

Add flags:
```go
showCmd.Flags().BoolVarP(&showFull, "full", "f", false, "Show full transcript")
showCmd.Flags().StringVar(&showSince, "since", "", "Show entries since specific commit")
```

## Edge Cases

### Initial Commit
- No parent commits exist
- `GetEntriesSince("")` returns all entries
- Behaves as full transcript (correct behavior)

### Merge Commits
- Multiple parent commits
- Strategy: Use first parent with conversation (follows git's first-parent convention)
- Alternative: Show union of new entries not in ANY parent

### Parent Without Conversation
- Walk back through history to find ancestor with conversation
- Or show full transcript if none found

### Different Sessions
- If session ID differs between commits, show full transcript
- User may have started a fresh session

## Testing Strategy

1. **Unit Tests** (`internal/claude/transcript_test.go`):
   - `TestGetLastEntryUUID` - verify UUID extraction
   - `TestGetEntriesSince` - verify filtering
   - `TestGetEntriesSinceEmpty` - initial commit case

2. **Integration Tests** (`tests/`):
   - Create repo with multiple commits
   - Store conversations at each
   - Verify `claudit show` shows only incremental
   - Verify `claudit show --full` shows everything
   - Test merge commits

3. **Edge Case Tests**:
   - Initial commit (no parent)
   - Parent without conversation note
   - Different session IDs
   - Empty transcript

## Migration

No migration needed - this is a display-time change. Existing stored conversations continue to work.

## Summary

| Component | Change |
|-----------|--------|
| `internal/git/repo.go` | Add `GetParentCommits()` |
| `internal/claude/transcript.go` | Add `GetLastEntryUUID()`, `GetEntriesSince()` |
| `cmd/show.go` | Filter by parent, add `--full` flag |
| `internal/web/handlers.go` | Add `?incremental=true` support |
| Storage format | No changes |

**Key principle**: Git graph provides the commit relationships; we compute "since last commit" by comparing transcripts at display time, keeping Git as the sole source of truth.
