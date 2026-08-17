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
//
// OpenCode's project-ID calculation is an internal implementation detail that
// has changed across versions, so a miss under our computed project directory
// doesn't necessarily mean there's no session — it may mean our project ID no
// longer matches OpenCode's. When the project-scoped lookup comes up empty, we
// fall back to scanning across all project directories for the most recently
// modified session within the recency window.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return nil, nil
	}

	if bestSessionID, bestModTime := mostRecentSessionFile(sessionDir); bestSessionID != "" {
		msgDir, _ := GetMessageDir(bestSessionID)
		return &agent.SessionInfo{
			SessionID:      bestSessionID,
			TranscriptPath: msgDir,
			StartedAt:      bestModTime.Format(time.RFC3339),
			ProjectPath:    projectPath,
		}, nil
	}

	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionsRoot := filepath.Join(dataDir, "storage", "session")
	projectDirs, err := os.ReadDir(sessionsRoot)
	if err != nil {
		return nil, nil
	}

	var bestSessionID string
	var bestModTime time.Time
	for _, pd := range projectDirs {
		if !pd.IsDir() {
			continue
		}
		id, modTime := mostRecentSessionFile(filepath.Join(sessionsRoot, pd.Name()))
		if id != "" && (bestSessionID == "" || modTime.After(bestModTime)) {
			bestSessionID, bestModTime = id, modTime
		}
	}

	if bestSessionID == "" {
		return nil, nil
	}

	msgDir, _ := GetMessageDir(bestSessionID)
	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// mostRecentSessionFile returns the session ID and mod time of the most
// recently modified session JSON file directly inside dir, considering only
// files modified within agent.RecentSessionTimeout. Returns ("", zero time)
// if dir doesn't exist or has no sufficiently recent session file.
func mostRecentSessionFile(dir string) (string, time.Time) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}
	}

	now := time.Now()
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
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			continue
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = strings.TrimSuffix(entry.Name(), ".json")
			bestModTime = modTime
		}
	}

	return bestSessionID, bestModTime
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
//
// OpenCode's SQLite table and column names are an internal implementation
// detail that has changed across releases, so rather than hardcoding a schema
// we introspect it at query time via sqlite_master/pragma_table_info and adapt.
// If the project-scoping column can't be matched (renamed, removed, or using a
// value we don't know how to compute), we fall back to the most recently
// updated session overall rather than reporting "no session found".
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := findSQLiteTable(dbPath, "session")
	messageTable := findSQLiteTable(dbPath, "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols, err := sqliteTableColumns(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project", "directory", "cwd", "path")
	timeCol := pickColumn(sessionCols, "time_updated", "updated_at", "updated", "mtime", "time_created", "created_at", "time")

	sessionID := findMostRecentSession(dbPath, sessionTable, idCol, projectCol, timeCol, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout)
	if timeCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`,
			timeCol, sessionTable, idCol, escapeSQLiteString(sessionID))
		cmd := exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			if t, ok := parseSQLiteTime(strings.TrimSpace(string(timeOutput))); ok {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
			// If we can't parse the time, proceed anyway — better to try than skip
		}
	}

	msgCols, err := sqliteTableColumns(dbPath, messageTable)
	if err != nil || len(msgCols) == 0 {
		return nil, nil
	}
	msgIDCol := pickColumn(msgCols, "id")
	sessionRefCol := pickColumn(msgCols, "session_id", "sessionid", "session")
	dataCol := pickColumn(msgCols, "data", "content", "body")
	msgTimeCol := pickColumn(msgCols, "time_created", "created_at", "created", "time")
	if msgIDCol == "" || sessionRefCol == "" || dataCol == "" {
		return nil, nil
	}

	// Get messages for this session as a JSON array
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s'`,
		dataCol, msgIDCol, messageTable, sessionRefCol, escapeSQLiteString(sessionID),
	)
	if msgTimeCol != "" {
		msgQuery += fmt.Sprintf(" ORDER BY %s", msgTimeCol)
	}
	msgQuery += ";"

	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
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

// findMostRecentSession returns the id of the most recently updated session,
// preferring one that matches the current project via whichever project-scoping
// column the schema actually has. If there's no such column, or no row matches
// it, it falls back to the single most recent session in the table.
func findMostRecentSession(dbPath, table, idCol, projectCol, timeCol, projectID, projectPath string) string {
	if projectCol != "" {
		matchValue := projectID
		lowerCol := strings.ToLower(projectCol)
		if strings.Contains(lowerCol, "dir") || strings.Contains(lowerCol, "path") || strings.Contains(lowerCol, "cwd") {
			matchValue = projectPath
		}

		query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s'`, idCol, table, projectCol, escapeSQLiteString(matchValue))
		if timeCol != "" {
			query += fmt.Sprintf(" ORDER BY %s DESC", timeCol)
		}
		query += " LIMIT 1;"

		if out, err := exec.Command("sqlite3", dbPath, query).Output(); err == nil {
			if id := strings.TrimSpace(string(out)); id != "" {
				return id
			}
		}
	}

	// Fall back to the most recent session overall — the project-scoping column
	// may have been renamed, removed, or use a value we don't know how to compute.
	query := fmt.Sprintf(`SELECT %s FROM %s`, idCol, table)
	if timeCol != "" {
		query += fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}
	query += " LIMIT 1;"

	out, err := exec.Command("sqlite3", dbPath, query).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// findSQLiteTable finds a table name matching hint (e.g. "session"), tolerating
// naming differences (e.g. pluralization) across OpenCode schema versions.
func findSQLiteTable(dbPath, hint string) string {
	out, err := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';").Output()
	if err != nil {
		return ""
	}

	var tables []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			tables = append(tables, name)
		}
	}

	hintLower := strings.ToLower(hint)
	for _, name := range tables {
		if strings.ToLower(name) == hintLower {
			return name
		}
	}
	for _, name := range tables {
		lower := strings.ToLower(name)
		if lower == hintLower+"s" || lower == hintLower+"es" {
			return name
		}
	}
	for _, name := range tables {
		if strings.Contains(strings.ToLower(name), hintLower) {
			return name
		}
	}
	return ""
}

// sqliteTableColumns returns the column names of a SQLite table.
func sqliteTableColumns(dbPath, table string) ([]string, error) {
	query := fmt.Sprintf("SELECT name FROM pragma_table_info('%s');", escapeSQLiteString(table))
	out, err := exec.Command("sqlite3", dbPath, query).Output()
	if err != nil {
		return nil, err
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			cols = append(cols, name)
		}
	}
	return cols, nil
}

// pickColumn returns the first candidate (case-insensitive) that exists in cols.
func pickColumn(cols []string, candidates ...string) string {
	byLower := make(map[string]string, len(cols))
	for _, c := range cols {
		byLower[strings.ToLower(c)] = c
	}
	for _, cand := range candidates {
		if actual, ok := byLower[cand]; ok {
			return actual
		}
	}
	return ""
}

// escapeSQLiteString escapes a value for safe interpolation into a
// single-quoted SQLite string literal.
func escapeSQLiteString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// parseSQLiteTime parses a timestamp value from OpenCode's SQLite storage,
// which has used ISO-8601 strings and, in some versions, epoch milliseconds
// or seconds.
func parseSQLiteTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n), true
		}
		if n > 1e9 {
			return time.Unix(n, 0), true
		}
	}
	return time.Time{}, false
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
