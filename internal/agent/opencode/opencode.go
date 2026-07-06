```go
package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
)

func init() {
	agent.Register(&Agent{})
}

// Agent implements the agent.Agent interface for OpenCode CLI.
type Agent struct{}

func (a *Agent) Name() agent.Name   { return agent.OpenCode }
func (a *Agent) DisplayName() string { return "OpenCode CLI" }

// ConfigureHooks installs the shiftlog plugin for OpenCode.
func (a *Agent) ConfigureHooks(repoRoot string) error {
	return InstallPlugin(repoRoot)
}

// RemoveHooks removes the shiftlog plugin for OpenCode.
func (a *Agent) RemoveHooks(repoRoot string) error {
	return RemovePlugin(repoRoot)
}

// DiagnoseHooks validates OpenCode plugin installation.
func (a *Agent) DiagnoseHooks(repoRoot string) []agent.DiagnosticCheck {
	var checks []agent.DiagnosticCheck

	if HasPlugin(repoRoot) {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "OpenCode plugin",
			OK:      true,
			Message: "Found .opencode/plugins/shiftlog.js",
		})
	} else {
		checks = append(checks, agent.DiagnosticCheck{
			Name:    "OpenCode plugin",
			OK:      false,
			Message: "Missing .opencode/plugins/shiftlog.js. Run 'shiftlog init --agent=opencode' to install.",
		})
	}

	return checks
}

// ParseHookInput parses OpenCode's plugin hook JSON.
func (a *Agent) ParseHookInput(raw []byte) (*agent.HookData, error) {
	var hook struct {
		SessionID      string `json:"session_id"`
		DataDir        string `json:"data_dir"`
		ProjectDir     string `json:"project_dir"`
		ToolName       string `json:"tool_name"`
		TranscriptData string `json:"transcript_data"`
		ToolInput      struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(raw, &hook); err != nil {
		return nil, err
	}

	// For OpenCode, we don't have a single transcript path.
	// Instead, we reconstruct from the data directory and session ID.
	transcriptPath := ""
	if hook.DataDir != "" && hook.SessionID != "" {
		transcriptPath = filepath.Join(hook.DataDir, "storage", "message", hook.SessionID)
	}

	// Use inline transcript data from the plugin SDK client if available
	var transcriptData []byte
	if hook.TranscriptData != "" {
		transcriptData = []byte(hook.TranscriptData)
	}

	return &agent.HookData{
		SessionID:      hook.SessionID,
		TranscriptPath: transcriptPath,
		ToolName:       hook.ToolName,
		Command:        hook.ToolInput.Command,
		TranscriptData: transcriptData,
	}, nil
}

// IsCommitCommand checks if a tool invocation represents a git commit.
func (a *Agent) IsCommitCommand(toolName, command string) bool {
	// OpenCode tool names for shell execution
	shellTools := map[string]bool{
		"bash":               true,
		"shell":              true,
		"terminal":           true,
		"execute":            true,
		"run":                true,
		"command":            true,
	}

	if !shellTools[toolName] {
		return false
	}
	return agent.IsGitCommitCommand(command)
}

// ParseTranscript parses an OpenCode transcript.
// OpenCode stores messages as individual JSON files, but we also handle
// the case where a single combined file is provided (e.g., during restore).
func (a *Agent) ParseTranscript(r io.Reader) (*agent.Transcript, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	var entries []agent.TranscriptEntry

	// If data starts with '[', try JSON array first
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		var messages []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &messages); err == nil {
			for _, msgData := range messages {
				var raw map[string]json.RawMessage
				if err := json.Unmarshal(msgData, &raw); err == nil {
					entry := parseOpenCodeEntry(raw, msgData)
					if entry.Type != "" {
						entries = append(entries, entry)
					}
				}
			}
			t := &agent.Transcript{Entries: entries}
			t.Turns = t.CountTurns()
			return t, nil
		}
	}

	// Try as JSONL (for restored transcripts and compatibility)
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}

		entry := parseOpenCodeEntry(raw, []byte(line))
		if entry.Type != "" {
			entries = append(entries, entry)
		}
	}

	t := &agent.Transcript{Entries: entries}
	t.Turns = t.CountTurns()
	return t, nil
}

// ParseTranscriptFile parses an OpenCode session from the message directory.
func (a *Agent) ParseTranscriptFile(path string) (*agent.Transcript, error) {
	// Check if path is a directory (message directory)
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if info.IsDir() {
		return a.parseMessageDir(path)
	}

	// Otherwise, treat as a single file
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return a.ParseTranscript(f)
}

// parseMessageDir reads all message files from an OpenCode message directory.
func (a *Agent) parseMessageDir(dir string) (*agent.Transcript, error) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var entries []agent.TranscriptEntry
	for _, de := range dirEntries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			// Handle .jsonl files too
			if strings.HasSuffix(de.Name(), ".jsonl") {
				f, err := os.Open(filepath.Join(dir, de.Name()))
				if err != nil {
					continue
				}
				transcript, err := a.ParseTranscript(f)
				_ = f.Close()
				if err == nil {
					entries = append(entries, transcript.Entries...)
				}
			}
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}

		entry := parseOpenCodeEntry(raw, data)
		if entry.Type != "" {
			entries = append(entries, entry)
		}
	}

	return &agent.Transcript{Entries: entries}, nil
}

// DiscoverSession finds an active or recent OpenCode session.
// It first tries flat file storage, then falls back to SQLite. OpenCode has
// changed its on-disk storage layout and SQLite schema across versions, so
// both discovery paths search broadly / introspect the schema at runtime
// rather than assuming one fixed shape.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first.
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite.
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// discoverFromFlatFiles searches OpenCode's flat-file session storage for the
// most recent session belonging to this project. OpenCode has used different
// directory layouts across versions (a per-project subdirectory under
// storage/session/, or a shared storage/session/info/ directory with the
// project recorded inside the JSON instead), so rather than assuming a fixed
// layout we search the storage tree broadly and match on file content.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	searchRoot := filepath.Join(dataDir, "storage", "session")
	if _, err := os.Stat(searchRoot); err != nil {
		searchRoot = filepath.Join(dataDir, "storage")
		if _, err := os.Stat(searchRoot); err != nil {
			return nil, nil
		}
	}

	projectID := GetProjectID(projectPath)
	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout

	var bestID string
	var bestModTime time.Time
	var bestMatchesProject bool
	found := false

	for _, path := range findSessionFiles(searchRoot) {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > recentTimeout {
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}

		id := firstNonEmpty(
			jsonStringField(raw, "id"),
			jsonStringField(raw, "sessionID"),
			jsonStringField(raw, "sessionId"),
		)
		if id == "" {
			// Older layout: the session ID is only encoded in the filename.
			id = strings.TrimSuffix(filepath.Base(path), ".json")
		}

		matchesProject := false
		for _, key := range []string{"projectID", "project_id", "directory", "cwd", "worktree"} {
			if v := jsonStringField(raw, key); v != "" && (v == projectID || v == projectPath) {
				matchesProject = true
				break
			}
		}

		if !found ||
			(matchesProject && !bestMatchesProject) ||
			(matchesProject == bestMatchesProject && modTime.After(bestModTime)) {
			bestID = id
			bestModTime = modTime
			bestMatchesProject = matchesProject
			found = true
		}
	}

	if !found {
		return nil, nil
	}

	session := &agent.SessionInfo{
		SessionID:   bestID,
		StartedAt:   bestModTime.Format(time.RFC3339),
		ProjectPath: projectPath,
	}

	if msgDir := findMessageDir(dataDir, bestID); msgDir != "" {
		session.TranscriptPath = msgDir
	} else {
		session.TranscriptData = []byte("[]")
	}

	return session, nil
}

// discoverFromSQLite looks for OpenCode's SQLite session database, trying a
// project-local path first (some OpenCode versions store it under
// <project>/.opencode/opencode.db) and then the shared XDG data directory.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	candidates := []string{
		filepath.Join(projectPath, ".opencode", "opencode.db"),
		filepath.Join(dataDir, "opencode.db"),
	}

	for _, dbPath := range candidates {
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		if session := discoverSessionFromDB(dbPath, projectID, projectPath); session != nil {
			session.ProjectPath = projectPath
			return session, nil
		}
	}

	return nil, nil
}

// discoverSessionFromDB queries a single OpenCode SQLite database for the
// most recent session belonging to projectID or projectPath, falling back to
// the most recent session overall if no project match is found. Table and
// column names are discovered at runtime (rather than hardcoded) because
// OpenCode's SQLite schema has changed across versions.
func discoverSessionFromDB(dbPath, projectID, projectPath string) *agent.SessionInfo {
	tables := sqliteTableNames(dbPath)
	sessionTable := findTableContaining(tables, "session")
	if sessionTable == "" {
		return nil
	}

	sessionCols := sqliteColumnNames(dbPath, sessionTable)
	idCol := findColumnMatching(sessionCols, "id")
	if idCol == "" {
		return nil
	}
	projectCol := findColumnMatching(sessionCols, "project_id", "projectid", "directory", "cwd", "worktree", "project")
	orderCol := findColumnMatching(sessionCols, "time_updated", "updated_at", "updatedat", "time_created", "created_at", "createdat")
	if orderCol == "" {
		orderCol = "rowid"
	}

	var sessionID string
	if projectCol != "" {
		for _, candidate := range []string{projectID, projectPath} {
			if candidate == "" {
				continue
			}
			query := fmt.Sprintf(
				"SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;",
				idCol, sessionTable, projectCol, sqlEscape(candidate), orderCol,
			)
			rows := sqliteQueryJSON(dbPath, query)
			if len(rows) > 0 {
				if id, ok := rows[0][idCol].(string); ok && id != "" {
					sessionID = id
					break
				}
			}
		}
	}

	if sessionID == "" {
		// No project match (or no project column at all): fall back to the
		// single most recent session in the database.
		query := fmt.Sprintf("SELECT %s FROM %s ORDER BY %s DESC LIMIT 1;", idCol, sessionTable, orderCol)
		rows := sqliteQueryJSON(dbPath, query)
		if len(rows) == 0 {
			return nil
		}
		id, _ := rows[0][idCol].(string)
		if id == "" {
			return nil
		}
		sessionID = id
	}

	transcriptData := collectMessagesJSON(dbPath, tables, sessionID)
	if len(transcriptData) == 0 {
		transcriptData = []byte("[]")
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		StartedAt:      time.Now().Format(time.RFC3339),
		TranscriptData: transcriptData,
	}
}

// collectMessagesJSON fetches every row belonging to sessionID from whichever
// table looks like a message table, returned as a JSON array (using the
// table's real column names) suitable for use as inline transcript data.
func collectMessagesJSON(dbPath string, tables []string, sessionID string) []byte {
	msgTable := findTableContaining(tables, "message")
	if msgTable == "" {
		return nil
	}

	msgCols := sqliteColumnNames(dbPath, msgTable)
	fkCol := findColumnMatching(msgCols, "session_id", "sessionid", "session")
	if fkCol == "" {
		return nil
	}

	query := fmt.Sprintf("SELECT * FROM %s WHERE %s='%s';", msgTable, fkCol, sqlEscape(sessionID))
	rows := sqliteQueryJSON(dbPath, query)
	if len(rows) == 0 {
		return nil
	}

	data, err := json.Marshal(rows)
	if err != nil {
		return nil
	}
	return data
}

// sqliteQueryJSON runs a query against dbPath via the sqlite3 CLI and parses
// the result as JSON rows (using sqlite3's own -json flag so we never need to
// hardcode column ordering or separators).
func sqliteQueryJSON(dbPath, query string) []map[string]interface{} {
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal(output, &rows); err != nil {
		return nil
	}
	return rows
}

// sqliteTableNames returns every table name defined in the database.
func sqliteTableNames(dbPath string) []string {
	rows := sqliteQueryJSON(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		if n, ok := r["name"].(string); ok {
			names = append(names, n)
		}
	}
	return names
}

// sqliteColumnNames returns every column name defined on table.
func sqliteColumnNames(dbPath, table string) []string {
	rows := sqliteQueryJSON(dbPath, fmt.Sprintf("PRAGMA table_info(%q);", table))
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		if n, ok := r["name"].(string); ok {
			names = append(names, n)
		}
	}
	return names
}

// findTableContaining returns the first table whose name contains substr
// (case-insensitive), or "" if none match.
func findTableContaining(tables []string, substr string) string {
	for _, t := range tables {
		if strings.Contains(strings.ToLower(t), substr) {
			return t
		}
	}
	return ""
}

// findColumnMatching returns the first column that exactly matches one of the
// candidates (case-insensitive), falling back to a substring match.
func findColumnMatching(columns []string, candidates ...string) string {
	for _, candidate := range candidates {
		for _, c := range columns {
			if strings.EqualFold(c, candidate) {
				return c
			}
		}
	}
	for _, candidate := range candidates {
		for _, c := range columns {
			if strings.Contains(strings.ToLower(c), candidate) {
				return c
			}
		}
	}
	return ""
}

// sqlEscape escapes single quotes for use inside a SQLite string literal.
func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// RestoreSession writes a session to OpenCode's storage location.
func (a *Agent) RestoreSession(projectPath, sessionID, gitBranch string,
	transcriptData []byte, messageCount int, summary string) error {

	_, err := WriteSessionFile(projectPath, sessionID, transcriptData)
	return err
}

// ResumeCommand returns the command to resume an OpenCode session.
func (a *Agent) ResumeCommand(sessionID string) (string, []string) {
	return "opencode", []string{"--session", sessionID}
}

// ToolAliases returns OpenCode's tool name mappings to canonical names.
func (a *Agent) ToolAliases() map[string]string {
	return map[string]string{
		"bash":     "Bash",
		"shell":    "Bash",
		"terminal": "Bash",
		"write":    "Write",
		"read":     "Read",
		"edit":     "Edit",
		"grep":     "Grep",
		"glob":     "Glob",
	}
}

// parseOpenCodeEntry parses a single OpenCode message into a TranscriptEntry.
func parseOpenCodeEntry(raw map[string]json.RawMessage, fullData []byte) agent.TranscriptEntry {
	entry := agent.TranscriptEntry{
		Raw: json.RawMessage(append([]byte{}, fullData...)),
	}

	// Parse role field
	if roleRaw, ok := raw["role"]; ok {
		var role string
		if err := json.Unmarshal(roleRaw, &role); err == nil {
			entry.Type = agent.NormalizeRole(role)
		}
	}

	// Try "type" field if role not found
	if entry.Type == "" {
		if typeRaw, ok := raw["type"]; ok {
			var t string
			if err := json.Unmarshal(typeRaw, &t); err == nil {
				entry.Type = agent.NormalizeRole(t)
			}
		}
	}

	// Parse id
	if idRaw, ok := raw["id"]; ok {
		var id string
		if err := json.Unmarshal(idRaw, &id); err == nil {
			entry.UUID = id
		}
	}

	// Parse timestamp
	if timeRaw, ok := raw["time"]; ok {
		var timeObj struct {
			Created string `json:"created"`
		}
		if err := json.Unmarshal(timeRaw, &timeObj); err == nil {
			entry.Timestamp = timeObj.Created
		}
	}

	// Parse content
	entry.Message = parseOpenCodeMessage(raw, entry.Type)

	return entry
}

// parseOpenCodeMessage parses message content from an OpenCode entry.
func parseOpenCodeMessage(raw map[string]json.RawMessage, msgType agent.MessageType) *agent.Message {
	if msgType == "" {
		return nil
	}

	msg := &agent.Message{}
	switch msgType {
	case agent.MessageTypeUser:
		msg.Role = "user"
	case agent.MessageTypeAssistant:
		msg.Role = "assistant"
	case agent.MessageTypeSystem:
		msg.Role = "system"
	}

	// Try "content" as string
	if contentRaw, ok := raw["content"]; ok {
		var text string
		if err := json.Unmarshal(contentRaw, &text); err == nil && text != "" {
			msg.Content = []agent.ContentBlock{{Type: "text", Text: text}}
			return msg
		}

		// Try as array of content blocks
		var blocks []agent.ContentBlock
		if err := json.Unmarshal(contentRaw, &blocks); err == nil && len(blocks) > 0 {
			msg.Content = blocks
			return msg
		}
	}

	// Try "message" field
	if msgRaw, ok := raw["message"]; ok {
		var innerMsg agent.Message
		if err := json.Unmarshal(msgRaw, &innerMsg); err == nil {
			return &innerMsg
		}
	}

	return msg
}
```
