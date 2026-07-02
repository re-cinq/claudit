# Web Visualization

Provides a web-based interface for browsing commits and viewing embedded conversation history.

## ADDED Requirements

### Requirement: Start web server
The serve command MUST start an HTTP server for the visualization interface.

#### Scenario: Start server on default port
- Given a git repository with stored conversations
- When the user runs `claudit serve`
- Then claudit starts an HTTP server on port 8080
- And displays the URL in the terminal

#### Scenario: Start server on custom port
- Given a git repository with stored conversations
- When the user runs `claudit serve --port 3000`
- Then claudit starts an HTTP server on port 3000

#### Scenario: Localhost binding by default
- Given the server is started without options
- When examining the server binding
- Then it binds to 127.0.0.1 (localhost only)

### Requirement: Serve embedded static assets
The web UI assets MUST be embedded in the binary for single-file distribution.

#### Scenario: Serve main page
- Given the server is running
- When the user accesses `/`
- Then the server returns the main HTML page

#### Scenario: Serve static assets
- Given the server is running
- When the user requests CSS, JS, or other assets
- Then the server returns the embedded files with correct content types

### Requirement: Commits API endpoint
The API MUST provide commit listing with conversation metadata.

#### Scenario: List all commits
- Given a repository with commits
- When the client requests `GET /api/commits`
- Then the server returns a JSON array of commits
- And each commit includes sha, message, author, date, and has_conversation flag

#### Scenario: Paginate commits
- Given a repository with many commits
- When the client requests `GET /api/commits?limit=20&offset=40`
- Then the server returns at most 20 commits starting from offset 40

#### Scenario: Filter commits with conversations
- Given a repository with some commits having conversations
- When the client requests `GET /api/commits?has_conversation=true`
- Then the server returns only commits that have stored conversations

### Requirement: Single commit API endpoint
The API MUST provide full conversation data for a specific commit.

#### Scenario: Get conversation for commit
- Given a commit with a stored conversation
- When the client requests `GET /api/commits/:sha`
- Then the server returns the full decompressed conversation
- And includes conversation metadata (session_id, timestamp, message_count)

#### Scenario: Commit without conversation
- Given a commit without a stored conversation
- When the client requests `GET /api/commits/:sha`
- Then the server returns 404 with message "no conversation found"

### Requirement: Git graph API endpoint
The API MUST provide data for rendering the commit graph.

#### Scenario: Get graph data
- Given a repository with branches and commits
- When the client requests `GET /api/graph`
- Then the server returns commit graph data
- And includes parent relationships for graph rendering
- And indicates which commits have conversations

### Requirement: Resume API endpoint
The API MUST provide an endpoint to trigger session resume.

#### Scenario: Resume session via API
- Given a commit with a stored conversation
- When the client sends `POST /api/resume/:sha`
- Then the server restores the session
- And checks out the commit
- And launches Claude Code
- And returns success status

#### Scenario: Resume with uncommitted changes
- Given uncommitted changes in the working directory
- When the client sends `POST /api/resume/:sha`
- Then the server returns 409 Conflict
- And includes message about uncommitted changes

### Requirement: Git graph visualization
The web UI MUST display the commit graph visually.

#### Scenario: Render commit graph
- Given the main page is loaded
- When viewing the left panel
- Then commits are displayed as a graph with branch lines
- And commits with conversations are visually highlighted

#### Scenario: Navigate graph
- Given the commit graph is displayed
- When the user scrolls or navigates
- Then more commits are loaded as needed

### Requirement: Conversation viewer
The web UI MUST display conversation content in a readable format.

#### Scenario: Display conversation on commit select
- Given the commit graph is displayed
- When the user clicks a commit with a conversation
- Then the right panel displays the conversation

#### Scenario: Format message content
- Given a conversation is displayed
- When viewing messages
- Then user messages are visually distinct from assistant messages
- And markdown content is rendered appropriately

#### Scenario: Collapsible tool uses
- Given a conversation with tool_use entries
- When viewing the conversation
- Then tool uses are displayed as collapsible sections
- And show tool name and can expand to show input/output

### Requirement: Resume button
The web UI MUST allow resuming sessions directly.

#### Scenario: Resume from UI
- Given a conversation is displayed
- When the user clicks the "Resume Session" button
- Then the UI calls the resume API
- And displays status feedback
