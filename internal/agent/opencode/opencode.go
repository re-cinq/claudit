```go
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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session belonging to this project.
//
// OpenCode's on-disk SQLite schema has changed across releases (older versions
// used singular "session"/"message" tables with a single JSON blob column;
// newer versions use plural "sessions"/"messages" tables with content split
// into a segmented "parts" column and integer epoch timestamps). Rather than
// hard-coding one schema, the table and column names are discovered at
// runtime so this keeps working across these versions.
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

	sessionTable := pickSQLiteTable(tables, "sessions", "session")
	messageTable := pickSQLiteTable(tables, "messages", "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols, err := sqliteTableColumns(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := pickSQLiteColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := pickSQLiteColumn(sessionCols, "updated_at", "time_updated", "created_at", "time_created")
	projectCol := pickSQLiteColumn(sessionCols, "directory", "cwd", "path", "worktree", "project_id", "project")

	sessionID, updatedAt := findRecentSQLiteSession(dbPath, sessionTable, idCol, timeCol, projectCol, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	if updatedAt != "" && !isRecentSQLiteTimestamp(updatedAt) {
		return nil, nil
	}

	transcriptData, err := readSQLiteMessages(dbPath, messageTable, sessionID)
	if err != nil || len(transcriptData) == 0 {
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

// sqliteTableNames returns all table names in the SQLite database.
func sqliteTableNames(dbPath string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// sqliteTableColumns returns the column names of a SQLite table via PRAGMA table_info.
func sqliteTableColumns(dbPath, table string) (map[string]bool, error) {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf(`PRAGMA table_info("%s");`, table))
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	cols := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 && fields[1] != "" {
			cols[fields[1]] = true
		}
	}
	return cols, nil
}

// pickSQLiteTable returns the first preferred table name present (exact match
// first, then substring match), or "" if none match.
func pickSQLiteTable(tables []string, preferred ...string) string {
	set := make(map[string]bool, len(tables))
	for _, t := range tables {
		set[t] = true
	}
	for _, p := range preferred {
		if set[p] {
			return p
		}
	}
	for _, t := range tables {
		for _, p := range preferred {
			if strings.Contains(t, p) {
				return t
			}
		}
	}
	return ""
}

// pickSQLiteColumn returns the first preferred column name present in cols, or "".
func pickSQLiteColumn(cols map[string]bool, preferred ...string) string {
	for _, p := range preferred {
		if cols[p] {
			return p
		}
	}
	return ""
}

// sqliteEscape escapes single quotes for use in a SQLite string literal.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// findRecentSQLiteSession finds the most recent session ID, preferring one
// scoped to the current project (matched by whatever directory/project column
// was detected) but falling back to the most recent session overall if no
// such column exists or nothing matches it. Returns the session ID and its
// raw "updated at" value (may be empty if no time column was found).
func findRecentSQLiteSession(dbPath, table, idCol, timeCol, projectCol, projectID, projectPath string) (string, string) {
	runQuery := func(where string) (string, string) {
		selectCols := fmt.Sprintf(`"%s"`, idCol)
		if timeCol != "" {
			selectCols += fmt.Sprintf(`, "%s"`, timeCol)
		}
		query := fmt.Sprintf(`SELECT %s FROM "%s"`, selectCols, table)
		if where != "" {
			query += " WHERE " + where
		}
		if timeCol != "" {
			query += fmt.Sprintf(` ORDER BY "%s" DESC`, timeCol)
		}
		query += " LIMIT 1;"

		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		output, err := cmd.Output()
		if err != nil {
			return "", ""
		}
		line := strings.TrimSpace(string(output))
		if line == "" {
			return "", ""
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) == 2 {
			return fields[0], fields[1]
		}
		return fields[0], ""
	}

	if projectCol != "" {
		for _, val := range []string{projectPath, projectID} {
			if val == "" {
				continue
			}
			where := fmt.Sprintf(`"%s"='%s'`, projectCol, sqliteEscape(val))
			if id, ts := runQuery(where); id != "" {
				return id, ts
			}
		}
	}

	return runQuery("")
}

// isRecentSQLiteTimestamp reports whether a session timestamp is within the
// recent session timeout. It accepts RFC3339-ish strings as well as Unix
// epoch timestamps in seconds or milliseconds. If the value can't be parsed,
// it's treated as recent (better to try the session than to skip it).
func isRecentSQLiteTimestamp(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}

	if epoch, err := strconv.ParseInt(raw, 10, 64); err == nil {
		t := time.Unix(epoch, 0)
		if epoch > 1e12 { // looks like milliseconds, not seconds
			t = time.UnixMilli(epoch)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, raw); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	return true
}

// flexString unmarshals a JSON value that may be a string or a number into a
// string. sqlite3's -json output represents INTEGER columns as JSON numbers,
// so this lets sqliteMessageRow accept either representation of a timestamp
// column depending on the schema.
type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err == nil {
		*f = flexString(n.String())
		return nil
	}
	*f = ""
	return nil
}

// sqliteMessageRow represents one row read from OpenCode's message table
// under the newer segmented "parts" schema.
type sqliteMessageRow struct {
	ID        string     `json:"id"`
	Role      string     `json:"role"`
	Parts     string     `json:"parts"`
	CreatedAt flexString `json:"created_at"`
}

// sqlitePart represents one segment of an OpenCode message's "parts" array.
type sqlitePart struct {
	Type string `json:"type"`
	Data struct {
		Text string `json:"text"`
	} `json:"data"`
}

// readSQLiteMessages reads all messages for a session and returns them as a
// JSON array in the normalized shape ParseTranscript expects:
// [{"id":..., "role":..., "content":..., "time":{"created":...}}, ...].
// It supports both the legacy single-blob "data" column schema and the newer
// segmented "parts" column schema.
func readSQLiteMessages(dbPath, table, sessionID string) ([]byte, error) {
	cols, err := sqliteTableColumns(dbPath, table)
	if err != nil {
		return nil, err
	}

	sessionCol := pickSQLiteColumn(cols, "session_id", "sessionid", "session")
	idCol := pickSQLiteColumn(cols, "id")
	if sessionCol == "" || idCol == "" {
		return nil, fmt.Errorf("opencode: unrecognized message table schema")
	}
	timeCol := pickSQLiteColumn(cols, "created_at", "time_created", "created")
	roleCol := pickSQLiteColumn(cols, "role")
	partsCol := pickSQLiteColumn(cols, "parts")
	dataCol := pickSQLiteColumn(cols, "data")

	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(` ORDER BY "%s"`, timeCol)
	}

	if dataCol != "" {
		// Legacy schema: the full message JSON is stored in one blob column.
		query := fmt.Sprintf(
			`SELECT json_group_array(json_patch("%s", json_object('id', "%s"))) FROM "%s" WHERE "%s"='%s'%s;`,
			dataCol, idCol, table, sessionCol, sqliteEscape(sessionID), orderClause,
		)
		cmd := exec.Command("sqlite3", dbPath, query)
		output, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		data := bytes.TrimSpace(output)
		if len(data) == 0 || string(data) == "[null]" || string(data) == "[]" {
			return nil, nil
		}
		return data, nil
	}

	if roleCol != "" && partsCol != "" {
		// Newer schema: message content is segmented into a "parts" array.
		selectCols := fmt.Sprintf(`"%s" AS id, "%s" AS role, "%s" AS parts`, idCol, roleCol, partsCol)
		if timeCol != "" {
			selectCols += fmt.Sprintf(`, "%s" AS created_at`, timeCol)
		}
		query := fmt.Sprintf(`SELECT %s FROM "%s" WHERE "%s"='%s'%s;`,
			selectCols, table, sessionCol, sqliteEscape(sessionID), orderClause)

		cmd := exec.Command("sqlite3", "-json", dbPath, query)
		output, err := cmd.Output()
		if err != nil {
			return nil, err
		}

		var rows []sqliteMessageRow
		if err := json.Unmarshal(output, &rows); err != nil {
			return nil, err
		}

		normalized := make([]map[string]interface{}, 0, len(rows))
		for _, row := range rows {
			normalized = append(normalized, map[string]interface{}{
				"id":      row.ID,
				"role":    row.Role,
				"content": extractPartsText(row.Parts),
				"time":    map[string]interface{}{"created": string(row.CreatedAt)},
			})
		}
		return json.Marshal(normalized)
	}

	return nil, fmt.Errorf("opencode: unrecognized message table schema")
}

// extractPartsText joins the text segments of an OpenCode "parts" JSON array
// into a single string, ignoring non-text segments (tool calls, reasoning, etc).
func extractPartsText(rawParts string) string {
	if rawParts == "" {
		return ""
	}

	var parts []sqlitePart
	if err := json.Unmarshal([]byte(rawParts), &parts); err != nil {
		return ""
	}

	var texts []string
	for _, p := range parts {
		if p.Type == "text" && p.Data.Text != "" {
			texts = append(texts, p.Data.Text)
		}
	}
	return strings.Join(texts, "\n")
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
