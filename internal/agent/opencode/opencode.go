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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session. OpenCode's SQLite schema and database location have changed across
// releases (table/column names and even the db path are not stable across
// major/minor versions), so this introspects the schema at runtime instead of
// assuming fixed names.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	dbPath := findOpenCodeDB(dataDir, projectID)
	if dbPath == "" {
		return nil, nil
	}

	tables, err := sqliteTables(dbPath)
	if err != nil || len(tables) == 0 {
		return nil, nil
	}

	sessionTable := pickName(tables, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}

	sessionCols, err := sqliteColumns(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	// Only trust an exact "id" match — a substring match here would also hit
	// columns like "project_id" or "session_id".
	idCol := pickExact(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := pickName(sessionCols, "project_id", "projectid", "project", "directory", "cwd", "worktree", "path")
	timeCol := pickName(sessionCols, "time_updated", "updated_at", "updatedat", "updated", "time_created", "created_at", "createdat", "created")

	orderClause := "rowid DESC"
	if timeCol != "" {
		orderClause = quoteIdent(timeCol) + " DESC"
	}

	var sessionQuery string
	if projectCol != "" {
		filterValue := projectID
		lowerProjectCol := strings.ToLower(projectCol)
		if strings.Contains(lowerProjectCol, "dir") || strings.Contains(lowerProjectCol, "cwd") ||
			strings.Contains(lowerProjectCol, "path") || strings.Contains(lowerProjectCol, "worktree") {
			filterValue = projectPath
		}
		sessionQuery = fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s LIMIT 1;`,
			quoteIdent(idCol), quoteIdent(sessionTable), quoteIdent(projectCol), sqlEscape(filterValue), orderClause)
	} else {
		sessionQuery = fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s LIMIT 1;`,
			quoteIdent(idCol), quoteIdent(sessionTable), orderClause)
	}

	sessionID, err := sqliteExec(dbPath, sessionQuery)
	if err != nil || sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout). If we can't run the
	// query or parse the time, proceed anyway — better to try than to skip.
	if timeCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`,
			quoteIdent(timeCol), quoteIdent(sessionTable), quoteIdent(idCol), sqlEscape(sessionID))
		if timeOutput, err := sqliteExec(dbPath, timeQuery); err == nil && timeOutput != "" {
			if !isWithinRecentTimeout(timeOutput) {
				return nil, nil
			}
		}
	}

	transcriptData := fetchOpenCodeMessages(dbPath, tables, sessionID)

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// findOpenCodeDB locates the OpenCode SQLite database. Newer OpenCode
// releases have moved this around (global vs. per-project), so try the
// known layouts in order of likelihood.
func findOpenCodeDB(dataDir, projectID string) string {
	candidates := []string{
		filepath.Join(dataDir, "opencode.db"),
		filepath.Join(dataDir, "storage", "opencode.db"),
		filepath.Join(dataDir, "project", projectID, "opencode.db"),
		filepath.Join(dataDir, "storage", "session", projectID, "opencode.db"),
		filepath.Join(dataDir, projectID, "opencode.db"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// fetchOpenCodeMessages retrieves the messages for a session as a JSON array,
// tolerating unknown message table/column naming by falling back to a
// generic row dump. Always returns valid (possibly empty) JSON array bytes,
// so a schema mismatch here degrades to an empty transcript rather than
// blocking note creation entirely.
func fetchOpenCodeMessages(dbPath string, tables []string, sessionID string) []byte {
	empty := []byte("[]")

	messageTable := pickName(tables, "message", "messages")
	if messageTable == "" {
		return empty
	}

	msgCols, err := sqliteColumns(dbPath, messageTable)
	if err != nil || len(msgCols) == 0 {
		return empty
	}

	sessionRefCol := pickName(msgCols, "session_id", "sessionid", "session")
	if sessionRefCol == "" {
		return empty
	}

	orderCol := pickName(msgCols, "time_created", "created_at", "createdat", "created", "time_updated", "updated_at", "updated")
	orderClause := "rowid"
	if orderCol != "" {
		orderClause = quoteIdent(orderCol)
	}

	// Legacy shape: a single JSON blob column holding the message body,
	// keyed by a separate "id" column.
	dataCol := pickName(msgCols, "data", "content", "body", "parts", "json")
	idCol := pickExact(msgCols, "id")
	if dataCol != "" && idCol != "" {
		msgQuery := fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s' ORDER BY %s;`,
			quoteIdent(dataCol), quoteIdent(idCol), quoteIdent(messageTable), quoteIdent(sessionRefCol), sqlEscape(sessionID), orderClause,
		)
		if out, err := sqliteExec(dbPath, msgQuery); err == nil && out != "" && out != "[null]" && out != "[]" {
			return []byte(out)
		}
	}

	// Fall back to a generic row dump: every column becomes a JSON field,
	// regardless of whether the message body lives in one blob column or
	// several typed columns.
	msgQuery := fmt.Sprintf(`SELECT * FROM %s WHERE %s='%s' ORDER BY %s;`,
		quoteIdent(messageTable), quoteIdent(sessionRefCol), sqlEscape(sessionID), orderClause)
	cmd := exec.Command("sqlite3", "-json", dbPath, msgQuery)
	out, err := cmd.Output()
	if err != nil {
		return empty
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "[null]" {
		return empty
	}
	return []byte(trimmed)
}

// isWithinRecentTimeout reports whether a raw SQLite timestamp value (string
// or integer epoch, in seconds/millis/micros/nanos) is within
// RecentSessionTimeout. Values that can't be parsed are treated as recent so
// discovery isn't blocked by an unrecognized time format.
func isWithinRecentTimeout(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}

	formats := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
	for _, f := range formats {
		if t, err := time.Parse(f, raw); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		var t time.Time
		switch {
		case n > 1e17:
			t = time.Unix(0, n) // nanoseconds
		case n > 1e14:
			t = time.UnixMicro(n)
		case n > 1e11:
			t = time.UnixMilli(n)
		default:
			t = time.Unix(n, 0)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	return true
}

// sqliteExec runs a sqlite3 query in list mode and returns trimmed stdout.
func sqliteExec(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sqliteTables returns all table names in the database.
func sqliteTables(dbPath string) ([]string, error) {
	out, err := sqliteExec(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// sqliteColumns returns the column names for a table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) ([]string, error) {
	out, err := sqliteExec(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(table)))
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// list-mode output: cid|name|type|notnull|dflt_value|pk
		parts := strings.Split(line, "|")
		if len(parts) > 1 {
			cols = append(cols, parts[1])
		}
	}
	return cols, nil
}

// pickExact finds the first name that matches a candidate exactly
// (case-insensitive). Used for lookups like "id" where a substring match
// would false-positive against columns like "project_id" or "session_id".
func pickExact(names []string, candidates ...string) string {
	byLower := make(map[string]string, len(names))
	for _, n := range names {
		byLower[strings.ToLower(n)] = n
	}
	for _, c := range candidates {
		if n, ok := byLower[c]; ok {
			return n
		}
	}
	return ""
}

// pickName finds the first name matching a candidate, preferring an exact
// (case-insensitive) match and falling back to a substring match.
func pickName(names []string, candidates ...string) string {
	if exact := pickExact(names, candidates...); exact != "" {
		return exact
	}
	for _, n := range names {
		nl := strings.ToLower(n)
		for _, c := range candidates {
			if strings.Contains(nl, c) {
				return n
			}
		}
	}
	return ""
}

// quoteIdent quotes a SQLite identifier to guard against reserved words.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqlEscape escapes a value for embedding in a single-quoted SQLite string literal.
func sqlEscape(value string) string {
	return strings.ReplaceAll(value, "'", "''")
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
