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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session belonging to projectPath. OpenCode's internal SQLite schema (table
// names, column names, and even its own project-identification scheme) has
// changed across releases, so this introspects the live schema via
// PRAGMA table_info instead of assuming fixed names, and tries several
// matching strategies before giving up.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, sessionCols := resolveSQLiteTable(dbPath, []string{"session", "sessions"})
	if sessionTable == "" {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, []string{"id"})
	if idCol == "" {
		return nil, nil
	}
	projectCol := pickColumn(sessionCols, []string{"project_id", "projectid", "project"})
	dirCol := pickColumn(sessionCols, []string{"directory", "dir", "cwd", "workdir", "path"})
	timeCol := pickColumn(sessionCols, []string{"time_updated", "updated_at", "updatedat", "time_created", "created_at"})

	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}

	findSessionID := func(col, val string) string {
		if col == "" || val == "" {
			return ""
		}
		q := fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;",
			idCol, sessionTable, col, sqlQuote(val), orderClause)
		cmd := exec.Command("sqlite3", dbPath, q)
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	// Strategy 1: match by our own project-id scheme (root commit hash).
	sessionID := findSessionID(projectCol, projectID)

	// Strategy 2: match by working directory, in case OpenCode's project
	// identification scheme differs from ours.
	if sessionID == "" {
		sessionID = findSessionID(dirCol, projectPath)
	}

	// Strategy 3: fall back to the single most recently active session.
	if sessionID == "" {
		q := fmt.Sprintf("SELECT %s FROM %s%s LIMIT 1;", idCol, sessionTable, orderClause)
		cmd := exec.Command("sqlite3", dbPath, q)
		if out, err := cmd.Output(); err == nil {
			sessionID = strings.TrimSpace(string(out))
		}
	}

	if sessionID == "" {
		return nil, nil
	}

	// Best-effort recency check; if we can't read/parse the timestamp we
	// proceed anyway rather than dropping a session we did find.
	if timeCol != "" {
		q := fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s';", timeCol, sessionTable, idCol, sqlQuote(sessionID))
		cmd := exec.Command("sqlite3", dbPath, q)
		if out, err := cmd.Output(); err == nil {
			if t, ok := parseSQLiteTime(strings.TrimSpace(string(out))); ok {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
		}
	}

	transcriptData := fetchSQLiteMessages(dbPath, sessionID)
	if len(transcriptData) == 0 {
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

// fetchSQLiteMessages retrieves all messages for a session as a JSON array of
// objects, tolerating drift in the message table's column names and in
// whether the row's payload column holds a JSON object or array.
func fetchSQLiteMessages(dbPath, sessionID string) []byte {
	msgTable, msgCols := resolveSQLiteTable(dbPath, []string{"message", "messages"})
	if msgTable == "" {
		return nil
	}

	idCol := pickColumn(msgCols, []string{"id"})
	sessionCol := pickColumn(msgCols, []string{"session_id", "sessionid", "session"})
	dataCol := pickColumn(msgCols, []string{"data", "content", "parts", "body", "payload"})
	roleCol := pickColumn(msgCols, []string{"role", "type"})
	timeCol := pickColumn(msgCols, []string{"time_created", "created_at", "createdat", "time"})

	if idCol == "" || sessionCol == "" || dataCol == "" {
		return nil
	}

	selectCols := fmt.Sprintf("%s AS id, %s AS data", idCol, dataCol)
	if roleCol != "" {
		selectCols += fmt.Sprintf(", %s AS role", roleCol)
	}

	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s", timeCol)
	}

	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s'%s;",
		selectCols, msgTable, sessionCol, sqlQuote(sessionID), orderClause)
	cmd := exec.Command("sqlite3", "-json", dbPath, q)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var rows []struct {
		ID   string `json:"id"`
		Data string `json:"data"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) == 0 {
		return nil
	}

	var messages []json.RawMessage
	for _, row := range rows {
		obj := map[string]json.RawMessage{}

		var asObject map[string]json.RawMessage
		if err := json.Unmarshal([]byte(row.Data), &asObject); err == nil {
			obj = asObject
		} else if json.Valid([]byte(row.Data)) {
			// Payload isn't a JSON object (e.g. a "parts" array) — keep it
			// under its original column name so downstream parsing can
			// still find it if it knows to look there.
			obj[dataCol] = json.RawMessage(row.Data)
		} else {
			continue
		}

		if row.Role != "" {
			if roleJSON, err := json.Marshal(row.Role); err == nil {
				obj["role"] = roleJSON
			}
		}
		if idJSON, err := json.Marshal(row.ID); err == nil {
			obj["id"] = idJSON
		}

		merged, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		messages = append(messages, merged)
	}
	if len(messages) == 0 {
		return nil
	}

	data, err := json.Marshal(messages)
	if err != nil {
		return nil
	}
	return data
}

// resolveSQLiteTable returns the name and column list of the first candidate
// table (in order) that actually exists in the database.
func resolveSQLiteTable(dbPath string, candidates []string) (string, []string) {
	for _, name := range candidates {
		if cols := sqliteColumns(dbPath, name); len(cols) > 0 {
			return name, cols
		}
	}
	return "", nil
}

// sqliteColumns returns the column names of a SQLite table via
// PRAGMA table_info, or nil if the table doesn't exist or sqlite3 fails.
// PRAGMA table_info silently returns no rows for a missing table rather
// than erroring, which is what lets resolveSQLiteTable probe candidates.
func sqliteColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// pickColumn returns the first column in cols matching one of the candidate
// names, preferring an exact (case-insensitive) match before falling back
// to a substring match.
func pickColumn(cols []string, candidates []string) string {
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.EqualFold(c, cand) {
				return c
			}
		}
	}
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.Contains(strings.ToLower(c), cand) {
				return c
			}
		}
	}
	return ""
}

// sqlQuote escapes a string for safe inclusion in a single-quoted SQLite
// string literal.
func sqlQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// parseSQLiteTime parses a timestamp value from OpenCode's SQLite storage,
// which may be an ISO-8601 string or a Unix epoch (seconds, milliseconds,
// or microseconds) depending on the schema version.
func parseSQLiteTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}

	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, raw); err == nil {
			return t, true
		}
	}

	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
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
```
