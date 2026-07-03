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

func (a *Agent) Name() agent.Name    { return agent.OpenCode }
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
		"bash":     true,
		"shell":    true,
		"terminal": true,
		"execute":  true,
		"run":      true,
		"command":  true,
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
// OpenCode's internal storage backend has changed shape across releases (flat
// per-project files, then a single global opencode.db, then per-project database
// files with renamed tables/columns). Rather than hardcoding one schema, the
// table/column names and database location are discovered at runtime so this
// keeps working as the upstream schema is renamed or moved.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	dbPath := findOpenCodeDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	tables, err := sqliteTables(dbPath)
	if err != nil || len(tables) == 0 {
		return nil, nil
	}

	sessionTable := findTableName(tables, "session")
	messageTable := findTableName(tables, "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols, err := sqliteColumns(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := findColumnName(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := findColumnName(sessionCols, "time_updated", "updated", "modified", "time")
	projCol := findColumnName(sessionCols, "project_id", "project", "directory", "worktree", "cwd", "path")

	sessionID := findSessionID(dbPath, sessionTable, idCol, timeCol, projCol, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	if timeCol != "" && !isSessionRecent(dbPath, sessionTable, idCol, timeCol, sessionID) {
		return nil, nil
	}

	messageCols, err := sqliteColumns(dbPath, messageTable)
	if err != nil || len(messageCols) == 0 {
		return nil, nil
	}

	msgIDCol := findColumnName(messageCols, "id")
	msgSessionCol := findColumnName(messageCols, "session_id", "sessionid", "session")
	msgTimeCol := findColumnName(messageCols, "time_created", "created", "time")
	msgDataCol := findColumnName(messageCols, "data", "content", "body", "json", "payload", "value")

	if msgSessionCol == "" || msgDataCol == "" {
		return nil, nil
	}

	var msgQuery string
	if msgIDCol != "" {
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s'`,
			quoteIdent(msgDataCol), quoteIdent(msgIDCol), quoteIdent(messageTable), quoteIdent(msgSessionCol), escapeSQL(sessionID),
		)
	} else {
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(%s) FROM %s WHERE %s='%s'`,
			quoteIdent(msgDataCol), quoteIdent(messageTable), quoteIdent(msgSessionCol), escapeSQL(sessionID),
		)
	}
	if msgTimeCol != "" {
		msgQuery += fmt.Sprintf(" ORDER BY %s", quoteIdent(msgTimeCol))
	}
	msgQuery += ";"

	msgOutput, err := runSQLite(dbPath, msgQuery)
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(msgOutput))
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

// findOpenCodeDB locates OpenCode's SQLite database. It first checks the
// well-known location (dataDir/opencode.db); if that's absent, it walks
// dataDir looking for any *.db/*.sqlite file, since newer OpenCode releases
// have moved the database into per-project subdirectories.
func findOpenCodeDB(dataDir string) string {
	defaultPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(defaultPath); err == nil {
		return defaultPath
	}

	var found string
	stop := fmt.Errorf("stop walk")
	_ = filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		name := strings.ToLower(info.Name())
		if strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".sqlite") || strings.HasSuffix(name, ".sqlite3") {
			found = path
			return stop
		}
		return nil
	})
	return found
}

// findSessionID finds the most recent session ID for a project, trying
// several candidate values for the project column (since OpenCode has used
// both a git-root-commit-hash and a raw directory path as the project
// identifier across releases) and falling back to the most recent session
// overall if no project column can be identified.
func findSessionID(dbPath, table, idCol, timeCol, projCol, projectID, projectPath string) string {
	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", quoteIdent(timeCol))
	}

	if projCol == "" {
		query := fmt.Sprintf(`SELECT %s FROM %s%s LIMIT 1;`, quoteIdent(idCol), quoteIdent(table), orderClause)
		out, err := runSQLite(dbPath, query)
		if err == nil {
			if id := strings.TrimSpace(out); id != "" {
				return id
			}
		}
		return ""
	}

	for _, val := range []string{projectID, projectPath} {
		if val == "" {
			continue
		}
		query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;`,
			quoteIdent(idCol), quoteIdent(table), quoteIdent(projCol), escapeSQL(val), orderClause)
		out, err := runSQLite(dbPath, query)
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(out); id != "" {
			return id
		}
	}

	return ""
}

// isSessionRecent checks whether a session's timestamp is within
// agent.RecentSessionTimeout. If the timestamp can't be read or parsed, it
// returns true (better to try storing than to silently skip).
func isSessionRecent(dbPath, table, idCol, timeCol, sessionID string) bool {
	query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`,
		quoteIdent(timeCol), quoteIdent(table), quoteIdent(idCol), escapeSQL(sessionID))
	out, err := runSQLite(dbPath, query)
	if err != nil {
		return true
	}

	timeStr := strings.TrimSpace(out)
	if timeStr == "" {
		return true
	}

	if t, ok := parseOpenCodeTime(timeStr); ok {
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	// Unknown timestamp format — proceed anyway rather than skip a real session.
	return true
}

// parseOpenCodeTime parses a timestamp value from OpenCode's database, which
// has been observed in RFC3339(Nano), SQLite's default datetime format, and
// (in newer schemas) Unix epoch seconds or milliseconds.
func parseOpenCodeTime(s string) (time.Time, bool) {
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, true
		}
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		// Heuristic: values beyond ~year-2286-in-seconds are milliseconds.
		if n > 1e12 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}

	return time.Time{}, false
}

// sqliteTables returns the list of table names in the database.
func sqliteTables(dbPath string) ([]string, error) {
	out, err := runSQLite(dbPath, ".tables")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// sqliteColumns returns the column names for a table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) ([]string, error) {
	query := fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(table))
	out, err := runSQLiteSep(dbPath, query, "|")
	if err != nil {
		return nil, err
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// findTableName finds a table matching keyword, preferring exact/plural
// matches over a loose substring match.
func findTableName(tables []string, keyword string) string {
	for _, candidate := range []string{keyword, keyword + "s"} {
		for _, t := range tables {
			if strings.EqualFold(t, candidate) {
				return t
			}
		}
	}
	for _, t := range tables {
		if strings.Contains(strings.ToLower(t), keyword) {
			return t
		}
	}
	return ""
}

// findColumnName finds a column matching one of the given keywords, in order
// of preference, trying exact matches before substring matches.
func findColumnName(cols []string, keywords ...string) string {
	for _, kw := range keywords {
		for _, c := range cols {
			if strings.EqualFold(c, kw) {
				return c
			}
		}
	}
	for _, kw := range keywords {
		for _, c := range cols {
			if strings.Contains(strings.ToLower(c), kw) {
				return c
			}
		}
	}
	return ""
}

// runSQLite runs a single sqlite3 query against dbPath and returns its output.
func runSQLite(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	return string(out), err
}

// runSQLiteSep runs a single sqlite3 query with a custom output separator.
func runSQLiteSep(dbPath, query, sep string) (string, error) {
	cmd := exec.Command("sqlite3", "-separator", sep, dbPath, query)
	out, err := cmd.Output()
	return string(out), err
}

// quoteIdent quotes a SQL identifier (table/column name) discovered via
// introspection so it round-trips safely even if it contains unusual characters.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// escapeSQL escapes single quotes in a value used inside a SQL string literal.
func escapeSQL(s string) string {
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
