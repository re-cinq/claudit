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

// sessionTableCandidates and messageTableCandidates list the table names
// tried when locating OpenCode's session/message tables. OpenCode's internal
// SQLite schema is undocumented and has been renamed across releases.
var sessionTableCandidates = []string{"session", "sessions"}
var messageTableCandidates = []string{"message", "messages"}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session belonging to this project.
//
// OpenCode's internal schema (table/column names) is not a stable public API
// and has changed across releases, so this does not hardcode column names.
// It introspects the actual schema via PRAGMA table_info, then:
//  1. Prefers an exact match on a "project"-like column against projectID.
//  2. Falls back to matching any column whose value contains projectPath,
//     since the working directory is generally recorded on the session row
//     regardless of how the "project" concept is currently modeled.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	for _, table := range sessionTableCandidates {
		info, err := sessionFromSQLiteTable(dbPath, table, projectID, projectPath)
		if err != nil {
			continue
		}
		if info != nil {
			return info, nil
		}
	}

	return nil, nil
}

// sessionFromSQLiteTable locates the most recent matching session in the
// given table and, if found, fetches its messages.
func sessionFromSQLiteTable(dbPath, table, projectID, projectPath string) (*agent.SessionInfo, error) {
	sessionCols, err := sqliteTableColumns(dbPath, table)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := findColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := findColumn(sessionCols, "project")
	timeCol := findColumn(sessionCols, "updated", "created", "time")

	rows, err := runSQLiteJSONQuery(dbPath, fmt.Sprintf("SELECT * FROM %s ORDER BY rowid DESC LIMIT 50;", table))
	if err != nil || len(rows) == 0 {
		return nil, nil
	}

	var matched map[string]interface{}
	if projectCol != "" {
		for _, row := range rows {
			if v, ok := row[projectCol].(string); ok && v == projectID {
				matched = row
				break
			}
		}
	}
	if matched == nil {
		for _, row := range rows {
			if rowContainsPath(row, projectPath) {
				matched = row
				break
			}
		}
	}
	if matched == nil {
		return nil, nil
	}

	if timeCol != "" {
		if t, ok := parseSQLiteTime(matched[timeCol]); ok && time.Since(t) > agent.RecentSessionTimeout {
			return nil, nil
		}
	}

	sessionID := fmt.Sprintf("%v", matched[idCol])
	if sessionID == "" || sessionID == "<nil>" {
		return nil, nil
	}

	transcriptData, err := fetchSQLiteMessages(dbPath, sessionID)
	if err != nil || len(transcriptData) == 0 {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// fetchSQLiteMessages retrieves all messages for a session as a JSON array,
// trying each candidate message table until one yields results.
func fetchSQLiteMessages(dbPath, sessionID string) ([]byte, error) {
	for _, table := range messageTableCandidates {
		data, err := messagesFromSQLiteTable(dbPath, table, sessionID)
		if err == nil && len(data) > 0 {
			return data, nil
		}
	}
	return nil, nil
}

// messagesFromSQLiteTable fetches and normalizes messages from a single
// candidate table. It is tolerant of schema changes: it introspects the
// table's columns rather than assuming fixed names, and merges any JSON blob
// column (e.g. a "data" column holding the full message body) with the row's
// other columns so downstream parsing sees a single flat object either way.
func messagesFromSQLiteTable(dbPath, table, sessionID string) ([]byte, error) {
	msgCols, err := sqliteTableColumns(dbPath, table)
	if err != nil || len(msgCols) == 0 {
		return nil, err
	}

	sessionIDCol := findColumn(msgCols, "session")
	if sessionIDCol == "" {
		return nil, fmt.Errorf("opencode: no session id column found in %s table", table)
	}
	idCol := findColumn(msgCols, "id")
	dataCol := findColumn(msgCols, "data")

	query := fmt.Sprintf(
		"SELECT * FROM %s WHERE %s='%s' ORDER BY rowid;",
		table, sessionIDCol, sqlQuote(sessionID),
	)
	rows, err := runSQLiteJSONQuery(dbPath, query)
	if err != nil || len(rows) == 0 {
		return nil, err
	}

	messages := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		merged := map[string]interface{}{}
		if dataCol != "" {
			if raw, ok := row[dataCol].(string); ok && raw != "" {
				var inner map[string]interface{}
				if err := json.Unmarshal([]byte(raw), &inner); err == nil {
					for k, v := range inner {
						merged[k] = v
					}
				}
			}
		}
		for k, v := range row {
			if k == dataCol {
				continue
			}
			merged[k] = v
		}
		if idCol != "" {
			merged["id"] = row[idCol]
		}
		messages = append(messages, merged)
	}

	return json.Marshal(messages)
}

// runSQLiteJSONQuery executes a query and returns rows as generic maps keyed
// by their actual column names, so callers never need to hardcode a schema.
func runSQLiteJSONQuery(dbPath, query string) ([]map[string]interface{}, error) {
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, nil
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// sqliteTableColumns returns a table's column names in schema order, or an
// empty slice if the table does not exist.
func sqliteTableColumns(dbPath, table string) ([]string, error) {
	rows, err := runSQLiteJSONQuery(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return nil, err
	}
	cols := make([]string, 0, len(rows))
	for _, row := range rows {
		if name, ok := row["name"].(string); ok {
			cols = append(cols, name)
		}
	}
	return cols, nil
}

// findColumn returns the first column matching any of the given
// case-insensitive substrings, trying substrings in priority order.
func findColumn(columns []string, substrings ...string) string {
	for _, sub := range substrings {
		for _, c := range columns {
			if strings.Contains(strings.ToLower(c), sub) {
				return c
			}
		}
	}
	return ""
}

// rowContainsPath reports whether any string value in the row matches or
// contains projectPath. Used to identify a session's project when the
// database's own project-identifier scheme can't be reproduced externally.
func rowContainsPath(row map[string]interface{}, projectPath string) bool {
	if projectPath == "" {
		return false
	}
	for _, v := range row {
		if s, ok := v.(string); ok && s != "" && strings.Contains(s, projectPath) {
			return true
		}
	}
	return false
}

// parseSQLiteTime attempts to parse a timestamp value from a SQLite row,
// which may be a formatted string or a numeric Unix timestamp (seconds or
// milliseconds).
func parseSQLiteTime(v interface{}) (time.Time, bool) {
	switch val := v.(type) {
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return time.Time{}, false
		}
		formats := []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05.000Z",
			"2006-01-02 15:04:05",
		}
		for _, f := range formats {
			if t, err := time.Parse(f, s); err == nil {
				return t, true
			}
		}
		return time.Time{}, false
	case float64:
		if val > 1e12 {
			return time.UnixMilli(int64(val)), true
		}
		if val > 0 {
			return time.Unix(int64(val), 0), true
		}
		return time.Time{}, false
	default:
		return time.Time{}, false
	}
}

// sqlQuote escapes single quotes for safe interpolation into a SQL string
// literal.
func sqlQuote(s string) string {
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
