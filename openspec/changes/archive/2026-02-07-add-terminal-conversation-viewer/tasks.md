# Tasks: Add Terminal Conversation Viewer

## 1. Implementation

- [x] **1.1** Create terminal renderer for conversations
  - Create `internal/claude/render.go`
  - Format user messages with "User:" prefix
  - Format assistant messages with "Assistant:" prefix
  - Format tool use with tool name and summary
  - Format system messages with "System:" prefix
  - Support color output (respect NO_COLOR env)
  - Verify: Unit tests pass

- [x] **1.2** Implement show command
  - Create `cmd/show.go`
  - Accept optional commit ref argument (default to HEAD)
  - Resolve commit reference
  - Read and decompress conversation from note
  - Render conversation to stdout
  - Handle missing conversation gracefully
  - Verify: Command runs

- [x] **1.3** Add acceptance tests for show command
  - Test: `claudit show <sha>` displays conversation
  - Test: `claudit show` (no args) shows HEAD conversation
  - Test: `claudit show <sha>` with no conversation shows error
  - Test: `claudit show invalid-ref` shows error
  - Test: Output contains User/Assistant prefixes
  - Verify: `ginkgo tests/acceptance` passes

## 2. Enhanced Tool Display

- [x] **2.1** Show tool call inputs
  - Display tool inputs (command for Bash, file_path/content for Write, etc.)
  - Handle multi-line inputs with indentation and truncation
  - Verify: Unit tests pass

- [x] **2.2** Show tool result content
  - Display actual tool result content instead of placeholder
  - Handle both string and array content formats
  - Verify: Unit tests pass

- [x] **2.3** Add unit tests for tool display
  - Test: Tool use renders with input parameters
  - Test: Tool result renders with content
  - Test: Multi-line content is properly indented
  - Verify: `go test ./internal/claude/...` passes

## 3. Web View Consistency

- [x] **3.1** Apply same rendering rules to web view
  - Show tool inputs with formatted summary
  - Display tool results with actual content
  - Add thinking block rendering (collapsible)
  - Filter progress/file-history-snapshot entries
  - Handle string content for first user message
  - Add system message rendering
  - Verify: Web UI shows same information as terminal

## 4. Version Bump

- [x] **4.1** Bump minor version
  - Update version constant in code
  - Verify: `claudit --version` shows new version
