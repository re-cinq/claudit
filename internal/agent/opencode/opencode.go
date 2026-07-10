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
// OpenCode's SQLite table/column names have shifted across releases (e.g. project_id
// vs projectID, time_updated vs updated_at), so instead of hardcoding a schema this
// discovers the actual table and column names at runtime via sqlite_master/PRAGMA
// table_info and adapts the queries accordingly.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findOpenCodeDB(dataDir)
	if dbPath == "" {
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

	sessionTable := findTable(tables, "session")
	messageTable := findTable(tables, "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols, err := sqliteColumnNames(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := findColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := findColumn(sessionCols, "project_id", "projectid", "project", "directory")
	updatedCol := findColumn(sessionCols, "time_updated", "updated_at", "updated", "time_created", "created_at")

	sessionQuery := fmt.Sprintf("SELECT * FROM %s", quoteIdent(sessionTable))
	limit := 1
	if projectCol != "" {
		sessionQuery += fmt.Sprintf(" WHERE %s = %s", quoteIdent(projectCol), sqlQuote(projectID))
	} else {
		// No recognizable project column — fetch a handful of the most recent
		// sessions and pick one that references this project by best effort.
		limit = 20
	}
	if updatedCol != "" {
		sessionQuery += fmt.Sprintf(" ORDER BY %s DESC", quoteIdent(updatedCol))
	}
	sessionQuery += fmt.Sprintf(" LIMIT %d;", limit)

	sessionData, err := sqliteQueryJSON(dbPath, sessionQuery)
	if err != nil {
		return nil, nil
	}
	var sessionRows []map[string]json.RawMessage
	if err := json.Unmarshal(sessionData, &sessionRows); err != nil || len(sessionRows) == 0 {
		return nil, nil
	}

	row := sessionRows[0]
	if projectCol == "" {
		if matched := findSessionRowForProject(sessionRows, projectID, projectPath); matched != nil {
			row = matched
		}
	}

	var sessionID string
	if raw, ok := row[idCol]; ok {
		_ = json.Unmarshal(raw, &sessionID)
	}
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout). If we can't find or
	// parse a timestamp, proceed anyway — better to try than skip.
	if updatedCol != "" {
		if raw, ok := row[updatedCol]; ok && !isRecentTimestamp(raw) {
			return nil, nil
		}
	}

	messageCols, err := sqliteColumnNames(dbPath, messageTable)
	if err != nil || len(messageCols) == 0 {
		return nil, nil
	}
	sessionFK := findColumn(messageCols, "session_id", "sessionid", "session")
	timeCreatedCol := findColumn(messageCols, "time_created", "created_at", "created")
	if sessionFK == "" {
		return nil, nil
	}

	msgQuery := fmt.Sprintf("SELECT * FROM %s WHERE %s = %s",
		quoteIdent(messageTable), quoteIdent(sessionFK), sqlQuote(sessionID))
	if timeCreatedCol != "" {
		msgQuery += fmt.Sprintf(" ORDER BY %s", quoteIdent(timeCreatedCol))
	}
	msgQuery += ";"

	transcriptData, err := sqliteQueryJSON(dbPath, msgQuery)
	if err != nil {
		return nil, nil
	}
	// sqlite3 -json returns "[]" for zero matching rows.
	if len(transcriptData) == 0 || string(transcriptData) == "[]" {
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

// findOpenCodeDB locates OpenCode's SQLite database. Newer releases have moved
// the file around, so if it isn't at the well-known path, fall back to
// searching the data directory for any *.db file.
func findOpenCodeDB(dataDir string) string {
	primary := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(primary); err == nil {
		return primary
	}

	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".db") {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// sqliteQueryJSON runs a query against dbPath and returns the raw JSON array output.
func sqliteQueryJSON(dbPath, query string) ([]byte, error) {
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(string(out))), nil
}

// sqliteTableNames returns the names of all tables in the database.
func sqliteTableNames(dbPath string) ([]string, error) {
	data, err := sqliteQueryJSON(dbPath, `SELECT name FROM sqlite_master WHERE type='table';`)
	if err != nil || len(data) == 0 {
		return nil, err
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Name)
	}
	return names, nil
}

// sqliteColumnNames returns the column names of a table via PRAGMA table_info.
func sqliteColumnNames(dbPath, table string) ([]string, error) {
	data, err := sqliteQueryJSON(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(table)))
	if err != nil || len(data) == 0 {
		return nil, err
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Name)
	}
	return names, nil
}

// findTable returns the table whose name matches hint (case-insensitive exact
// match preferred, falling back to a substring match), or "" if none match.
func findTable(tables []string, hint string) string {
	hint = strings.ToLower(hint)
	for _, t := range tables {
		if strings.ToLower(t) == hint {
			return t
		}
	}
	for _, t := range tables {
		if strings.Contains(strings.ToLower(t), hint) {
			return t
		}
	}
	return ""
}

// findColumn returns the first column matching any candidate name, trying
// exact (case-insensitive) matches across all candidates before falling back
// to substring matches.
func findColumn(cols []string, candidates ...string) string {
	for _, cand := range candidates {
		cand = strings.ToLower(cand)
		for _, c := range cols {
			if strings.ToLower(c) == cand {
				return c
			}
		}
	}
	for _, cand := range candidates {
		cand = strings.ToLower(cand)
		for _, c := range cols {
			if strings.Contains(strings.ToLower(c), cand) {
				return c
			}
		}
	}
	return ""
}

// findSessionRowForProject does a best-effort scan for a session row that
// references projectID or projectPath, used when no project-identifying
// column could be found on the session table.
func findSessionRowForProject(rows []map[string]json.RawMessage, projectID, projectPath string) map[string]json.RawMessage {
	for _, row := range rows {
		for _, raw := range row {
			s := string(raw)
			if (projectID != "" && strings.Contains(s, projectID)) || (projectPath != "" && strings.Contains(s, projectPath)) {
				return row
			}
		}
	}
	return nil
}

// isRecentTimestamp reports whether raw (a JSON string or numeric timestamp)
// falls within agent.RecentSessionTimeout. Unparseable values return true so
// discovery proceeds rather than silently giving up.
func isRecentTimestamp(raw json.RawMessage) bool {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil && asString != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, asString); err == nil {
				return time.Since(t) <= agent.RecentSessionTimeout
			}
		}
		return true
	}

	var asNumber float64
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		sec := int64(asNumber)
		if sec > 1_000_000_000_000 {
			// Milliseconds since epoch.
			sec /= 1000
		}
		return time.Since(time.Unix(sec, 0)) <= agent.RecentSessionTimeout
	}

	return true
}

// quoteIdent quotes a SQL identifier (table/column name).
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqlQuote quotes a SQL string literal.
func sqlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
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
