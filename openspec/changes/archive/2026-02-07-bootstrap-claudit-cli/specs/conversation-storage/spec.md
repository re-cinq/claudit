# Conversation Storage

Captures Claude Code conversation transcripts and stores them as Git Notes attached to commits.

## ADDED Requirements

### Requirement: Hook handler for commit detection
The `claudit store` command MUST process PostToolUse hook events and detect git commits.

#### Scenario: Detect git commit command
- Given Claude Code executed a Bash command
- When the hook JSON indicates `tool_input.command` contains `git commit`
- Then claudit proceeds to store the conversation

#### Scenario: Ignore non-commit commands
- Given Claude Code executed a Bash command
- When the hook JSON indicates a command that is not `git commit`
- Then claudit exits silently with status 0

#### Scenario: Handle malformed hook input
- Given claudit receives invalid JSON on stdin
- When processing the hook
- Then claudit logs a warning and exits with status 0
- And does not disrupt the user's workflow

### Requirement: Read Claude Code transcript
The store command MUST read the JSONL transcript from the path provided by the hook.

#### Scenario: Read transcript from hook-provided path
- Given the hook JSON contains `transcript_path`
- When claudit processes the hook
- Then claudit reads the JSONL file at that path

#### Scenario: Handle missing transcript file
- Given the hook JSON contains a `transcript_path` that doesn't exist
- When claudit processes the hook
- Then claudit logs a warning and exits with status 0

### Requirement: Parse JSONL transcript format
The storage module MUST correctly parse Claude Code's JSONL transcript format.

#### Scenario: Parse user message entry
- Given a JSONL line with `"type": "user"`
- When parsing the transcript
- Then claudit extracts uuid, parentUuid, timestamp, and message content

#### Scenario: Parse assistant message entry
- Given a JSONL line with `"type": "assistant"`
- When parsing the transcript
- Then claudit extracts the message content blocks (text, thinking, tool_use)

#### Scenario: Parse tool result entry
- Given a JSONL line with `"type": "user"` and `tool_result` content
- When parsing the transcript
- Then claudit links it to the source tool_use via `sourceToolAssistantUUID`

#### Scenario: Handle unknown entry types gracefully
- Given a JSONL line with an unrecognized type
- When parsing the transcript
- Then claudit preserves the raw JSON for future compatibility

### Requirement: Compress and encode transcript
Transcripts MUST be compressed and encoded for efficient storage in git notes.

#### Scenario: Compress with gzip
- Given a JSONL transcript
- When storing the conversation
- Then the transcript is compressed using gzip

#### Scenario: Encode as base64
- Given compressed transcript data
- When storing the conversation
- Then the data is encoded as base64 for safe text embedding

### Requirement: Store as Git Note
The compressed transcript MUST be stored as a Git Note attached to the commit.

#### Scenario: Create git note on commit
- Given a compressed, encoded transcript
- When storing the conversation
- Then claudit attaches the note to HEAD using `refs/notes/claude-conversations`

#### Scenario: Storage format structure
- Given a stored conversation
- When examining the git note content
- Then it contains version, session_id, timestamp, project_path, git_branch, message_count, checksum, and transcript fields

#### Scenario: Overwrite existing note
- Given a commit already has a claude-conversations note
- When storing a new conversation for that commit
- Then the existing note is replaced (using `git notes add -f`)

### Requirement: Compute integrity checksum
A checksum MUST be stored to verify transcript integrity on retrieval.

#### Scenario: SHA256 checksum of original transcript
- Given a JSONL transcript
- When storing the conversation
- Then the checksum is computed from the original (uncompressed) transcript
- And stored in the format `sha256:<hex>`
