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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session belonging to this project. OpenCode's on-disk schema (table/column
// names, JSON blob columns) has changed across releases, so instead of
// hardcoding column names this introspects the schema at query time via
// sqlite3's "-json" output mode and PRAGMA table_info.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findSQLiteDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	rows, err := sqliteQueryJSON(dbPath, "SELECT * FROM session ORDER BY rowid DESC LIMIT 25;")
	if err != nil || len(rows) == 0 {
		return nil, nil
	}

	// Prefer a row that actually references this project; fall back to the
	// most recent row overall if project scoping can't be determined from
	// the available columns/data.
	chosen := rows[0]
	for _, row := range rows {
		if rowMatchesProject(row, projectPath, projectID) {
			chosen = row
			break
		}
	}

	sessionID := extractStringField(chosen, "id", "sessionID", "session_id")
	if sessionID == "" {
		return nil, nil
	}

	if t := extractTimeField(chosen); !t.IsZero() && time.Since(t) > agent.RecentSessionTimeout {
		return nil, nil
	}

	msgCols := sqliteTableColumns(dbPath, "message")
	var whereClauses []string
	if containsColumn(msgCols, "session_id") {
		whereClauses = append(whereClauses, "session_id="+quoteSQLiteString(sessionID))
	}
	if containsColumn(msgCols, "data") {
		for _, jsonPath := range []string{"$.sessionID", "$.session_id", "$.session.id"} {
			whereClauses = append(whereClauses,
				fmt.Sprintf("json_extract(data,'%s')=%s", jsonPath, quoteSQLiteString(sessionID)))
		}
	}
	if len(whereClauses) == 0 {
		return nil, nil
	}

	orderCol := "rowid"
	for _, c := range []string{"time_created", "created_at", "time"} {
		if containsColumn(msgCols, c) {
			orderCol = c
			break
		}
	}

	msgQuery := fmt.Sprintf("SELECT * FROM message WHERE %s ORDER BY %s ASC;",
		strings.Join(whereClauses, " OR "), orderCol)
	msgRows, err := sqliteQueryJSON(dbPath, msgQuery)
	if err != nil || len(msgRows) == 0 {
		return nil, nil
	}

	var entries []map[string]interface{}
	for _, row := range msgRows {
		entry := row
		if raw, ok := row["data"].(string); ok && raw != "" {
			var nested map[string]interface{}
			if json.Unmarshal([]byte(raw), &nested) == nil {
				entry = nested
			}
		}
		if _, ok := entry["id"]; !ok {
			if id := extractStringField(row, "id"); id != "" {
				entry["id"] = id
			}
		}
		entries = append(entries, entry)
	}

	transcriptData, err := json.Marshal(entries)
	if err != nil || len(entries) == 0 {
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

// findSQLiteDB locates OpenCode's SQLite database file, trying known names
// before falling back to any *.db/*.sqlite* file directly under dataDir.
func findSQLiteDB(dataDir string) string {
	for _, name := range []string{"opencode.db", "opencode.sqlite", "storage.db", "db.sqlite3", "data.db"} {
		p := filepath.Join(dataDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, pattern := range []string{"*.db", "*.sqlite*"} {
		matches, _ := filepath.Glob(filepath.Join(dataDir, pattern))
		if len(matches) > 0 {
			return matches[0]
		}
	}
	return ""
}

// sqliteQueryJSON runs a query against a SQLite database and returns the
// rows as generic maps, using sqlite3's "-json" output mode so column names
// are discovered from the result instead of assumed ahead of time.
func sqliteQueryJSON(dbPath, query string) ([]map[string]interface{}, error) {
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// sqliteTableColumns returns the column names of a SQLite table.
func sqliteTableColumns(dbPath, table string) []string {
	rows, err := sqliteQueryJSON(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return nil
	}
	var cols []string
	for _, row := range rows {
		if name, ok := row["name"].(string); ok {
			cols = append(cols, name)
		}
	}
	return cols
}

func containsColumn(cols []string, name string) bool {
	for _, c := range cols {
		if c == name {
			return true
		}
	}
	return false
}

func quoteSQLiteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// rowMatchesProject reports whether a session row appears to belong to the
// given project by searching its values (including nested JSON blob
// columns) for the project path or project ID, without assuming which
// column holds that information.
func rowMatchesProject(row map[string]interface{}, projectPath, projectID string) bool {
	if projectPath == "" {
		return false
	}
	blob := flattenRowText(row)
	if strings.Contains(blob, projectPath) {
		return true
	}
	if projectID != "" && projectID != "global" && strings.Contains(blob, projectID) {
		return true
	}
	return false
}

func flattenRowText(row map[string]interface{}) string {
	var sb strings.Builder
	for _, v := range row {
		appendValueText(&sb, v)
	}
	return sb.String()
}

func appendValueText(sb *strings.Builder, v interface{}) {
	switch val := v.(type) {
	case string:
		sb.WriteString(val)
		sb.WriteString(" ")
		var nested interface{}
		if json.Unmarshal([]byte(val), &nested) == nil {
			appendValueText(sb, nested)
		}
	case map[string]interface{}:
		for _, vv := range val {
			appendValueText(sb, vv)
		}
	case []interface{}:
		for _, vv := range val {
			appendValueText(sb, vv)
		}
	}
}

// extractStringField returns the first non-empty string value found under
// any of the given keys, checking the row directly and then any nested
// JSON blob stored in a "data" column.
func extractStringField(row map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if s, ok := row[k].(string); ok && s != "" {
			return s
		}
	}
	if raw, ok := row["data"].(string); ok {
		var nested map[string]interface{}
		if json.Unmarshal([]byte(raw), &nested) == nil {
			for _, k := range keys {
				if s, ok := nested[k].(string); ok && s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// extractTimeField returns the best-effort timestamp for a session row,
// checking common column names and nested time.{created,updated} fields.
func extractTimeField(row map[string]interface{}) time.Time {
	keys := []string{"time_updated", "updated_at", "timeUpdated", "time_created", "created_at"}
	for _, k := range keys {
		if v, ok := row[k]; ok {
			if t := parseFlexibleTime(v); !t.IsZero() {
				return t
			}
		}
	}
	raw, ok := row["data"].(string)
	if !ok {
		return time.Time{}
	}
	var nested map[string]interface{}
	if json.Unmarshal([]byte(raw), &nested) != nil {
		return time.Time{}
	}
	if timeObj, ok := nested["time"].(map[string]interface{}); ok {
		for _, k := range []string{"updated", "created"} {
			if v, ok := timeObj[k]; ok {
				if t := parseFlexibleTime(v); !t.IsZero() {
					return t
				}
			}
		}
	}
	for _, k := range keys {
		if v, ok := nested[k]; ok {
			if t := parseFlexibleTime(v); !t.IsZero() {
				return t
			}
		}
	}
	return time.Time{}
}

// parseFlexibleTime parses a timestamp that may be a unix epoch (seconds or
// milliseconds) or one of several common string formats.
func parseFlexibleTime(v interface{}) time.Time {
	switch val := v.(type) {
	case float64:
		switch {
		case val > 1e12:
			return time.UnixMilli(int64(val))
		case val > 1e9:
			return time.Unix(int64(val), 0)
		}
	case string:
		layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
		for _, layout := range layouts {
			if t, err := time.Parse(layout, val); err == nil {
				return t
			}
		}
	}
	return time.Time{}
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
