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
		dataDir = ""
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
// session belonging to projectPath.
//
// OpenCode's on-disk schema has changed across versions: the database file
// itself may live under the global data directory or inside a project-local
// ".opencode" directory, table names have been seen as both singular
// ("session"/"message") and plural ("sessions"/"messages"), and the columns
// used for project scoping, ordering, and message content differ (e.g. a
// "project_id" column keyed off a git commit hash vs. a "directory" column
// holding the working directory, or a single "data" JSON blob per message vs.
// separate "role"/"parts" columns). Rather than hard-coding one shape, this
// introspects the actual schema at query time so discovery keeps working
// across these variations.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findOpenCodeDB(dataDir, projectPath)
	if dbPath == "" || !sqliteAvailable() {
		return nil, nil
	}

	sessionTable := sqliteFindTable(dbPath, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}
	sessionCols := sqliteColumnNames(dbPath, sessionTable)
	idCol := sqliteFirstColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := sqliteFirstColumn(sessionCols, "time_updated", "updated_at", "time_created", "created_at")
	scopeCol := sqliteFirstColumn(sessionCols, "project_id", "directory", "cwd", "project_path")

	// Find the most recent session, scoped to this project when a scoping
	// column is present. Older schemas scope by a "project_id" derived from
	// the git root commit; newer schemas may instead store the working
	// directory directly, or not scope sessions per-project at all (e.g. when
	// the database itself is project-local).
	query := fmt.Sprintf("SELECT %s FROM %s", idCol, sessionTable)
	switch scopeCol {
	case "project_id":
		query += fmt.Sprintf(" WHERE project_id='%s'", sqlEscape(projectID))
	case "directory", "cwd", "project_path":
		query += fmt.Sprintf(" WHERE %s='%s'", scopeCol, sqlEscape(projectPath))
	}
	if timeCol != "" {
		query += fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}
	query += " LIMIT 1;"

	sessionID, err := sqliteQuery(dbPath, query)
	if err != nil || sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout).
	if timeCol != "" {
		timeStr, err := sqliteQuery(dbPath, fmt.Sprintf(
			"SELECT %s FROM %s WHERE %s='%s';", timeCol, sessionTable, idCol, sqlEscape(sessionID)))
		if err == nil && timeStr != "" && !isRecentTimestamp(timeStr) {
			return nil, nil
		}
	}

	messageTable := sqliteFindTable(dbPath, "message", "messages")
	transcriptData := readOpenCodeMessages(dbPath, messageTable, sessionID)
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

// findOpenCodeDB locates the OpenCode SQLite database. Older versions keep it
// under the global XDG data directory; newer versions have been seen to keep
// a project-local database instead.
func findOpenCodeDB(dataDir, projectPath string) string {
	var candidates []string
	if dataDir != "" {
		candidates = append(candidates, filepath.Join(dataDir, "opencode.db"))
	}
	candidates = append(candidates, filepath.Join(projectPath, ".opencode", "opencode.db"))

	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// sqliteAvailable reports whether the sqlite3 CLI is on PATH.
func sqliteAvailable() bool {
	_, err := exec.LookPath("sqlite3")
	return err == nil
}

// sqliteQuery runs a single SQL statement against dbPath and returns its
// trimmed stdout, or an error if sqlite3 fails (e.g. unknown table/column).
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sqlEscape escapes single quotes for use inside a SQL string literal.
func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// sqliteFindTable returns the first candidate table name that actually
// exists in the database, or "" if none do.
func sqliteFindTable(dbPath string, candidates ...string) string {
	for _, name := range candidates {
		out, err := sqliteQuery(dbPath, fmt.Sprintf(
			`SELECT name FROM sqlite_master WHERE type='table' AND name='%s';`, name))
		if err == nil && out == name {
			return name
		}
	}
	return ""
}

// sqliteColumnNames returns the column names of a table via PRAGMA table_info.
func sqliteColumnNames(dbPath, table string) []string {
	out, err := sqliteQuery(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil || out == "" {
		return nil
	}
	var cols []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// sqliteFirstColumn returns the first candidate column name present in cols.
func sqliteFirstColumn(cols []string, candidates ...string) string {
	for _, cand := range candidates {
		for _, c := range cols {
			if c == cand {
				return cand
			}
		}
	}
	return ""
}

// isRecentTimestamp reports whether raw (an ISO-8601 string, or a Unix epoch
// integer in seconds or milliseconds) falls within RecentSessionTimeout. If
// the value can't be parsed in any known format, it's treated as recent so
// discovery proceeds rather than silently failing on an unrecognized format.
func isRecentTimestamp(raw string) bool {
	formats := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
	for _, f := range formats {
		if t, err := time.Parse(f, raw); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		t := time.Unix(n, 0)
		if n > 1e12 {
			t = time.UnixMilli(n)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}
	return true
}

// readOpenCodeMessages reads all messages for a session from the message
// table and returns them as a JSON array. It handles both a legacy schema
// (a single "data" column holding the full message blob) and a newer schema
// (separate "role"/"parts"/"model" columns, no combined blob column).
func readOpenCodeMessages(dbPath, table, sessionID string) []byte {
	if table == "" {
		return nil
	}
	cols := sqliteColumnNames(dbPath, table)
	idCol := sqliteFirstColumn(cols, "id")
	sessionCol := sqliteFirstColumn(cols, "session_id")
	orderCol := sqliteFirstColumn(cols, "time_created", "created_at", "time_updated", "updated_at")
	if idCol == "" || sessionCol == "" {
		return nil
	}

	orderBy := ""
	if orderCol != "" {
		orderBy = fmt.Sprintf(" ORDER BY %s", orderCol)
	}

	if dataCol := sqliteFirstColumn(cols, "data"); dataCol != "" {
		query := fmt.Sprintf(
			"SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s'%s;",
			dataCol, idCol, table, sessionCol, sqlEscape(sessionID), orderBy)
		out, err := sqliteQuery(dbPath, query)
		if err == nil && out != "" && out != "[null]" && out != "[]" {
			return []byte(out)
		}
		return nil
	}

	// Newer schema: message rows carry role/parts/model directly rather than
	// a single combined "data" blob.
	selectCols := []string{idCol, sessionCol}
	for _, c := range []string{"role", "model"} {
		if col := sqliteFirstColumn(cols, c); col != "" {
			selectCols = append(selectCols, col)
		}
	}
	if partsCol := sqliteFirstColumn(cols, "parts", "content"); partsCol != "" {
		selectCols = append(selectCols, partsCol)
	}
	if orderCol != "" {
		selectCols = append(selectCols, orderCol)
	}

	fields := make([]string, len(selectCols))
	for i, c := range selectCols {
		fields[i] = fmt.Sprintf("'%s', %s", c, c)
	}
	query := fmt.Sprintf(
		"SELECT json_group_array(json_object(%s)) FROM %s WHERE %s='%s'%s;",
		strings.Join(fields, ", "), table, sessionCol, sqlEscape(sessionID), orderBy)
	out, err := sqliteQuery(dbPath, query)
	if err != nil || out == "" || out == "[null]" || out == "[]" {
		return nil
	}
	return []byte(out)
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

	// Try "parts" field: newer OpenCode versions store message content as a
	// typed parts array (e.g. {"type":"text","data":{"text":"..."}}) instead
	// of a plain "content" string/array.
	if partsRaw, ok := raw["parts"]; ok {
		var parts []struct {
			Type string          `json:"type"`
			Text string          `json:"text"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(partsRaw, &parts); err == nil && len(parts) > 0 {
			var blocks []agent.ContentBlock
			for _, p := range parts {
				text := p.Text
				if text == "" && len(p.Data) > 0 {
					var inner struct {
						Text string `json:"text"`
					}
					if err := json.Unmarshal(p.Data, &inner); err == nil {
						text = inner.Text
					}
				}
				if p.Type == "text" || text != "" {
					blocks = append(blocks, agent.ContentBlock{Type: p.Type, Text: text})
				}
			}
			if len(blocks) > 0 {
				msg.Content = blocks
				return msg
			}
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
