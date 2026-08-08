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
// Newer OpenCode releases have been observed namespacing storage under a
// "project/<projectID>" directory rather than "storage/session/<projectID>"
// directly off the data dir, so both layouts are tried.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}
	projectID := GetProjectID(projectPath)

	legacySessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return nil, nil
	}

	candidates := []struct {
		sessionDir string
		messageDir string
	}{
		{legacySessionDir, filepath.Join(dataDir, "storage", "message")},
		{
			filepath.Join(dataDir, "project", projectID, "storage", "session"),
			filepath.Join(dataDir, "project", projectID, "storage", "message"),
		},
	}

	for _, c := range candidates {
		if session := mostRecentSessionFile(c.sessionDir, c.messageDir, projectPath); session != nil {
			return session, nil
		}
	}

	return nil, nil
}

// mostRecentSessionFile scans sessionDir for the most recently modified
// *.json file within the recent-session timeout window.
func mostRecentSessionFile(sessionDir, messageDir, projectPath string) *agent.SessionInfo {
	dirEntries, err := os.ReadDir(sessionDir)
	if err != nil {
		return nil
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
		return nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: filepath.Join(messageDir, bestSessionID),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session. Table and column names are detected at query time
// (rather than hard-coded) because OpenCode's SQLite schema has changed
// across releases (e.g. "session"/"sessions", "project_id"/"directory",
// "time_updated"/"updated_at", a single JSON blob column vs. discrete
// role/parts columns).
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := findSQLiteTable(dbPath, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}

	sessionCols := sqliteColumns(dbPath, sessionTable)
	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	updatedCol := pickColumn(sessionCols, "time_updated", "updated_at", "updatedAt", "timeUpdated")
	orderExpr := "rowid"
	if updatedCol != "" {
		orderExpr = updatedCol
	}

	// Match by project ID (root commit hash) if the schema tracks one,
	// otherwise fall back to matching on the absolute working directory.
	var sessionID string
	if projectCol := pickColumn(sessionCols, "project_id", "projectID", "projectId", "project"); projectCol != "" {
		sessionID, _ = sqliteScalar(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
			idCol, sessionTable, projectCol, projectID, orderExpr,
		))
	}
	if sessionID == "" {
		if dirCol := pickColumn(sessionCols, "directory", "cwd", "project_dir", "path"); dirCol != "" {
			sessionID, _ = sqliteScalar(dbPath, fmt.Sprintf(
				`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
				idCol, sessionTable, dirCol, projectPath, orderExpr,
			))
		}
	}
	if sessionID == "" {
		return nil, nil
	}

	// Best-effort recency check. If we can't determine (or parse) the
	// update time, proceed anyway rather than block discovery on an
	// unrecognized timestamp format.
	if updatedCol != "" {
		if timeStr, ok := sqliteScalar(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s';`, updatedCol, sessionTable, idCol, sessionID,
		)); ok {
			if t, ok := parseOpenCodeTimestamp(timeStr); ok && time.Since(t) > agent.RecentSessionTimeout {
				return nil, nil
			}
		}
	}

	transcriptData := loadSQLiteMessages(dbPath, sessionID)

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// loadSQLiteMessages reads all messages for a session as a JSON array.
// It adapts to either a schema storing the full message as a single JSON
// blob column, or one that splits fields (role, parts, ...) across
// discrete columns.
func loadSQLiteMessages(dbPath, sessionID string) []byte {
	messageTable := findSQLiteTable(dbPath, "message", "messages")
	if messageTable == "" {
		return nil
	}

	msgCols := sqliteColumns(dbPath, messageTable)
	idCol := pickColumn(msgCols, "id")
	sessCol := pickColumn(msgCols, "session_id", "sessionID", "sessionId")
	if idCol == "" || sessCol == "" {
		return nil
	}

	orderCol := pickColumn(msgCols, "time_created", "created_at", "createdAt", "timeCreated")
	orderExpr := "rowid"
	if orderCol != "" {
		orderExpr = orderCol
	}

	var selectExpr string
	if dataCol := pickColumn(msgCols, "data", "content", "json"); dataCol != "" {
		selectExpr = fmt.Sprintf(`json_patch(%s, json_object('id', %s))`, dataCol, idCol)
	} else {
		fields := []string{fmt.Sprintf("'id', %s", idCol)}
		if roleCol := pickColumn(msgCols, "role"); roleCol != "" {
			fields = append(fields, fmt.Sprintf("'role', %s", roleCol))
		}
		if partsCol := pickColumn(msgCols, "parts"); partsCol != "" {
			fields = append(fields, fmt.Sprintf("'content', %s", partsCol))
		}
		if orderCol != "" {
			fields = append(fields, fmt.Sprintf("'time', json_object('created', %s)", orderCol))
		}
		selectExpr = fmt.Sprintf(`json_object(%s)`, strings.Join(fields, ", "))
	}

	query := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM %s WHERE %s='%s' ORDER BY %s;`,
		selectExpr, messageTable, sessCol, sessionID, orderExpr,
	)

	out, ok := sqliteScalar(dbPath, query)
	if !ok || out == "" || out == "[null]" || out == "[]" {
		return nil
	}

	return []byte(out)
}

// sqliteScalar runs a sqlite3 query expected to return a single value.
func sqliteScalar(dbPath, query string) (string, bool) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	val := strings.TrimSpace(string(out))
	return val, val != ""
}

// findSQLiteTable returns the first candidate table name that exists in the
// database, or "" if none do.
func findSQLiteTable(dbPath string, candidates ...string) string {
	for _, name := range candidates {
		val, ok := sqliteScalar(dbPath, fmt.Sprintf(
			`SELECT name FROM sqlite_master WHERE type='table' AND name='%s';`, name,
		))
		if ok && val == name {
			return name
		}
	}
	return ""
}

// sqliteColumns returns the set of column names for a table.
func sqliteColumns(dbPath, table string) map[string]bool {
	cmd := exec.Command("sqlite3", "-json", dbPath, fmt.Sprintf(`PRAGMA table_info(%s);`, table))
	out, err := cmd.Output()
	cols := make(map[string]bool)
	if err != nil {
		return cols
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return cols
	}
	for _, r := range rows {
		cols[r.Name] = true
	}
	return cols
}

// pickColumn returns the first candidate present in cols, or "".
func pickColumn(cols map[string]bool, candidates ...string) string {
	for _, c := range candidates {
		if cols[c] {
			return c
		}
	}
	return ""
}

// parseOpenCodeTimestamp parses a timestamp in any of the formats OpenCode
// has used across releases: RFC3339(Nano), a millisecond-precision ISO
// string, a plain SQL datetime, or a Unix epoch (seconds or milliseconds).
func parseOpenCodeTimestamp(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		switch {
		case n > 1e12:
			return time.UnixMilli(n), true
		case n > 1e9:
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
