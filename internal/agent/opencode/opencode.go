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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
//
// OpenCode's SQLite schema has changed table and column names across
// releases (only the session table's "project_id"/"id" and the message
// table's "data" column have ever been confirmed against a real database),
// so the actual table/column names are discovered at runtime via
// PRAGMA table_info rather than hardcoded. This keeps discovery working
// across OpenCode releases that rename or add columns, and avoids relying
// on SQL JSON functions (json_patch/json_group_array) that were never
// verified to exist in the sqlite3 build used by OpenCode's users.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, sessionCols := firstNonEmptyTable(dbPath, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := pickColumn(sessionCols, "project_id", "projectID", "project")
	dirCol := pickColumn(sessionCols, "directory", "dir", "cwd")
	timeCol := pickColumn(sessionCols, "time_updated", "updated_at", "updatedAt", "time_created", "created_at", "createdAt")

	var where string
	switch {
	case projectCol != "":
		where = fmt.Sprintf("WHERE %s='%s'", projectCol, sqliteQuote(projectID))
	case dirCol != "":
		where = fmt.Sprintf("WHERE %s='%s'", dirCol, sqliteQuote(projectPath))
	}

	selectCols := idCol
	orderBy := ""
	if timeCol != "" {
		selectCols += ", " + timeCol
		orderBy = fmt.Sprintf("ORDER BY %s DESC", timeCol)
	}

	sessionQuery := strings.TrimSpace(fmt.Sprintf(
		`SELECT %s FROM %s %s %s LIMIT 1;`, selectCols, sessionTable, where, orderBy,
	))
	sessionRows, err := sqliteQueryJSON(dbPath, sessionQuery)
	if err != nil || len(sessionRows) == 0 {
		return nil, nil
	}

	sessionID, _ := sessionRows[0][idCol].(string)
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout). If the timestamp
	// can't be read or interpreted, proceed anyway — better to try than skip.
	if timeCol != "" {
		if raw, ok := sessionRows[0][timeCol]; ok && !isRecentSessionTime(raw) {
			return nil, nil
		}
	}

	messageTable, msgCols := firstNonEmptyTable(dbPath, "message", "messages")
	if messageTable == "" {
		return nil, nil
	}

	msgIDCol := pickColumn(msgCols, "id")
	dataCol := pickColumn(msgCols, "data", "parts", "content")
	sessionLinkCol := pickColumn(msgCols, "session_id", "sessionID", "session")
	orderCol := pickColumn(msgCols, "time_created", "created_at", "createdAt")
	if orderCol == "" {
		orderCol = msgIDCol
	}
	if dataCol == "" || sessionLinkCol == "" {
		return nil, nil
	}

	msgSelectCols := dataCol
	if msgIDCol != "" && msgIDCol != dataCol {
		msgSelectCols = msgIDCol + ", " + dataCol
	}

	msgQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s'`,
		msgSelectCols, messageTable, sessionLinkCol, sqliteQuote(sessionID))
	if orderCol != "" {
		msgQuery += fmt.Sprintf(" ORDER BY %s", orderCol)
	}
	msgQuery += ";"

	msgRows, err := sqliteQueryJSON(dbPath, msgQuery)
	if err != nil {
		return nil, nil
	}

	var messages []json.RawMessage
	for _, row := range msgRows {
		dataStr, _ := row[dataCol].(string)
		if dataStr == "" {
			continue
		}
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(dataStr), &entry); err != nil {
			continue
		}
		if msgIDCol != "" {
			if _, hasID := entry["id"]; !hasID {
				if idVal, ok := row[msgIDCol]; ok {
					entry["id"] = idVal
				}
			}
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		messages = append(messages, encoded)
	}

	if len(messages) == 0 {
		return nil, nil
	}

	transcriptData, err := json.Marshal(messages)
	if err != nil {
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

// sqliteQueryJSON runs a SQL query against dbPath and decodes the result as
// a slice of column-name-keyed rows using sqlite3's -json output mode.
func sqliteQueryJSON(dbPath, query string) ([]map[string]interface{}, error) {
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

// firstNonEmptyTable returns the first candidate table name that exists in
// dbPath (along with its column names), or "" if none do. Table and column
// names in OpenCode's SQLite database have changed across releases.
func firstNonEmptyTable(dbPath string, candidates ...string) (string, []string) {
	for _, name := range candidates {
		cols, err := sqliteTableColumns(dbPath, name)
		if err == nil && len(cols) > 0 {
			return name, cols
		}
	}
	return "", nil
}

// sqliteTableColumns returns the column names of a SQLite table via
// PRAGMA table_info. Returns an empty slice (no error) if the table doesn't exist.
func sqliteTableColumns(dbPath, table string) ([]string, error) {
	rows, err := sqliteQueryJSON(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
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

// pickColumn returns the first candidate (case-insensitively) present in cols, or "".
func pickColumn(cols []string, candidates ...string) string {
	lookup := make(map[string]string, len(cols))
	for _, c := range cols {
		lookup[strings.ToLower(c)] = c
	}
	for _, cand := range candidates {
		if actual, ok := lookup[strings.ToLower(cand)]; ok {
			return actual
		}
	}
	return ""
}

// sqliteQuote escapes single quotes for use in a SQLite string literal.
func sqliteQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// isRecentSessionTime reports whether a session timestamp value (a Unix
// epoch number in seconds/milliseconds/microseconds/nanoseconds, or a
// formatted date string) falls within the recent-session window. Returns
// true if the value can't be interpreted — better to try than skip silently.
func isRecentSessionTime(v interface{}) bool {
	switch val := v.(type) {
	case float64:
		return time.Since(unixToTime(val)) <= agent.RecentSessionTimeout
	case string:
		formats := []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
		for _, f := range formats {
			if t, err := time.Parse(f, val); err == nil {
				return time.Since(t) <= agent.RecentSessionTimeout
			}
		}
	}
	return true
}

// unixToTime converts a Unix epoch number to time.Time, auto-detecting
// whether it's expressed in seconds, milliseconds, microseconds, or nanoseconds.
func unixToTime(v float64) time.Time {
	switch {
	case v >= 1e18:
		return time.Unix(0, int64(v))
	case v >= 1e15:
		return time.UnixMicro(int64(v))
	case v >= 1e12:
		return time.UnixMilli(int64(v))
	default:
		return time.Unix(int64(v), 0)
	}
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
