```go
package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// It first tries flat file storage (pre-v1.2), then falls back to SQLite (v1.2+).
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (pre-v1.2 OpenCode)
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite (OpenCode v1.2+)
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// discoverFromFlatFiles tries the legacy flat file session discovery.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return nil, nil
	}

	dirEntries, err := os.ReadDir(sessionDir)
	if err != nil {
		return nil, nil
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout
	var bestSessionID string
	var bestModTime time.Time

	for _, entry := range dirEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > recentTimeout {
			continue
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = strings.TrimSuffix(entry.Name(), ".json")
			bestModTime = modTime
		}
	}

	if bestSessionID == "" {
		return nil, nil
	}

	// The transcript path for OpenCode is the message directory
	msgDir, _ := GetMessageDir(bestSessionID)

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
//
// OpenCode's on-disk SQLite schema (table and column names) has changed across
// releases. Rather than hardcoding names that can drift out from under us, we
// introspect sqlite_master and PRAGMA table_info at runtime and match tables
// and columns by keyword. This keeps session discovery working across OpenCode
// versions without needing to track their internal storage schema exactly.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	tables, err := sqliteQueryRows(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil || len(tables) == 0 {
		return nil, nil
	}

	sessionTable := findTableName(tables, "session")
	messageTable := findTableName(tables, "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols, err := sqliteColumnNames(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	sessionIDCol := findColumnName(sessionCols, "id")
	if sessionIDCol == "" {
		return nil, nil
	}
	projectCol := findColumnName(sessionCols, "project", "workspace", "directory", "path", "cwd")
	updatedCol := findColumnName(sessionCols, "updated", "modified")
	orderCol := updatedCol
	if orderCol == "" {
		orderCol = findColumnName(sessionCols, "created", "time")
	}
	if orderCol == "" {
		orderCol = sessionIDCol
	}

	// Match on either the project ID (root commit hash) or the project's
	// filesystem path, since different OpenCode versions key sessions
	// differently.
	whereClause := ""
	if projectCol != "" {
		whereClause = fmt.Sprintf(
			"WHERE %s = '%s' OR %s LIKE '%%%s%%'",
			projectCol, escapeSQLiteLiteral(projectID),
			projectCol, escapeSQLiteLiteral(projectPath),
		)
	}

	sessionQuery := fmt.Sprintf(
		`SELECT %s FROM %s %s ORDER BY %s DESC LIMIT 1;`,
		sessionIDCol, sessionTable, whereClause, orderCol,
	)
	sessionID, err := sqliteQuery(dbPath, sessionQuery)
	if err != nil || sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout)
	if updatedCol != "" {
		timeQuery := fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s = '%s';`,
			updatedCol, sessionTable, sessionIDCol, escapeSQLiteLiteral(sessionID),
		)
		if timeOutput, err := sqliteQuery(dbPath, timeQuery); err == nil && timeOutput != "" {
			if !withinRecentWindow(timeOutput) {
				return nil, nil
			}
		}
		// If we can't read/parse the time, proceed anyway — better to try than skip.
	}

	messageCols, err := sqliteColumnNames(dbPath, messageTable)
	if err != nil || len(messageCols) == 0 {
		return nil, nil
	}

	msgIDCol := findColumnName(messageCols, "id")
	msgSessionCol := findColumnName(messageCols, "session")
	if msgIDCol == "" || msgSessionCol == "" {
		return nil, nil
	}
	msgOrderCol := findColumnName(messageCols, "created", "time")
	if msgOrderCol == "" {
		msgOrderCol = msgIDCol
	}

	// Get messages for this session as a JSON array. If the message table
	// stores each row as a single JSON blob column, patch the row's id into
	// it; otherwise fall back to packing every column into a JSON object.
	var msgQuery string
	if dataCol := findColumnName(messageCols, "data", "payload", "content", "body", "parts"); dataCol != "" {
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s = '%s' ORDER BY %s;`,
			dataCol, msgIDCol, messageTable, msgSessionCol, escapeSQLiteLiteral(sessionID), msgOrderCol,
		)
	} else {
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(json_object(%s)) FROM %s WHERE %s = '%s' ORDER BY %s;`,
			sqliteJSONObjectArgs(messageCols), messageTable, msgSessionCol, escapeSQLiteLiteral(sessionID), msgOrderCol,
		)
	}

	msgOutput, err := sqliteQuery(dbPath, msgQuery)
	if err != nil {
		return nil, nil
	}

	// sqlite3 returns "[null]" when no rows match
	if msgOutput == "" || msgOutput == "[null]" || msgOutput == "[]" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: []byte(msgOutput),
	}, nil
}

// sqliteQuery runs a single SQLite statement against dbPath and returns its
// trimmed output.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sqliteQueryRows runs a query and returns its output split into non-empty lines.
func sqliteQueryRows(dbPath, query string) ([]string, error) {
	out, err := sqliteQuery(dbPath, query)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var rows []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			rows = append(rows, line)
		}
	}
	return rows, nil
}

// sqliteColumnNames returns the column names of a table via PRAGMA table_info.
func sqliteColumnNames(dbPath, table string) ([]string, error) {
	rows, err := sqliteQueryRows(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, row := range rows {
		fields := strings.Split(row, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// findTableName returns the first table name containing substr (case-insensitive).
func findTableName(tables []string, substr string) string {
	for _, t := range tables {
		if strings.Contains(strings.ToLower(t), substr) {
			return t
		}
	}
	return ""
}

// findColumnName returns the first column matching any of substrs. Each
// substring is tried against every column before moving on to the next
// substring, so more specific keywords (e.g. "updated") are preferred over
// generic fallbacks (e.g. "created").
func findColumnName(cols []string, substrs ...string) string {
	for _, want := range substrs {
		for _, c := range cols {
			if strings.Contains(strings.ToLower(c), want) {
				return c
			}
		}
	}
	return ""
}

// sqliteJSONObjectArgs builds the argument list for a SQLite json_object()
// call that includes every given column under its own name.
func sqliteJSONObjectArgs(cols []string) string {
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, fmt.Sprintf("'%s', %s", c, c))
	}
	return strings.Join(parts, ", ")
}

// escapeSQLiteLiteral escapes single quotes for safe inclusion in a SQL string literal.
func escapeSQLiteLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// withinRecentWindow reports whether raw (a timestamp in an unknown format —
// RFC3339, a SQL datetime string, or a Unix epoch in seconds/milliseconds)
// falls within agent.RecentSessionTimeout of now. Unparseable values are
// treated as recent so a session isn't skipped just because we can't read its
// timestamp format.
func withinRecentWindow(raw string) bool {
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return time.Since(t) <= agent.RecentSessionTimeout
	}
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", raw); err == nil {
		return time.Since(t) <= agent.RecentSessionTimeout
	}
	if t, err := time.Parse("2006-01-02 15:04:05", raw); err == nil {
		return time.Since(t) <= agent.RecentSessionTimeout
	}
	if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
		t := time.Unix(v, 0)
		if v > 1_000_000_000_000 {
			t = time.UnixMilli(v)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}
	return true
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
