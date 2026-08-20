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
// session belonging to this project.
//
// Table and column names are discovered dynamically (via sqlite_master and
// PRAGMA table_info) rather than hardcoded, because OpenCode's on-disk schema
// has changed across releases — e.g. table names have been pluralised and
// project scoping has moved from a project_id column (derived from the git
// root commit) to a directory column (the absolute project path, which is
// also what OpenCode's plugin API exposes as "directory" — see plugin.go).
// We try directory-based matching first, then fall back to project_id-based
// matching for older databases, and finally to "most recently updated
// session" if neither scoping column is present.
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

	sessionCols := sqliteTableColumns(dbPath, sessionTable)
	idCol := pickSQLiteColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	updatedCol := pickSQLiteColumn(sessionCols,
		"time_updated", "updated_at", "updatedAt", "timeUpdated", "updated", "mtime")
	dirCol := pickSQLiteColumn(sessionCols,
		"directory", "path", "cwd", "worktree", "project_dir", "projectDir")
	projectCol := pickSQLiteColumn(sessionCols, "project_id", "projectID", "project")

	orderBy := ""
	if updatedCol != "" {
		orderBy = fmt.Sprintf(" ORDER BY %s DESC", updatedCol)
	}

	sessionID := ""
	if dirCol != "" {
		sessionID = sqliteScalar(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;`,
			idCol, sessionTable, dirCol, sqlQuote(projectPath), orderBy))
	}
	if sessionID == "" && projectCol != "" {
		sessionID = sqliteScalar(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;`,
			idCol, sessionTable, projectCol, sqlQuote(projectID), orderBy))
	}
	if sessionID == "" && dirCol == "" && projectCol == "" {
		// No project-scoping column found at all; fall back to the most
		// recently updated session in the database.
		sessionID = sqliteScalar(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s%s LIMIT 1;`, idCol, sessionTable, orderBy))
	}
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout)
	if updatedCol != "" {
		timeStr := sqliteScalar(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s';`, updatedCol, sessionTable, idCol, sqlQuote(sessionID)))
		if timeStr != "" && !isRecentTimestamp(timeStr) {
			return nil, nil
		}
		// If we can't parse the time, proceed anyway — better to try than skip
	}

	// Get messages for this session as a JSON array
	transcriptData := sqliteSessionMessages(dbPath, sessionID)

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// sqliteSessionMessages fetches all messages for a session as a JSON array,
// discovering the message table/column names dynamically. It handles both
// the older schema (a single "data"/"content" column holding a full JSON
// message object) and the newer "parts" array schema (role + parts columns).
func sqliteSessionMessages(dbPath, sessionID string) []byte {
	msgTable := findSQLiteTable(dbPath, "message", "messages")
	if msgTable == "" {
		return nil
	}

	msgCols := sqliteTableColumns(dbPath, msgTable)
	sessionRefCol := pickSQLiteColumn(msgCols, "session_id", "sessionID", "sessionId")
	if sessionRefCol == "" {
		return nil
	}
	msgIDCol := pickSQLiteColumn(msgCols, "id")
	createdCol := pickSQLiteColumn(msgCols,
		"time_created", "created_at", "createdAt", "timeCreated", "created")
	roleCol := pickSQLiteColumn(msgCols, "role")
	partsCol := pickSQLiteColumn(msgCols, "parts")
	blobCol := pickSQLiteColumn(msgCols, "data", "content", "message")

	orderBy := ""
	if createdCol != "" {
		orderBy = fmt.Sprintf(" ORDER BY %s", createdCol)
	}

	var rowExpr string
	switch {
	case blobCol != "":
		// Older schema: the column already holds a full JSON message object.
		if msgIDCol != "" {
			rowExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", blobCol, msgIDCol)
		} else {
			rowExpr = blobCol
		}
	case partsCol != "" || roleCol != "":
		var fields []string
		if msgIDCol != "" {
			fields = append(fields, fmt.Sprintf("'id', %s", msgIDCol))
		}
		if roleCol != "" {
			fields = append(fields, fmt.Sprintf("'role', %s", roleCol))
		}
		if createdCol != "" {
			fields = append(fields, fmt.Sprintf("'time', json_object('created', %s)", createdCol))
		}
		if partsCol != "" {
			fields = append(fields, fmt.Sprintf("'parts', json(%s)", partsCol))
		}
		if len(fields) == 0 {
			return nil
		}
		rowExpr = fmt.Sprintf("json_object(%s)", strings.Join(fields, ", "))
	default:
		return nil
	}

	query := fmt.Sprintf(`SELECT json_group_array(%s) FROM %s WHERE %s='%s'%s;`,
		rowExpr, msgTable, sessionRefCol, sqlQuote(sessionID), orderBy)

	out := sqliteScalar(dbPath, query)
	// sqlite3 returns "[null]" when no rows match
	if out == "" || out == "[null]" || out == "[]" {
		return nil
	}
	return []byte(out)
}

// sqliteScalar runs a query against the sqlite3 CLI and returns the trimmed
// output, or "" on error or empty result.
func sqliteScalar(dbPath, query string) string {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// findSQLiteTable returns the first table in the database matching one of the
// given candidate names: exact (case-insensitive) matches are preferred over
// substring matches, so callers can list singular/plural variants in order
// of preference.
func findSQLiteTable(dbPath string, candidates ...string) string {
	out := sqliteScalar(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if out == "" {
		return ""
	}
	tables := strings.Split(out, "\n")

	for _, want := range candidates {
		for _, t := range tables {
			t = strings.TrimSpace(t)
			if strings.EqualFold(t, want) {
				return t
			}
		}
	}
	for _, want := range candidates {
		for _, t := range tables {
			t = strings.TrimSpace(t)
			if t != "" && strings.Contains(strings.ToLower(t), strings.ToLower(want)) {
				return t
			}
		}
	}
	return ""
}

// sqliteTableColumns returns the column names of a table via PRAGMA table_info.
func sqliteTableColumns(dbPath, table string) []string {
	out := sqliteScalar(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if out == "" {
		return nil
	}
	var cols []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// pickSQLiteColumn returns the first candidate present in cols (case-insensitive).
func pickSQLiteColumn(cols []string, candidates ...string) string {
	for _, want := range candidates {
		for _, c := range cols {
			if strings.EqualFold(c, want) {
				return c
			}
		}
	}
	return ""
}

// sqlQuote escapes single quotes for safe interpolation into a SQL string literal.
func sqlQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// isRecentTimestamp reports whether a timestamp value (an RFC3339-family
// string, or a Unix epoch integer in seconds/milliseconds/microseconds/
// nanoseconds) falls within RecentSessionTimeout. Unparseable values are
// treated as recent — better to try storing than to silently skip.
func isRecentTimestamp(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}

	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Since(unixFromMagnitude(n)) <= agent.RecentSessionTimeout
	}

	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
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

// unixFromMagnitude converts a Unix epoch integer of unknown resolution
// (seconds, milliseconds, microseconds, or nanoseconds) to a time.Time,
// inferring the resolution from its magnitude.
func unixFromMagnitude(n int64) time.Time {
	switch {
	case n > 1e17:
		return time.Unix(0, n) // nanoseconds
	case n > 1e14:
		return time.UnixMicro(n)
	case n > 1e11:
		return time.UnixMilli(n)
	default:
		return time.Unix(n, 0)
	}
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

	// Try "parts" as an array of typed content blocks (newer OpenCode schema:
	// [{"type":"text","text":"..."}] or [{"type":"text","data":{"text":"..."}}]).
	if partsRaw, ok := raw["parts"]; ok {
		var parts []struct {
			Type string          `json:"type"`
			Text string          `json:"text"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(partsRaw, &parts); err == nil {
			var texts []string
			for _, p := range parts {
				if p.Type != "text" {
					continue
				}
				if p.Text != "" {
					texts = append(texts, p.Text)
					continue
				}
				if len(p.Data) > 0 {
					var d struct {
						Text string `json:"text"`
					}
					if json.Unmarshal(p.Data, &d) == nil && d.Text != "" {
						texts = append(texts, d.Text)
					}
				}
			}
			if len(texts) > 0 {
				msg.Content = []agent.ContentBlock{{Type: "text", Text: strings.Join(texts, "\n")}}
				return msg
			}
		}
	}

	return msg
}
```
