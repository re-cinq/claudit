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
// Newer OpenCode releases have been observed storing sessions either nested
// under a per-project directory or flat directly under storage/session/, so
// both layouts are tried.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	sessionDir, err := GetSessionDir(projectPath)
	if err == nil {
		if sessionID, modTime := findRecentSessionFile(sessionDir); sessionID != "" {
			msgDir, _ := GetMessageDir(sessionID)
			return &agent.SessionInfo{
				SessionID:      sessionID,
				TranscriptPath: msgDir,
				StartedAt:      modTime.Format(time.RFC3339),
				ProjectPath:    projectPath,
			}, nil
		}
	}

	// Fall back to a flat (non-project-nested) session directory layout.
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}
	flatDir := filepath.Join(dataDir, "storage", "session")
	if sessionID, modTime := findRecentSessionFile(flatDir); sessionID != "" {
		msgDir, _ := GetMessageDir(sessionID)
		return &agent.SessionInfo{
			SessionID:      sessionID,
			TranscriptPath: msgDir,
			StartedAt:      modTime.Format(time.RFC3339),
			ProjectPath:    projectPath,
		}, nil
	}

	return nil, nil
}

// findRecentSessionFile scans dir for the most recently modified *.json
// session file within the recent-session timeout. Returns "" if none found.
func findRecentSessionFile(dir string) (sessionID string, modTime time.Time) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}
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

		mt := info.ModTime()
		if now.Sub(mt) > agent.RecentSessionTimeout {
			continue
		}

		if bestSessionID == "" || mt.After(bestModTime) {
			bestSessionID = strings.TrimSuffix(entry.Name(), ".json")
			bestModTime = mt
		}
	}

	return bestSessionID, bestModTime
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
// Table/column names are introspected rather than hardcoded, since they have
// changed across OpenCode releases; any single query failing (e.g. because a
// column was renamed) falls back to a looser query instead of discarding a
// perfectly valid session.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := sqliteResolveTable(dbPath, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}
	sessionCols := sqliteTableColumns(dbPath, sessionTable)

	idCol := sqliteFirstColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := sqliteFirstColumn(sessionCols, "time_updated", "updated_at", "updated", "time_created", "created_at")
	projectCol := sqliteFirstColumn(sessionCols, "project_id", "projectID", "project", "directory", "worktree")

	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}

	var sessionID string
	if projectCol != "" {
		for _, candidate := range []string{projectID, projectPath} {
			query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;`,
				idCol, sessionTable, projectCol, sqliteEscape(candidate), orderClause)
			if out, err := sqliteQueryOne(dbPath, query); err == nil && out != "" {
				sessionID = out
				break
			}
		}
	}

	if sessionID == "" {
		// Project column may be missing/renamed/mismatched — fall back to
		// the most recently active session overall (still bounded by the
		// recency check below).
		query := fmt.Sprintf(`SELECT %s FROM %s%s LIMIT 1;`, idCol, sessionTable, orderClause)
		out, err := sqliteQueryOne(dbPath, query)
		if err != nil || out == "" {
			return nil, nil
		}
		sessionID = out
	}

	// Check if this session was recent (within timeout)
	if timeCol != "" {
		query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, timeCol, sessionTable, idCol, sqliteEscape(sessionID))
		if timeStr, err := sqliteQueryOne(dbPath, query); err == nil && timeStr != "" {
			if t, ok := parseSQLiteTime(timeStr); ok {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
			// If we can't parse the time, proceed anyway — better to try than skip
		}
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: sqliteFetchMessages(dbPath, sessionID),
	}, nil
}

// sqliteFetchMessages returns the messages for sessionID as a JSON array.
// Column names are introspected; if the message table/columns can't be
// resolved or the query fails, an empty array is returned rather than
// discarding the (already-found) session.
func sqliteFetchMessages(dbPath, sessionID string) []byte {
	empty := []byte("[]")

	messageTable := sqliteResolveTable(dbPath, "message", "messages")
	if messageTable == "" {
		return empty
	}
	msgCols := sqliteTableColumns(dbPath, messageTable)

	sessionCol := sqliteFirstColumn(msgCols, "session_id", "sessionID", "session")
	idCol := sqliteFirstColumn(msgCols, "id")
	if sessionCol == "" || idCol == "" {
		return empty
	}
	createdCol := sqliteFirstColumn(msgCols, "time_created", "created_at", "created")

	orderClause := ""
	if createdCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s", createdCol)
	}

	var query string
	if sqliteHasColumn(msgCols, "data") {
		query = fmt.Sprintf(
			`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM %s WHERE %s='%s'%s;`,
			messageTable, sessionCol, sqliteEscape(sessionID), orderClause)
	} else {
		fields := []string{fmt.Sprintf("'id', %s", idCol)}
		if roleCol := sqliteFirstColumn(msgCols, "role"); roleCol != "" {
			fields = append(fields, fmt.Sprintf("'role', %s", roleCol))
		}
		// "parts" stores a JSON-encoded array as text; embed it parsed via
		// json() rather than as an escaped string. Plain "content"/"text"
		// columns are embedded as-is.
		if partsCol := sqliteFirstColumn(msgCols, "parts"); partsCol != "" {
			fields = append(fields, fmt.Sprintf("'content', json(%s)", partsCol))
		} else if contentCol := sqliteFirstColumn(msgCols, "content", "text"); contentCol != "" {
			fields = append(fields, fmt.Sprintf("'content', %s", contentCol))
		}
		if createdCol != "" {
			fields = append(fields, fmt.Sprintf("'time', json_object('created', %s)", createdCol))
		}
		query = fmt.Sprintf(`SELECT json_group_array(json_object(%s)) FROM %s WHERE %s='%s'%s;`,
			strings.Join(fields, ", "), messageTable, sessionCol, sqliteEscape(sessionID), orderClause)
	}

	out, err := sqliteQueryOne(dbPath, query)
	if err != nil || out == "" || out == "[null]" {
		return empty
	}
	return []byte(out)
}

// sqliteQueryOne runs a single-value/single-column SQLite query and returns
// the trimmed output.
func sqliteQueryOne(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sqliteTableColumns returns the column names of table via PRAGMA table_info.
// Returns nil if the table doesn't exist or has no columns.
func sqliteTableColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", "-separator", "|", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(trimmed, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// sqliteResolveTable returns the first candidate table name that exists in dbPath.
func sqliteResolveTable(dbPath string, candidates ...string) string {
	for _, name := range candidates {
		if cols := sqliteTableColumns(dbPath, name); len(cols) > 0 {
			return name
		}
	}
	return ""
}

// sqliteHasColumn reports whether name is present in cols.
func sqliteHasColumn(cols []string, name string) bool {
	for _, c := range cols {
		if c == name {
			return true
		}
	}
	return false
}

// sqliteFirstColumn returns the first candidate present in cols, or "".
func sqliteFirstColumn(cols []string, candidates ...string) string {
	for _, cand := range candidates {
		if sqliteHasColumn(cols, cand) {
			return cand
		}
	}
	return ""
}

// sqliteEscape escapes single quotes for safe inclusion in a SQLite string literal.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// parseSQLiteTime parses a timestamp stored in a SQLite column, trying
// several text formats as well as Unix epoch (s/ms/us) integers.
func parseSQLiteTime(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, true
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
