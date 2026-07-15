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
// OpenCode has renamed its session/message table and column names across
// releases, so the table and column names used here are discovered at
// runtime via SQLite schema introspection (sqlite_master / PRAGMA
// table_info) rather than hard-coded, to keep working across those
// upstream schema changes.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findSQLiteDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, sessionCols, err := findTableAndColumns(dbPath, "session")
	if err != nil || sessionTable == "" {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project", "directory", "dir", "cwd", "path", "worktree", "worktree_id")
	timeCol := pickColumn(sessionCols, "time_updated", "updated_at", "timeupdated", "updated", "modified", "time_created", "created_at", "time", "created")

	sessionID, err := findRecentSessionID(dbPath, sessionTable, idCol, projectCol, timeCol, projectID, projectPath)
	if err != nil || sessionID == "" {
		return nil, nil
	}

	messageTable, messageCols, err := findTableAndColumns(dbPath, "message")
	if err != nil || messageTable == "" {
		return nil, nil
	}

	msgIDCol := pickColumn(messageCols, "id")
	msgSessionCol := pickColumn(messageCols, "session_id", "sessionid", "session")
	msgDataCol := pickColumn(messageCols, "data", "content", "body", "payload")
	msgOrderCol := pickColumn(messageCols, "time_created", "created_at", "timecreated", "created", "time")
	if msgSessionCol == "" {
		return nil, nil
	}

	transcriptData, err := fetchMessages(dbPath, messageTable, msgIDCol, msgSessionCol, msgDataCol, msgOrderCol, sessionID)
	if err != nil {
		return nil, nil
	}

	// sqlite3 returns "[null]" when no rows match
	trimmed := strings.TrimSpace(string(transcriptData))
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

// findSQLiteDB locates OpenCode's SQLite database file. It checks the
// well-known "opencode.db" path first, then falls back to a shallow search
// in case the filename or nesting has changed across versions.
func findSQLiteDB(dataDir string) string {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); err == nil {
		return dbPath
	}

	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() {
			if path == dataDir {
				return nil
			}
			rel, relErr := filepath.Rel(dataDir, path)
			if relErr == nil && strings.Count(rel, string(filepath.Separator)) >= 2 {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".db") {
			found = path
		}
		return nil
	})
	return found
}

// findTableAndColumns looks up a table whose name matches (or contains) the
// given keyword and returns its name along with its column names, using
// sqlite_master and PRAGMA table_info introspection.
func findTableAndColumns(dbPath, keyword string) (string, []string, error) {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	output, err := cmd.Output()
	if err != nil {
		return "", nil, err
	}

	var table string
	for _, name := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if lower == keyword || lower == keyword+"s" {
			table = name
			break
		}
		if table == "" && strings.Contains(lower, keyword) {
			table = name
		}
	}
	if table == "" {
		return "", nil, nil
	}

	cmd = exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(table)))
	output, err = cmd.Output()
	if err != nil {
		return table, nil, err
	}

	var columns []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			columns = append(columns, fields[1])
		}
	}
	return table, columns, nil
}

// pickColumn returns the actual column name (preserving case) matching the
// first candidate found in columns, case-insensitively.
func pickColumn(columns []string, candidates ...string) string {
	lowerCols := make(map[string]string, len(columns))
	for _, c := range columns {
		lowerCols[strings.ToLower(c)] = c
	}
	for _, cand := range candidates {
		if actual, ok := lowerCols[cand]; ok {
			return actual
		}
	}
	return ""
}

// quoteIdent quotes a SQLite identifier (table/column name) safely.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqlEscape escapes a string for safe inclusion in a single-quoted SQLite literal.
func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// findRecentSessionID finds the most recent session ID for a project. It
// matches the project column against either the computed project ID or the
// literal project path, since different OpenCode versions have stored
// either value there. If no project column is found (or nothing matches),
// it falls back to the single most recently updated session overall.
func findRecentSessionID(dbPath, table, idCol, projectCol, timeCol, projectID, projectPath string) (string, error) {
	runQuery := func(whereClause string) (string, error) {
		query := fmt.Sprintf("SELECT %s FROM %s", quoteIdent(idCol), quoteIdent(table))
		if whereClause != "" {
			query += " WHERE " + whereClause
		}
		if timeCol != "" {
			query += fmt.Sprintf(" ORDER BY %s DESC", quoteIdent(timeCol))
		}
		query += " LIMIT 1;"

		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		output, err := cmd.Output()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(output)), nil
	}

	if projectCol != "" {
		whereClause := fmt.Sprintf("%s='%s' OR %s='%s'",
			quoteIdent(projectCol), sqlEscape(projectID), quoteIdent(projectCol), sqlEscape(projectPath))
		if sessionID, err := runQuery(whereClause); err == nil && sessionID != "" {
			if isRecentSession(dbPath, table, idCol, timeCol, sessionID) {
				return sessionID, nil
			}
			return "", nil
		}
	}

	// Fall back to the most recent session overall (best-effort, used when
	// the project column is unknown or its stored value doesn't match).
	sessionID, err := runQuery("")
	if err != nil || sessionID == "" {
		return "", err
	}
	if !isRecentSession(dbPath, table, idCol, timeCol, sessionID) {
		return "", nil
	}
	return sessionID, nil
}

// isRecentSession checks whether a session's timestamp is within the recent
// session window. If the timestamp column is unknown or unparseable, it
// returns true — better to try storing the note than to silently skip it.
func isRecentSession(dbPath, table, idCol, timeCol, sessionID string) bool {
	if timeCol == "" {
		return true
	}

	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s';",
		quoteIdent(timeCol), quoteIdent(table), quoteIdent(idCol), sqlEscape(sessionID))
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return true
	}

	timeStr := strings.TrimSpace(string(output))
	t, ok := parseSQLiteTime(timeStr)
	if !ok {
		return true
	}
	return time.Since(t) <= agent.RecentSessionTimeout
}

// parseSQLiteTime parses a timestamp value read from SQLite, which may be
// stored as an ISO-8601 string or as Unix epoch seconds/milliseconds.
func parseSQLiteTime(timeStr string) (time.Time, bool) {
	if timeStr == "" {
		return time.Time{}, false
	}

	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, timeStr); err == nil {
			return t, true
		}
	}

	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		now := time.Now()
		if ms := time.UnixMilli(n); ms.After(now.Add(-24*365*time.Hour)) && ms.Before(now.Add(24*time.Hour)) {
			return ms, true
		}
		if sec := time.Unix(n, 0); sec.After(now.Add(-24*365*time.Hour)) && sec.Before(now.Add(24*time.Hour)) {
			return sec, true
		}
	}

	return time.Time{}, false
}

// fetchMessages retrieves and normalizes message rows for a session from a
// dynamically-discovered message table. It handles both a legacy schema
// (the full message JSON stored as one blob column per row) and a
// normalized schema (role/content/etc. stored as individual columns), and
// returns a JSON array of message objects compatible with parseOpenCodeEntry.
func fetchMessages(dbPath, table, idCol, sessionCol, dataCol, orderCol, sessionID string) ([]byte, error) {
	query := fmt.Sprintf("SELECT * FROM %s WHERE %s='%s'", quoteIdent(table), quoteIdent(sessionCol), sqlEscape(sessionID))
	if orderCol != "" {
		query += fmt.Sprintf(" ORDER BY %s", quoteIdent(orderCol))
	}
	query += ";"

	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(output, &rows); err != nil {
		return nil, err
	}

	messages := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		merged := map[string]json.RawMessage{}

		// Legacy schema: unpack a blob column holding the full message JSON
		// (as a JSON-encoded string) so its fields become top-level.
		if dataCol != "" {
			if raw, ok := row[dataCol]; ok {
				var text string
				var nested map[string]json.RawMessage
				if json.Unmarshal(raw, &text) == nil && text != "" {
					_ = json.Unmarshal([]byte(text), &nested)
				} else {
					_ = json.Unmarshal(raw, &nested)
				}
				for k, v := range nested {
					merged[k] = v
				}
			}
		}

		// Normalized columns always take precedence over anything unpacked
		// from a blob column.
		for k, v := range row {
			merged[k] = v
		}

		if idCol != "" && idCol != "id" {
			if v, ok := row[idCol]; ok {
				merged["id"] = v
			}
		}

		data, err := json.Marshal(merged)
		if err != nil {
			continue
		}
		messages = append(messages, data)
	}

	return json.Marshal(messages)
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
