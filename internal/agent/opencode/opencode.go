package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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
// OpenCode's SQLite schema is not a stable public contract — table and column
// names have already changed once across OpenCode releases (e.g. the earlier
// "session"/"message" schema validated against v1.1.60). Rather than hardcode
// names that can silently drift again, this introspects the actual schema at
// runtime (sqlite_master + PRAGMA table_info) and picks the best-matching
// table/column names by substring, so newer OpenCode versions keep working
// without a code change as long as the general session/message shape holds.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	dbPath := findOpenCodeDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	sessionTable, sessionCols := findSQLiteTable(dbPath, []string{"session", "sessions"})
	if sessionTable == "" {
		return nil, nil
	}
	messageTable, messageCols := findSQLiteTable(dbPath, []string{"message", "messages"})
	if messageTable == "" {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project", "directory", "cwd", "path", "worktree")
	updatedCol := pickColumn(sessionCols, "time_updated", "updatedat", "updated_at", "updated", "time_created", "createdat", "created_at", "created")

	// Prefer a session scoped to this project; if the project column no
	// longer matches (schema drift), fall back to the most recently updated
	// session in the database rather than silently finding nothing.
	sessionID := querySQLiteSessionID(dbPath, sessionTable, idCol, projectCol, updatedCol, projectID)
	if sessionID == "" {
		sessionID = querySQLiteSessionID(dbPath, sessionTable, idCol, "", updatedCol, "")
	}
	if sessionID == "" {
		return nil, nil
	}

	if updatedCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`,
			quoteIdent(updatedCol), quoteIdent(sessionTable), quoteIdent(idCol), sqlEscape(sessionID))
		if out, err := exec.Command("sqlite3", dbPath, timeQuery).Output(); err == nil {
			if isStaleTimestamp(strings.TrimSpace(string(out))) {
				return nil, nil
			}
		}
	}

	msgIDCol := pickColumn(messageCols, "id")
	msgSessionCol := pickColumn(messageCols, "session_id", "sessionid", "session")
	msgDataCol := pickColumn(messageCols, "data", "content", "parts", "body", "message")
	msgTimeCol := pickColumn(messageCols, "time_created", "createdat", "created_at", "created", "time_updated", "updatedat", "updated_at")

	transcriptData := buildSQLiteTranscript(dbPath, messageTable, msgIDCol, msgSessionCol, msgDataCol, msgTimeCol, sessionID)
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

// findOpenCodeDB locates OpenCode's SQLite database under dataDir. It checks
// the historically known "opencode.db" path first, then falls back to a
// shallow recursive search for any *.db/*.sqlite* file in case the filename
// or nesting under dataDir changed in a newer OpenCode version.
func findOpenCodeDB(dataDir string) string {
	primary := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(primary); err == nil {
		return primary
	}

	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() {
			rel, relErr := filepath.Rel(dataDir, path)
			if relErr == nil && rel != "." && strings.Count(rel, string(filepath.Separator)) >= 4 {
				return filepath.SkipDir
			}
			return nil
		}
		name := strings.ToLower(d.Name())
		if strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".sqlite") || strings.HasSuffix(name, ".sqlite3") {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// sqliteTables lists table names in the SQLite database.
func sqliteTables(dbPath string) []string {
	out, err := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';").Output()
	if err != nil {
		return nil
	}
	var tables []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			tables = append(tables, line)
		}
	}
	return tables
}

// sqliteColumns lists column names for a table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) []string {
	out, err := exec.Command("sqlite3", "-separator", "\t", dbPath,
		fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(table))).Output()
	if err != nil {
		return nil
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// findSQLiteTable finds the first table matching any of the given candidate
// names (case-insensitive exact match preferred, substring match as fallback)
// and returns its name and columns.
func findSQLiteTable(dbPath string, candidates []string) (string, []string) {
	tables := sqliteTables(dbPath)

	for _, cand := range candidates {
		for _, t := range tables {
			if strings.EqualFold(t, cand) {
				return t, sqliteColumns(dbPath, t)
			}
		}
	}
	for _, cand := range candidates {
		for _, t := range tables {
			if strings.Contains(strings.ToLower(t), cand) {
				return t, sqliteColumns(dbPath, t)
			}
		}
	}
	return "", nil
}

// pickColumn returns the first column matching any candidate name
// (case-insensitive exact match preferred, substring match as fallback).
func pickColumn(cols []string, candidates ...string) string {
	lower := make(map[string]string, len(cols))
	for _, c := range cols {
		lower[strings.ToLower(c)] = c
	}
	for _, cand := range candidates {
		if actual, ok := lower[cand]; ok {
			return actual
		}
	}
	for _, cand := range candidates {
		for lc, actual := range lower {
			if strings.Contains(lc, cand) {
				return actual
			}
		}
	}
	return ""
}

// querySQLiteSessionID finds the most recently updated session ID, optionally
// scoped to a project column/value.
func querySQLiteSessionID(dbPath, table, idCol, projectCol, updatedCol, projectID string) string {
	query := fmt.Sprintf(`SELECT %s FROM %s`, quoteIdent(idCol), quoteIdent(table))
	if projectCol != "" && projectID != "" {
		query += fmt.Sprintf(` WHERE %s='%s'`, quoteIdent(projectCol), sqlEscape(projectID))
	}
	if updatedCol != "" {
		query += fmt.Sprintf(` ORDER BY %s DESC`, quoteIdent(updatedCol))
	}
	query += ` LIMIT 1;`

	out, err := exec.Command("sqlite3", dbPath, query).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// isStaleTimestamp reports whether a raw timestamp value (RFC3339, a common
// SQL datetime string, or a Unix epoch in seconds/milliseconds/microseconds)
// is older than agent.RecentSessionTimeout. Unparseable values are treated as
// not stale — better to try storing than to silently skip a valid session.
func isStaleTimestamp(raw string) bool {
	if raw == "" {
		return false
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return time.Since(t) > agent.RecentSessionTimeout
	}
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", raw); err == nil {
		return time.Since(t) > agent.RecentSessionTimeout
	}
	if t, err := time.Parse("2006-01-02 15:04:05", raw); err == nil {
		return time.Since(t) > agent.RecentSessionTimeout
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		var t time.Time
		switch {
		case n > 1e15:
			t = time.UnixMicro(n)
		case n > 1e12:
			t = time.UnixMilli(n)
		case n > 0:
			t = time.Unix(n, 0)
		default:
			return false
		}
		return time.Since(t) > agent.RecentSessionTimeout
	}
	return false
}

// buildSQLiteTranscript reads all messages for a session and returns them as
// a JSON array, injecting the message id into each entry's data blob so
// ParseTranscript can see it regardless of how the data column is named.
func buildSQLiteTranscript(dbPath, table, idCol, sessionCol, dataCol, timeCol, sessionID string) []byte {
	if sessionCol == "" || dataCol == "" {
		return nil
	}

	selectCols := []string{quoteIdent(dataCol) + " AS data"}
	if idCol != "" {
		selectCols = append([]string{quoteIdent(idCol) + " AS id"}, selectCols...)
	}

	query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s'`,
		strings.Join(selectCols, ", "), quoteIdent(table), quoteIdent(sessionCol), sqlEscape(sessionID))
	if timeCol != "" {
		query += fmt.Sprintf(` ORDER BY %s`, quoteIdent(timeCol))
	}
	query += `;`

	out, err := exec.Command("sqlite3", "-json", dbPath, query).Output()
	if err != nil {
		return nil
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) == 0 {
		return nil
	}

	var messages []json.RawMessage
	for _, row := range rows {
		var id string
		if idRaw, ok := row["id"]; ok {
			_ = json.Unmarshal(idRaw, &id)
		}

		dataRaw, ok := row["data"]
		if !ok {
			continue
		}

		// The data column is TEXT holding JSON, so sqlite3 -json emits it as
		// a JSON-encoded string; unwrap it to get the actual message JSON.
		var rawData string
		if err := json.Unmarshal(dataRaw, &rawData); err != nil {
			// Column wasn't a string (e.g. SQLite JSON1 already returned an object).
			messages = append(messages, dataRaw)
			continue
		}
		if rawData == "" {
			continue
		}

		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(rawData), &obj); err == nil {
			if id != "" {
				if idJSON, err := json.Marshal(id); err == nil {
					obj["id"] = idJSON
				}
			}
			if merged, err := json.Marshal(obj); err == nil {
				messages = append(messages, merged)
				continue
			}
		}

		messages = append(messages, json.RawMessage(rawData))
	}

	if len(messages) == 0 {
		return nil
	}

	result, err := json.Marshal(messages)
	if err != nil {
		return nil
	}
	return result
}

// quoteIdent quotes a SQLite identifier (table/column name) for safe
// interpolation into a query string.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// sqlEscape escapes a value for safe interpolation into a single-quoted
// SQLite string literal.
func sqlEscape(s string) string {
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
