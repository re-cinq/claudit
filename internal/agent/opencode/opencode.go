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
// OpenCode's internal schema has changed across releases (table and column
// names have been renamed as the storage layer evolved), so rather than
// hardcoding exact names we introspect the schema at query time and adapt
// to whatever table/column names are actually present. This keeps manual
// (post-commit hook) session discovery working across OpenCode upgrades
// without needing a code change for every renamed column.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := sqliteFindTable(dbPath, "session")
	if sessionTable == "" {
		return nil, nil
	}

	idCol := sqliteFindColumn(dbPath, sessionTable, "id")
	if idCol == "" {
		idCol = "id"
	}
	timeCol := sqliteFindColumn(dbPath, sessionTable, "time_updated", "updated_at", "updatedat", "updated", "time_created", "time")
	projectCol := sqliteFindColumn(dbPath, sessionTable, "project_id", "projectid", "project", "directory", "worktree", "cwd", "path")

	orderClause := " ORDER BY rowid DESC"
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}

	var sessionID string
	if projectCol != "" {
		query := fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s' OR %s='%s'%s LIMIT 1;`,
			idCol, sessionTable,
			projectCol, sqliteEscape(projectID),
			projectCol, sqliteEscape(projectPath),
			orderClause,
		)
		if rows, err := sqliteQuery(dbPath, query); err == nil && len(rows) > 0 {
			sessionID = strings.TrimSpace(rows[0])
		}
	}

	if sessionID == "" {
		// No project-scoping column found (or it didn't match). Fall back to
		// the most recently updated session overall; the recency check below
		// guards against picking up a stale, unrelated session.
		query := fmt.Sprintf(`SELECT %s FROM %s%s LIMIT 1;`, idCol, sessionTable, orderClause)
		rows, err := sqliteQuery(dbPath, query)
		if err != nil || len(rows) == 0 {
			return nil, nil
		}
		sessionID = strings.TrimSpace(rows[0])
	}

	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout)
	if timeCol != "" {
		query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, timeCol, sessionTable, idCol, sqliteEscape(sessionID))
		if rows, err := sqliteQuery(dbPath, query); err == nil && len(rows) > 0 {
			if t, ok := parseSQLiteTime(rows[0]); ok && time.Since(t) > agent.RecentSessionTimeout {
				return nil, nil
			}
			// If we can't parse the time, proceed anyway — better to try than skip
		}
	}

	messageTable := sqliteFindTable(dbPath, "message")
	if messageTable == "" {
		return nil, nil
	}

	msgIDCol := sqliteFindColumn(dbPath, messageTable, "id")
	if msgIDCol == "" {
		msgIDCol = "id"
	}
	sessionRefCol := sqliteFindColumn(dbPath, messageTable, "session_id", "sessionid", "session")
	dataCol := sqliteFindColumn(dbPath, messageTable, "data", "content", "body", "message")
	createdCol := sqliteFindColumn(dbPath, messageTable, "time_created", "created_at", "createdat", "created", "time")

	if sessionRefCol == "" || dataCol == "" {
		return nil, nil
	}

	orderMsg := ""
	if createdCol != "" {
		orderMsg = fmt.Sprintf(" ORDER BY %s", createdCol)
	}

	// Get messages for this session as a JSON array
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s'%s;`,
		dataCol, msgIDCol, messageTable, sessionRefCol, sqliteEscape(sessionID), orderMsg,
	)
	msgRows, err := sqliteQuery(dbPath, msgQuery)
	if err != nil || len(msgRows) == 0 {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(msgRows[0]))
	// sqlite3 returns "[null]" when no rows match
	if len(transcriptData) == 0 || string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
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

// sqliteQuery runs a query against the given SQLite database and returns its
// output split into lines (each line being one row, tab-separated columns).
func sqliteQuery(dbPath, query string) ([]string, error) {
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}

// sqliteFindTable returns the name of the table best matching the given
// singular name (e.g. "session" matches "session" or "sessions"), or "" if
// no table could be found. This allows discovery to survive OpenCode
// renaming its tables between releases.
func sqliteFindTable(dbPath, name string) string {
	rows, err := sqliteQuery(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return ""
	}
	want := strings.ToLower(name)
	var contains string
	for _, row := range rows {
		table := strings.TrimSpace(row)
		lower := strings.ToLower(table)
		if lower == want || lower == want+"s" {
			return table
		}
		if contains == "" && strings.Contains(lower, want) {
			contains = table
		}
	}
	return contains
}

// sqliteFindColumn returns the name of the first column on the given table
// that matches one of the candidate names (exact match preferred, then
// substring match), or "" if none match. Candidates should be ordered from
// most to least preferred.
func sqliteFindColumn(dbPath, table string, candidates ...string) string {
	rows, err := sqliteQuery(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return ""
	}
	var columns []string
	for _, row := range rows {
		fields := strings.Split(row, "\t")
		if len(fields) > 1 {
			columns = append(columns, fields[1])
		}
	}
	for _, candidate := range candidates {
		want := strings.ToLower(candidate)
		for _, col := range columns {
			if strings.ToLower(col) == want {
				return col
			}
		}
	}
	for _, candidate := range candidates {
		want := strings.ToLower(candidate)
		for _, col := range columns {
			if strings.Contains(strings.ToLower(col), want) {
				return col
			}
		}
	}
	return ""
}

// parseSQLiteTime attempts to parse a timestamp value read from SQLite,
// trying the formats OpenCode has used across releases (RFC3339 variants,
// a plain SQL datetime, and Unix epoch seconds/milliseconds).
func parseSQLiteTime(raw string) (time.Time, bool) {
	timeStr := strings.TrimSpace(raw)
	if timeStr == "" {
		return time.Time{}, false
	}
	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, format := range formats {
		if t, err := time.Parse(format, timeStr); err == nil {
			return t, true
		}
	}
	if millis, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		// Distinguish seconds vs milliseconds epoch by magnitude.
		if millis > 1e12 {
			return time.UnixMilli(millis), true
		}
		return time.Unix(millis, 0), true
	}
	return time.Time{}, false
}

// sqliteEscape escapes single quotes for safe inclusion in a SQL string literal.
func sqliteEscape(s string) string {
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
