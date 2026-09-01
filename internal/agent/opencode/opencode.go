package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
// OpenCode's on-disk database schema (table names, column names) has changed
// across releases (e.g. singular vs. plural table names, "time_updated" vs.
// "updated_at"), so the schema is detected at runtime via sqlite_master and
// PRAGMA table_info instead of being hardcoded. Project-scoping and
// timestamp columns are treated as optional so discovery still degrades
// gracefully (falling back to "most recent session in the database") if
// OpenCode's project identity scheme no longer matches our computed
// projectID.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	schema, err := detectOpenCodeSQLSchema(dbPath)
	if err != nil {
		return nil, nil
	}

	sessionID, updatedAt, err := findRecentSQLiteSession(dbPath, schema, projectID)
	if err != nil || sessionID == "" {
		return nil, nil
	}

	if updatedAt != "" {
		if t, ok := parseOpenCodeTimestamp(updatedAt); ok && time.Since(t) > agent.RecentSessionTimeout {
			return nil, nil
		}
	}

	transcriptData, err := loadSQLiteMessages(dbPath, schema, sessionID)
	if err != nil {
		return nil, nil
	}

	trimmed := strings.TrimSpace(string(transcriptData))
	// sqlite3 returns "[null]" when no rows match
	if trimmed == "" || trimmed == "[null]" || trimmed == "[]" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: []byte(trimmed),
	}, nil
}

// openCodeSQLSchema describes the table/column names discovered in an
// OpenCode SQLite database. SessionProject, SessionUpdated, MessageRole and
// MessageCreated are optional and left as "" when no matching column exists.
type openCodeSQLSchema struct {
	SessionTable   string
	SessionID      string
	SessionProject string
	SessionUpdated string

	MessageTable   string
	MessageID      string
	MessageSession string
	MessageBody    string
	MessageRole    string
	MessageCreated string
}

// identPattern matches safe, unquoted SQLite identifiers. Only names
// matching this are ever interpolated into a query.
var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// detectOpenCodeSQLSchema inspects an OpenCode SQLite database to determine
// its actual table and column names, tolerating renames across releases.
func detectOpenCodeSQLSchema(dbPath string) (*openCodeSQLSchema, error) {
	tables, err := sqliteQueryLines(dbPath, `SELECT name FROM sqlite_master WHERE type='table';`)
	if err != nil {
		return nil, err
	}

	sessionTable := pickIdent(tables, "session", "sessions")
	messageTable := pickIdent(tables, "message", "messages")
	if sessionTable == "" || messageTable == "" {
		return nil, fmt.Errorf("opencode: could not identify session/message tables")
	}

	sessionCols, err := sqliteTableColumns(dbPath, sessionTable)
	if err != nil {
		return nil, err
	}
	messageCols, err := sqliteTableColumns(dbPath, messageTable)
	if err != nil {
		return nil, err
	}

	schema := &openCodeSQLSchema{
		SessionTable:   sessionTable,
		SessionID:      pickIdent(sessionCols, "id"),
		SessionProject: pickIdent(sessionCols, "project_id", "projectid", "project", "directory", "cwd"),
		SessionUpdated: pickIdent(sessionCols, "time_updated", "updated_at", "updatedat", "updated", "modified_at"),
		MessageTable:   messageTable,
		MessageID:      pickIdent(messageCols, "id"),
		MessageSession: pickIdent(messageCols, "session_id", "sessionid"),
		MessageBody:    pickIdent(messageCols, "data", "parts", "content", "body"),
		MessageRole:    pickIdent(messageCols, "role"),
		MessageCreated: pickIdent(messageCols, "time_created", "created_at", "createdat", "created"),
	}

	if schema.SessionID == "" || schema.MessageID == "" || schema.MessageSession == "" || schema.MessageBody == "" {
		return nil, fmt.Errorf("opencode: unrecognized database schema")
	}

	return schema, nil
}

// pickIdent returns the first candidate that case-insensitively matches a
// name in names, restricted to safe SQL identifiers. Returns "" if none match.
func pickIdent(names []string, candidates ...string) string {
	for _, candidate := range candidates {
		for _, name := range names {
			if strings.EqualFold(name, candidate) && identPattern.MatchString(name) {
				return name
			}
		}
	}
	return ""
}

// sqliteQueryLines runs a query against dbPath and returns non-empty output lines.
func sqliteQueryLines(dbPath, query string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// sqliteTableColumns returns the column names of a table via PRAGMA table_info.
func sqliteTableColumns(dbPath, table string) ([]string, error) {
	if !identPattern.MatchString(table) {
		return nil, fmt.Errorf("opencode: unsafe table name %q", table)
	}
	rows, err := sqliteQueryLines(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, row := range rows {
		fields := strings.Split(row, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// findRecentSQLiteSession returns the most recently updated session's ID
// (and its raw "updated" column value, if the schema has one) for the given
// project. If no session matches the project scope (or the schema has no
// project-scoping column), it falls back to the most recent session overall,
// since OpenCode's project identity scheme has changed across releases and
// may no longer align with our computed projectID.
func findRecentSQLiteSession(dbPath string, schema *openCodeSQLSchema, projectID string) (sessionID, updatedAt string, err error) {
	selectCols := schema.SessionID
	if schema.SessionUpdated != "" {
		selectCols += ", " + schema.SessionUpdated
	}

	orderBy := " ORDER BY rowid DESC"
	if schema.SessionUpdated != "" {
		orderBy = fmt.Sprintf(" ORDER BY %s DESC", schema.SessionUpdated)
	}

	var queries []string
	if schema.SessionProject != "" {
		queries = append(queries, fmt.Sprintf(
			"SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;",
			selectCols, schema.SessionTable, schema.SessionProject, escapeSQLiteLiteral(projectID), orderBy,
		))
	}
	queries = append(queries, fmt.Sprintf(
		"SELECT %s FROM %s%s LIMIT 1;",
		selectCols, schema.SessionTable, orderBy,
	))

	for _, query := range queries {
		lines, qErr := sqliteQueryLines(dbPath, query)
		if qErr != nil || len(lines) == 0 {
			continue
		}
		fields := strings.SplitN(lines[0], "|", 2)
		id := strings.TrimSpace(fields[0])
		if id == "" {
			continue
		}
		var updated string
		if len(fields) > 1 {
			updated = strings.TrimSpace(fields[1])
		}
		return id, updated, nil
	}

	return "", "", nil
}

// loadSQLiteMessages returns the messages for a session as a JSON array,
// normalized to the {"id":..., "role":..., "content":...} shape our
// transcript parser expects, regardless of whether the underlying body
// column stores that shape directly (older releases) or a differently
// structured JSON value (newer releases).
func loadSQLiteMessages(dbPath string, schema *openCodeSQLSchema, sessionID string) ([]byte, error) {
	body := fmt.Sprintf(
		`CASE WHEN json_valid(%[1]s) AND json_type(%[1]s)='object' `+
			`THEN json_patch(%[1]s, json_object('id', %[2]s)) `+
			`ELSE json_object('id', %[2]s, 'content', json(%[1]s)) END`,
		schema.MessageBody, schema.MessageID,
	)
	if schema.MessageRole != "" {
		body = fmt.Sprintf("json_set(%s, '$.role', %s)", body, schema.MessageRole)
	}

	orderBy := ""
	if schema.MessageCreated != "" {
		orderBy = fmt.Sprintf(" ORDER BY %s", schema.MessageCreated)
	}

	query := fmt.Sprintf(
		"SELECT json_group_array(%s) FROM %s WHERE %s='%s'%s;",
		body, schema.MessageTable, schema.MessageSession, escapeSQLiteLiteral(sessionID), orderBy,
	)

	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(string(output))), nil
}

// escapeSQLiteLiteral escapes a value for safe interpolation into a SQLite
// string literal.
func escapeSQLiteLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// parseOpenCodeTimestamp parses a session's "updated" value, which may be an
// RFC3339-ish string or a Unix epoch (seconds, milliseconds, or
// microseconds) depending on the OpenCode release.
func parseOpenCodeTimestamp(v string) (time.Time, bool) {
	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, v); err == nil {
			return t, true
		}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		switch {
		case n > 1e15:
			return time.UnixMicro(n), true
		case n > 1e12:
			return time.UnixMilli(n), true
		case n > 0:
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
