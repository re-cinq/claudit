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
// Table and column names are introspected at query time rather than hardcoded: OpenCode's
// SQLite schema (table names, column names, and the column used to scope sessions to a
// project) has changed across releases, so a fixed query silently returns zero rows
// against newer schemas instead of erroring. Introspection keeps discovery working across
// those renames instead of just against whichever schema happened to be current when this
// was written.
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
	idCol := findColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := findColumn(sessionCols,
		"time_updated", "updated_at", "updatedat", "updated", "mtime",
		"time_created", "created_at", "createdat", "created")

	// Candidate columns used to scope sessions to this project, tried in order.
	// Hash/id-style columns are compared against our computed projectID; path-style
	// columns are compared against the literal project directory. An unscoped
	// fallback is tried last so a schema/id-scheme change doesn't hide an
	// otherwise-discoverable, recently-updated session.
	type scopeAttempt struct {
		col string
		val string
	}
	var attempts []scopeAttempt
	if c := findColumn(sessionCols, "project_id", "projectid", "project"); c != "" {
		attempts = append(attempts, scopeAttempt{c, projectID})
	}
	if c := findColumn(sessionCols, "directory", "worktree", "cwd", "path"); c != "" {
		attempts = append(attempts, scopeAttempt{c, projectPath})
	}
	attempts = append(attempts, scopeAttempt{"", ""}) // unscoped fallback

	orderClause := " ORDER BY rowid DESC"
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}

	var sessionID, sessionTime string
	for _, a := range attempts {
		selectCols := idCol
		if timeCol != "" {
			selectCols = idCol + ", " + timeCol
		}
		query := fmt.Sprintf("SELECT %s FROM %s", selectCols, sessionTable)
		if a.col != "" {
			query += fmt.Sprintf(" WHERE %s=%s", a.col, sqliteQuote(a.val))
		}
		query += orderClause + " LIMIT 1;"

		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		line := strings.TrimSpace(string(out))
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if fields[0] == "" {
			continue
		}
		sessionID = fields[0]
		if len(fields) > 1 {
			sessionTime = fields[1]
		}
		break
	}

	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout).
	if sessionTime != "" {
		if t, ok := parseSQLiteTime(sessionTime); ok {
			if time.Since(t) > agent.RecentSessionTimeout {
				return nil, nil
			}
		}
		// If we can't parse the time, proceed anyway — better to try than skip
	}

	// Messages are fetched best-effort: a schema change in the message table should
	// not cause us to discard a session we already know is real and recent.
	transcriptData := fetchSQLiteMessages(dbPath, sessionID)

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// findSQLiteTable returns the first table name matching one of the candidates
// (case-insensitive exact match first, then substring match), or "" if none exist.
func findSQLiteTable(dbPath string, candidates ...string) string {
	out, err := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';").Output()
	if err != nil {
		return ""
	}

	var tables []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			tables = append(tables, line)
		}
	}

	for _, c := range candidates {
		for _, t := range tables {
			if strings.EqualFold(t, c) {
				return t
			}
		}
	}
	for _, c := range candidates {
		for _, t := range tables {
			if strings.Contains(strings.ToLower(t), strings.ToLower(c)) {
				return t
			}
		}
	}
	return ""
}

// sqliteColumns returns the column names of a table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) []string {
	out, err := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table)).Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// PRAGMA table_info columns: cid|name|type|notnull|dflt_value|pk
		parts := strings.Split(line, "|")
		if len(parts) > 1 && parts[1] != "" {
			cols = append(cols, parts[1])
		}
	}
	return cols
}

// findColumn returns the first column matching one of the candidates
// (case-insensitive exact match first, then substring match), or "" if none exist.
func findColumn(cols []string, candidates ...string) string {
	for _, c := range candidates {
		for _, col := range cols {
			if strings.EqualFold(col, c) {
				return col
			}
		}
	}
	for _, c := range candidates {
		for _, col := range cols {
			if strings.Contains(strings.ToLower(col), strings.ToLower(c)) {
				return col
			}
		}
	}
	return ""
}

// sqliteQuote quotes a string literal for inclusion in a SQL query.
func sqliteQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// parseSQLiteTime parses a timestamp that may be an ISO-ish string or a unix
// epoch in seconds, milliseconds, or microseconds.
func parseSQLiteTime(s string) (time.Time, bool) {
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
		switch {
		case n > 1e15:
			return time.UnixMicro(n), true
		case n > 1e12:
			return time.UnixMilli(n), true
		default:
			return time.Unix(n, 0), true
		}
	}
	return time.Time{}, false
}

// fetchSQLiteMessages returns a best-effort JSON array of messages for a session.
// It returns an empty JSON array if the message table or its columns can't be
// identified, so a discovered session is never discarded just because message
// lookup failed.
func fetchSQLiteMessages(dbPath, sessionID string) []byte {
	empty := []byte("[]")

	msgTable := findSQLiteTable(dbPath, "message", "messages")
	if msgTable == "" {
		return empty
	}

	cols := sqliteColumns(dbPath, msgTable)
	idCol := findColumn(cols, "id")
	sessionCol := findColumn(cols, "session_id", "sessionid", "session")
	if idCol == "" || sessionCol == "" {
		return empty
	}
	roleCol := findColumn(cols, "role", "type")
	contentCol := findColumn(cols, "data", "content", "parts", "body", "text")
	timeCol := findColumn(cols, "time_created", "created_at", "createdat", "created", "time")

	fields := []string{fmt.Sprintf("'id', %s", idCol)}
	if roleCol != "" {
		fields = append(fields, fmt.Sprintf("'role', %s", roleCol))
	}
	if contentCol != "" {
		fields = append(fields, fmt.Sprintf("'content', %s", contentCol))
	}
	if timeCol != "" {
		fields = append(fields, fmt.Sprintf("'time', json_object('created', %s)", timeCol))
	}

	orderClause := " ORDER BY rowid"
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s", timeCol)
	}

	query := fmt.Sprintf(
		"SELECT json_group_array(json_object(%s)) FROM %s WHERE %s=%s%s;",
		strings.Join(fields, ", "), msgTable, sessionCol, sqliteQuote(sessionID), orderClause,
	)

	out, err := exec.Command("sqlite3", dbPath, query).Output()
	if err != nil {
		return empty
	}

	data := strings.TrimSpace(string(out))
	if data == "" || data == "null" || data == "[null]" {
		return empty
	}
	return []byte(data)
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
