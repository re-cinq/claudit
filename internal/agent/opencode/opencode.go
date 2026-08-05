package opencode

import (
	"bytes"
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
// OpenCode's SQLite schema has changed across releases (table and column renames),
// so instead of hardcoding names, the schema is introspected at query time via
// sqlite_master and PRAGMA table_info, and the best-matching table/column names are
// picked from a set of known candidates. This keeps discovery working across schema
// drift rather than silently failing (a bad table/column name makes the sqlite3 CLI
// exit non-zero, which previously caused DiscoverSession to report "no session found").
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	tables, err := sqliteTableNames(dbPath)
	if err != nil || len(tables) == 0 {
		return nil, nil
	}

	sessionTable := findSQLiteTable(tables, "session")
	if sessionTable == "" {
		return nil, nil
	}
	messageTable := findSQLiteTable(tables, "message")

	sessionCols, err := sqliteColumnNames(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := findSQLiteColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := findSQLiteColumn(sessionCols, "project_id", "projectid", "project", "directory", "cwd", "path")
	timeCol := findSQLiteColumn(sessionCols, "time_updated", "updated_at", "updated", "mtime", "time_created", "created_at")

	sessionID, updatedAt := querySQLiteSession(dbPath, sessionTable, idCol, projectCol, timeCol, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout). If the timestamp can't
	// be parsed in any known format, proceed anyway — better to try than skip.
	if updatedAt != "" && !isRecentTimestamp(updatedAt) {
		return nil, nil
	}

	var transcriptData []byte
	if messageTable != "" {
		if msgCols, err := sqliteColumnNames(dbPath, messageTable); err == nil && len(msgCols) > 0 {
			transcriptData = querySQLiteMessages(dbPath, messageTable, msgCols, sessionID)
		}
	}

	if len(transcriptData) == 0 {
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

// sqlQuoteLiteral escapes a string for use as a single-quoted SQLite string literal.
func sqlQuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// sqlQuoteIdent quotes a SQLite identifier (table/column name).
func sqlQuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// sqliteTableNames lists user table names in the database.
func sqliteTableNames(dbPath string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var tables []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			tables = append(tables, line)
		}
	}
	return tables, nil
}

// sqliteColumnNames lists column names for a table via PRAGMA table_info.
func sqliteColumnNames(dbPath, table string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", sqlQuoteIdent(table)))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) >= 2 {
			cols = append(cols, parts[1])
		}
	}
	return cols, nil
}

// findSQLiteTable returns the table whose name best matches keyword, preferring
// an exact (case-insensitive) match, then any table whose name contains keyword.
func findSQLiteTable(tables []string, keyword string) string {
	for _, t := range tables {
		if strings.EqualFold(t, keyword) {
			return t
		}
	}
	for _, t := range tables {
		if strings.Contains(strings.ToLower(t), keyword) {
			return t
		}
	}
	return ""
}

// findSQLiteColumn returns the first column matching one of the candidate names
// (case-insensitive), in priority order.
func findSQLiteColumn(cols []string, candidates ...string) string {
	byLower := make(map[string]string, len(cols))
	for _, c := range cols {
		byLower[strings.ToLower(c)] = c
	}
	for _, cand := range candidates {
		if actual, ok := byLower[strings.ToLower(cand)]; ok {
			return actual
		}
	}
	return ""
}

// querySQLiteSession finds the most recently updated session, preferring one whose
// project column matches projectID or projectPath. If no project column was found,
// or neither value matches any row, it falls back to the most recently updated
// session overall (relying on the recency check in the caller to reject stale rows).
// Returns the session ID and its raw "updated at" value (empty if no time column
// was found).
func querySQLiteSession(dbPath, table, idCol, projectCol, timeCol, projectID, projectPath string) (string, string) {
	selectCols := sqlQuoteIdent(idCol)
	orderBy := ""
	if timeCol != "" {
		selectCols += ", " + sqlQuoteIdent(timeCol)
		orderBy = fmt.Sprintf(" ORDER BY %s DESC", sqlQuoteIdent(timeCol))
	}

	runQuery := func(where string) (string, string) {
		query := fmt.Sprintf("SELECT %s FROM %s", selectCols, sqlQuoteIdent(table))
		if where != "" {
			query += " WHERE " + where
		}
		query += orderBy + " LIMIT 1;"

		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		out, err := cmd.Output()
		if err != nil {
			return "", ""
		}
		line := strings.TrimSpace(string(out))
		if line == "" {
			return "", ""
		}
		parts := strings.SplitN(line, "\t", 2)
		id := strings.TrimSpace(parts[0])
		updatedAt := ""
		if len(parts) > 1 {
			updatedAt = strings.TrimSpace(parts[1])
		}
		return id, updatedAt
	}

	if projectCol != "" {
		for _, val := range []string{projectID, projectPath} {
			if val == "" {
				continue
			}
			where := fmt.Sprintf("%s=%s", sqlQuoteIdent(projectCol), sqlQuoteLiteral(val))
			if id, updatedAt := runQuery(where); id != "" {
				return id, updatedAt
			}
		}
	}

	if timeCol != "" {
		return runQuery("")
	}

	return "", ""
}

// querySQLiteMessages fetches all messages for a session as a JSON array,
// matching each message's id back into its data blob (mirroring the shape the
// rest of the OpenCode transcript parser already expects).
func querySQLiteMessages(dbPath, table string, cols []string, sessionID string) []byte {
	sessionCol := findSQLiteColumn(cols, "session_id", "sessionid", "session")
	dataCol := findSQLiteColumn(cols, "data", "content", "json", "body")
	idCol := findSQLiteColumn(cols, "id")
	timeCol := findSQLiteColumn(cols, "time_created", "created_at", "created", "time")

	if sessionCol == "" || dataCol == "" {
		return nil
	}

	selectExpr := sqlQuoteIdent(dataCol)
	if idCol != "" {
		selectExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", sqlQuoteIdent(dataCol), sqlQuoteIdent(idCol))
	}

	query := fmt.Sprintf(
		"SELECT json_group_array(%s) FROM %s WHERE %s=%s",
		selectExpr, sqlQuoteIdent(table), sqlQuoteIdent(sessionCol), sqlQuoteLiteral(sessionID),
	)
	if timeCol != "" {
		query += fmt.Sprintf(" ORDER BY %s", sqlQuoteIdent(timeCol))
	}
	query += ";"

	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	data := bytes.TrimSpace(out)
	// sqlite3 returns "[null]" when no rows match, "[]" for an empty group.
	if len(data) == 0 || string(data) == "[null]" || string(data) == "[]" {
		return nil
	}
	return data
}

// isRecentTimestamp reports whether a timestamp string, in any of OpenCode's known
// formats (RFC3339 variants, space-separated SQL datetime, or numeric Unix epoch
// seconds/milliseconds), falls within the recent-session timeout. Unparseable
// timestamps are treated as recent — better to try than skip.
func isRecentTimestamp(timeStr string) bool {
	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		var t time.Time
		if n > 1e12 {
			t = time.UnixMilli(n)
		} else {
			t = time.Unix(n, 0)
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
