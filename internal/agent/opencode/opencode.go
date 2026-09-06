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
// OpenCode's project ID algorithm may not match shiftlog's git-root-commit
// based computation across CLI versions, so if the expected project
// directory doesn't yield a session, every project directory under
// storage/session is scanned for the most recently modified session file.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	if sessionDir, err := GetSessionDir(projectPath); err == nil {
		if info := latestSessionFileIn(sessionDir); info != nil {
			info.ProjectPath = projectPath
			return info, nil
		}
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

	var best *agent.SessionInfo
	for _, pd := range projectDirs {
		if !pd.IsDir() {
			continue
		}
		info := latestSessionFileIn(filepath.Join(sessionsRoot, pd.Name()))
		if info == nil {
			continue
		}
		if best == nil || info.StartedAt > best.StartedAt {
			best = info
		}
	}

	if best != nil {
		best.ProjectPath = projectPath
	}
	return best, nil
}

// latestSessionFileIn returns session info for the most recently modified
// *.json session file in dir, within the recent-session timeout window, or
// nil if none qualify.
func latestSessionFileIn(dir string) *agent.SessionInfo {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil
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

	if bestSessionID == "" {
		return nil
	}

	// The transcript path for OpenCode is the message directory
	msgDir, _ := GetMessageDir(bestSessionID)

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
	}
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session. OpenCode's table/column names have changed across versions, so
// rather than hardcoding a schema, this introspects sqlite_master/PRAGMA
// table_info at runtime and reads rows via `-json` output, so data survives
// schema drift instead of silently returning nothing.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	tables, err := sqliteTables(dbPath)
	if err != nil || len(tables) == 0 {
		return nil, nil
	}

	sessionTable := findTable(tables, "session")
	messageTable := findTable(tables, "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols, err := sqliteColumns(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := primaryKeyColumn(sessionCols)
	if idCol == "" {
		return nil, nil
	}
	updatedCol := firstNonEmpty(
		findColumn(sessionCols, "updated"),
		findColumn(sessionCols, "modified"),
		findColumn(sessionCols, "time"),
	)
	projectCol := findColumn(sessionCols, "project")

	orderCol := updatedCol
	if orderCol == "" {
		orderCol = "rowid"
	}

	// Find the most recent session for this project.
	var sessionRow map[string]interface{}
	if projectCol != "" {
		query := fmt.Sprintf(`SELECT * FROM %q WHERE %q = %s ORDER BY %q DESC LIMIT 1;`,
			sessionTable, projectCol, sqliteQuote(projectID), orderCol)
		if rows, err := sqliteQueryJSON(dbPath, query); err == nil && len(rows) > 0 {
			sessionRow = rows[0]
		}
	}
	if sessionRow == nil {
		// Fall back to the most recent session regardless of project: shiftlog's
		// project ID computation may not match OpenCode's own algorithm.
		query := fmt.Sprintf(`SELECT * FROM %q ORDER BY %q DESC LIMIT 1;`, sessionTable, orderCol)
		rows, err := sqliteQueryJSON(dbPath, query)
		if err != nil || len(rows) == 0 {
			return nil, nil
		}
		sessionRow = rows[0]
	}

	sessionID := stringValue(sessionRow[idCol])
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout). If we can't parse
	// the time, proceed anyway — better to try than skip.
	if updatedCol != "" {
		if t, ok := parseSQLiteTime(sessionRow[updatedCol]); ok && time.Since(t) > agent.RecentSessionTimeout {
			return nil, nil
		}
	}

	msgCols, err := sqliteColumns(dbPath, messageTable)
	if err != nil || len(msgCols) == 0 {
		return nil, nil
	}
	msgSessionCol := findColumn(msgCols, "session")
	if msgSessionCol == "" {
		return nil, nil
	}
	msgOrderCol := firstNonEmpty(
		findColumn(msgCols, "created"),
		findColumn(msgCols, "time"),
		"rowid",
	)

	// Get messages for this session as a JSON array
	msgQuery := fmt.Sprintf(`SELECT * FROM %q WHERE %q = %s ORDER BY %q;`,
		messageTable, msgSessionCol, sqliteQuote(sessionID), msgOrderCol)
	messages, err := sqliteQueryJSON(dbPath, msgQuery)
	if err != nil || len(messages) == 0 {
		return nil, nil
	}

	transcriptData, err := json.Marshal(messages)
	if err != nil {
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

// sqliteColumn describes a single column reported by PRAGMA table_info.
type sqliteColumn struct {
	Name string
	PK   bool
}

// sqliteTables lists user tables in the database.
func sqliteTables(dbPath string) ([]string, error) {
	rows, err := sqliteQueryJSON(dbPath, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%';`)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range rows {
		if n, ok := r["name"].(string); ok {
			names = append(names, n)
		}
	}
	return names, nil
}

// sqliteColumns returns the columns of table, including primary-key status.
func sqliteColumns(dbPath, table string) ([]sqliteColumn, error) {
	rows, err := sqliteQueryJSON(dbPath, fmt.Sprintf(`PRAGMA table_info(%q);`, table))
	if err != nil {
		return nil, err
	}
	var cols []sqliteColumn
	for _, r := range rows {
		name, _ := r["name"].(string)
		if name == "" {
			continue
		}
		pk := false
		if pkVal, ok := r["pk"].(float64); ok && pkVal > 0 {
			pk = true
		}
		cols = append(cols, sqliteColumn{Name: name, PK: pk})
	}
	return cols, nil
}

// sqliteQueryJSON runs query against dbPath and decodes the `-json` output.
// Modern sqlite3 CLIs report rows using their actual column names, so this
// keeps working even when OpenCode renames or adds columns.
func sqliteQueryJSON(dbPath, query string) ([]map[string]interface{}, error) {
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, nil
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// findTable returns the table whose name best matches keyword, preferring an
// exact (singular or plural) match before falling back to a substring match.
func findTable(tables []string, keyword string) string {
	for _, want := range []string{keyword, keyword + "s"} {
		for _, t := range tables {
			if strings.EqualFold(t, want) {
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

// findColumn returns the column whose name best matches keyword, preferring
// an exact match before falling back to a substring match.
func findColumn(cols []sqliteColumn, keyword string) string {
	for _, c := range cols {
		if strings.EqualFold(c.Name, keyword) {
			return c.Name
		}
	}
	for _, c := range cols {
		if strings.Contains(strings.ToLower(c.Name), keyword) {
			return c.Name
		}
	}
	return ""
}

// primaryKeyColumn returns the declared primary key column, falling back to
// a column named (or containing) "id".
func primaryKeyColumn(cols []sqliteColumn) string {
	for _, c := range cols {
		if c.PK {
			return c.Name
		}
	}
	return findColumn(cols, "id")
}

// firstNonEmpty returns the first non-empty string argument.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// stringValue converts a decoded JSON scalar to a string.
func stringValue(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return ""
	}
}

// sqliteQuote quotes a string as a SQLite string literal.
func sqliteQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// parseSQLiteTime parses a timestamp value that may be an ISO-8601 string or
// a Unix timestamp in seconds or milliseconds.
func parseSQLiteTime(v interface{}) (time.Time, bool) {
	switch x := v.(type) {
	case string:
		for _, layout := range []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05.000Z",
			"2006-01-02 15:04:05",
		} {
			if t, err := time.Parse(layout, x); err == nil {
				return t, true
			}
		}
	case float64:
		i := int64(x)
		if i <= 0 {
			return time.Time{}, false
		}
		if i > 1e12 {
			return time.UnixMilli(i), true
		}
		return time.Unix(i, 0), true
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
