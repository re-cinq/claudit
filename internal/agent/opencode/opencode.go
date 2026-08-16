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
// OpenCode's on-disk schema (table and column names) has changed across releases,
// so instead of assuming fixed names this introspects sqlite_master/pragma_table_info
// at query time to find the session/message tables and their relevant columns. If a
// project-scoping column can't be matched with confidence, it falls back to the most
// recently updated session across the whole database within the recency window.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	tables, err := sqliteRows(dbPath, `SELECT name FROM sqlite_master WHERE type='table';`)
	if err != nil || len(tables) == 0 {
		return nil, nil
	}

	sessionTable := pickTable(tables, "session")
	messageTable := pickTable(tables, "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols, err := sqliteRows(dbPath, fmt.Sprintf(`SELECT name FROM pragma_table_info('%s');`, sessionTable))
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := pickColumn(sessionCols, "updated", "update", "time", "created")
	scopeCol := pickColumn(sessionCols, "project_id", "projectid", "directory", "worktree", "cwd", "path")

	orderClause := "rowid DESC"
	if timeCol != "" {
		orderClause = fmt.Sprintf("%s DESC", timeCol)
	}

	var sessionID, timeStr string

	// Try matching the project scope, if we found a column that looks like one.
	if scopeCol != "" {
		candidates := strings.Join([]string{escapeSQL(projectID), escapeSQL(projectPath)}, "','")
		cols := idCol
		if timeCol != "" {
			cols = idCol + ",IFNULL(" + timeCol + ",'')"
		}
		query := fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s IN ('%s') ORDER BY %s LIMIT 1;`,
			cols, sessionTable, scopeCol, candidates, orderClause,
		)
		if rows, err := sqliteRows(dbPath, query); err == nil && len(rows) > 0 {
			parts := strings.SplitN(rows[0], "\t", 2)
			sessionID = parts[0]
			if len(parts) > 1 {
				timeStr = parts[1]
			}
		}
	}

	// Fall back to the single most recently updated session across the database —
	// the project-scoping column (if any) may no longer match how this OpenCode
	// version keys sessions.
	if sessionID == "" {
		cols := idCol
		if timeCol != "" {
			cols = idCol + ",IFNULL(" + timeCol + ",'')"
		}
		query := fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s LIMIT 1;`, cols, sessionTable, orderClause)
		rows, err := sqliteRows(dbPath, query)
		if err != nil || len(rows) == 0 {
			return nil, nil
		}
		parts := strings.SplitN(rows[0], "\t", 2)
		sessionID = parts[0]
		if len(parts) > 1 {
			timeStr = parts[1]
		}
	}

	if sessionID == "" {
		return nil, nil
	}

	if !withinRecentWindow(timeStr) {
		return nil, nil
	}

	msgCols, err := sqliteRows(dbPath, fmt.Sprintf(`SELECT name FROM pragma_table_info('%s');`, messageTable))
	if err != nil || len(msgCols) == 0 {
		return nil, nil
	}

	msgSessionCol := pickColumn(msgCols, "session_id", "sessionid", "session")
	if msgSessionCol == "" {
		return nil, nil
	}
	msgIDCol := pickColumn(msgCols, "id")
	dataCol := pickColumn(msgCols, "data")

	var msgQuery string
	if msgIDCol != "" && dataCol != "" {
		// Whole-row JSON blob column plus an id — reconstruct entries by patching the id in.
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s' ORDER BY rowid;`,
			dataCol, msgIDCol, messageTable, msgSessionCol, escapeSQL(sessionID),
		)
	} else {
		// No single JSON blob column — assemble one from whatever columns exist.
		parts := make([]string, 0, len(msgCols))
		for _, c := range msgCols {
			parts = append(parts, fmt.Sprintf(`'%s', %s`, c, c))
		}
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(json_object(%s)) FROM %s WHERE %s='%s' ORDER BY rowid;`,
			strings.Join(parts, ", "), messageTable, msgSessionCol, escapeSQL(sessionID),
		)
	}

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

// sqliteRows runs a query against dbPath and returns each result row as a
// tab-separated string, skipping blank lines.
func sqliteRows(dbPath, query string) ([]string, error) {
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var rows []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			rows = append(rows, line)
		}
	}
	return rows, nil
}

// pickTable finds a table name matching want, preferring an exact (or plural)
// match before falling back to a substring match.
func pickTable(tables []string, want string) string {
	lowerWant := strings.ToLower(want)
	for _, t := range tables {
		lt := strings.ToLower(t)
		if lt == lowerWant || lt == lowerWant+"s" {
			return t
		}
	}
	for _, t := range tables {
		if strings.Contains(strings.ToLower(t), lowerWant) {
			return t
		}
	}
	return ""
}

// pickColumn returns the first column matching any of the preferred names,
// trying exact matches for every candidate before falling back to substrings.
func pickColumn(cols []string, preferred ...string) string {
	for _, want := range preferred {
		lowerWant := strings.ToLower(want)
		for _, c := range cols {
			if strings.ToLower(c) == lowerWant {
				return c
			}
		}
	}
	for _, want := range preferred {
		lowerWant := strings.ToLower(want)
		for _, c := range cols {
			if strings.Contains(strings.ToLower(c), lowerWant) {
				return c
			}
		}
	}
	return ""
}

// escapeSQL escapes single quotes for safe inline use in a SQL string literal.
func escapeSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// withinRecentWindow reports whether a session timestamp (RFC3339-ish string or
// a numeric epoch in seconds/milliseconds/microseconds/nanoseconds) falls within
// agent.RecentSessionTimeout. An empty or unrecognized value doesn't block
// discovery — it's better to try the session than to skip it.
func withinRecentWindow(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}

	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, value); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		var t time.Time
		switch {
		case n > 1e17: // nanoseconds
			t = time.Unix(0, n)
		case n > 1e14: // microseconds
			t = time.Unix(0, n*1e3)
		case n > 1e11: // milliseconds
			t = time.Unix(0, n*1e6)
		default: // seconds
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
```
