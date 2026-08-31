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

// findOpenCodeDB locates OpenCode's SQLite database file within dataDir.
// The filename has changed across releases, so several known candidates
// are checked before falling back to any *.db/*.sqlite* file found
// directly under dataDir.
func findOpenCodeDB(dataDir string) string {
	candidates := []string{"opencode.db", "opencode.sqlite", "db.sqlite", "state.db"}
	for _, name := range candidates {
		p := filepath.Join(dataDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".sqlite") || strings.HasSuffix(name, ".sqlite3") {
			return filepath.Join(dataDir, name)
		}
	}
	return ""
}

// sqliteQuery runs a single sqlite3 statement and returns its trimmed output.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sqliteEscape escapes a value for safe inclusion in a single-quoted SQLite
// string literal.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// sqliteFindTable returns the shortest table name containing the given
// substring (case-insensitive), or "" if none is found. OpenCode has
// changed its SQLite table names across releases.
func sqliteFindTable(dbPath, contains string) string {
	out, err := sqliteQuery(dbPath, fmt.Sprintf(
		"SELECT name FROM sqlite_master WHERE type='table' AND lower(name) LIKE '%%%s%%' ORDER BY length(name) LIMIT 1;",
		strings.ToLower(contains)))
	if err != nil || out == "" {
		return ""
	}
	return strings.Split(out, "\n")[0]
}

// sqliteColumns returns the column names of a table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// sqliteFindColumn returns the first column matching any candidate name
// (exact match preferred, then substring match), or "" if none match.
func sqliteFindColumn(cols []string, candidates ...string) string {
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.EqualFold(c, cand) {
				return c
			}
		}
	}
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.Contains(strings.ToLower(c), strings.ToLower(cand)) {
				return c
			}
		}
	}
	return ""
}

// parseSQLiteTime parses a session timestamp in any of the formats OpenCode
// has used across releases: RFC3339, SQLite datetime, or a raw Unix epoch
// in seconds, milliseconds, or microseconds.
func parseSQLiteTime(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, true
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		switch {
		case n > 1e14:
			return time.UnixMicro(n), true
		case n > 1e11:
			return time.UnixMilli(n), true
		default:
			return time.Unix(n, 0), true
		}
	}
	return time.Time{}, false
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session. Table and column names are discovered at query time
// (rather than hardcoded) since OpenCode has renamed its schema across
// releases; a hardcoded query would silently find nothing after such a
// rename since sqlite3 errors are treated as "no session found".
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findOpenCodeDB(dataDir)
	if dbPath == "" {
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
	sessionCols := sqliteColumns(dbPath, sessionTable)

	idCol := sqliteFindColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := sqliteFindColumn(sessionCols,
		"time_updated", "updated_at", "timeUpdated", "updatedAt",
		"time_created", "created_at", "updated", "created")
	dirCol := sqliteFindColumn(sessionCols, "directory", "cwd", "path", "workdir", "project_path")
	projectCol := sqliteFindColumn(sessionCols, "project_id", "projectid", "project")

	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}

	// Find most recent session for this project. Prefer filtering by working
	// directory when available (most direct signal), then the computed
	// project id.
	var sessionID string
	switch {
	case dirCol != "":
		sessionID, _ = sqliteQuery(dbPath, fmt.Sprintf(
			"SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;",
			idCol, sessionTable, dirCol, sqliteEscape(projectPath), orderClause))
	case projectCol != "":
		sessionID, _ = sqliteQuery(dbPath, fmt.Sprintf(
			"SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;",
			idCol, sessionTable, projectCol, sqliteEscape(projectID), orderClause))
	}

	// Fall back to the most recently updated session overall when we
	// couldn't filter by project (e.g. the matching column was renamed) or
	// nothing matched — better to try the most likely session than skip.
	if sessionID == "" {
		sessionID, _ = sqliteQuery(dbPath, fmt.Sprintf(
			"SELECT %s FROM %s%s LIMIT 1;", idCol, sessionTable, orderClause))
	}

	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout)
	if timeCol != "" {
		timeStr, err := sqliteQuery(dbPath, fmt.Sprintf(
			"SELECT %s FROM %s WHERE %s='%s';", timeCol, sessionTable, idCol, sqliteEscape(sessionID)))
		if err == nil && timeStr != "" {
			if t, ok := parseSQLiteTime(timeStr); ok && time.Since(t) > agent.RecentSessionTimeout {
				return nil, nil
			}
			// If we can't parse the time, proceed anyway — better to try than skip.
		}
	}

	msgTable := sqliteFindTable(dbPath, "message")
	if msgTable == "" {
		return nil, nil
	}
	msgCols := sqliteColumns(dbPath, msgTable)
	msgDataCol := sqliteFindColumn(msgCols, "data")
	msgSessionCol := sqliteFindColumn(msgCols, "session_id", "sessionid", "sessionID")
	msgIDCol := sqliteFindColumn(msgCols, "id")
	msgOrderCol := sqliteFindColumn(msgCols, "time_created", "created_at", "timeCreated", "createdAt", "seq", "idx", "id")
	if msgDataCol == "" || msgSessionCol == "" {
		return nil, nil
	}

	orderBy := ""
	if msgOrderCol != "" {
		orderBy = fmt.Sprintf(" ORDER BY %s", msgOrderCol)
	}

	// Get messages for this session as a JSON array
	var msgQuery string
	if msgIDCol != "" {
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s'%s;`,
			msgDataCol, msgIDCol, msgTable, msgSessionCol, sqliteEscape(sessionID), orderBy)
	} else {
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(%s) FROM %s WHERE %s='%s'%s;`,
			msgDataCol, msgTable, msgSessionCol, sqliteEscape(sessionID), orderBy)
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
