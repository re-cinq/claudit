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
// OpenCode's SQLite schema (table names, column names, database filename, and
// even whether sessions are keyed by a project id or a raw directory/cwd
// path) has changed across releases. Rather than hardcode one snapshot of
// that schema, the table/column names are discovered at runtime via
// sqlite_master and PRAGMA table_info, so this keeps working as the schema
// evolves in minor OpenCode releases.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	dbPath := findOpenCodeDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	tables, err := sqliteTables(dbPath)
	if err != nil || len(tables) == 0 {
		return nil, nil
	}

	sessionTable := pickTable(tables, []string{"session", "sessions"}, "session", "message")
	if sessionTable == "" {
		return nil, nil
	}

	sessionCols, err := sqliteColumns(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	updatedCol := pickColumn(sessionCols,
		"time_updated", "timeupdated", "updated_at", "updatedat", "updated",
		"time_created", "timecreated", "created_at", "createdat", "created")
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project")
	dirCol := pickColumn(sessionCols,
		"directory", "cwd", "workdir", "project_path", "projectpath", "path", "worktree")

	sessionID, updatedStr := findSessionRow(dbPath, sessionTable, idCol, updatedCol, projectCol, dirCol, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	if updatedStr != "" && !withinRecentTimeout(updatedStr) {
		return nil, nil
	}

	messageTable := pickTable(tables, []string{"message", "messages"}, "message", "part")
	if messageTable == "" {
		return nil, nil
	}

	msgCols, err := sqliteColumns(dbPath, messageTable)
	if err != nil || len(msgCols) == 0 {
		return nil, nil
	}

	transcriptData := fetchMessages(dbPath, messageTable, msgCols, sessionID)
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

// findOpenCodeDB locates the OpenCode SQLite database file. It tries the
// well-known filename first and falls back to scanning for any *.db file in
// (or one level under) the data directory, since OpenCode has used different
// filenames/locations across versions.
func findOpenCodeDB(dataDir string) string {
	candidates := []string{
		filepath.Join(dataDir, "opencode.db"),
		filepath.Join(dataDir, "storage", "opencode.db"),
		filepath.Join(dataDir, "db", "opencode.db"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}

	if matches, _ := filepath.Glob(filepath.Join(dataDir, "*.db")); len(matches) > 0 {
		return matches[0]
	}
	if matches, _ := filepath.Glob(filepath.Join(dataDir, "*", "*.db")); len(matches) > 0 {
		return matches[0]
	}
	return ""
}

// sqliteTables returns all table names in the database.
func sqliteTables(dbPath string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var tables []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			tables = append(tables, line)
		}
	}
	return tables, nil
}

// sqliteColumns returns the column names of a table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// pickTable returns the first table matching an exact name (case-insensitive),
// falling back to the first table whose name contains mustContain but not
// mustNotContain.
func pickTable(tables []string, exact []string, mustContain, mustNotContain string) string {
	for _, t := range tables {
		for _, e := range exact {
			if strings.EqualFold(t, e) {
				return t
			}
		}
	}
	for _, t := range tables {
		lc := strings.ToLower(t)
		if strings.Contains(lc, mustContain) && !strings.Contains(lc, mustNotContain) {
			return t
		}
	}
	return ""
}

// pickColumn returns the first column matching one of the candidate names
// (case-insensitive), in priority order.
func pickColumn(cols []string, candidates ...string) string {
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.EqualFold(c, cand) {
				return c
			}
		}
	}
	return ""
}

// sqlEscape escapes single quotes for use inside a SQLite string literal.
func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// findSessionRow finds the most relevant session row: it prefers an exact
// project id match, then a directory/cwd match, then (if the schema exposes
// neither field) simply the single most recently updated session. Returns
// the session id and its updated-timestamp string (empty if no timestamp
// column was found).
func findSessionRow(dbPath, table, idCol, updatedCol, projectCol, dirCol, projectID, projectPath string) (string, string) {
	orderBy := ""
	if updatedCol != "" {
		orderBy = fmt.Sprintf(" ORDER BY %s DESC", updatedCol)
	}

	selectCols := idCol
	if updatedCol != "" {
		selectCols = idCol + ", " + updatedCol
	}

	tryQuery := func(where string) (string, string) {
		q := fmt.Sprintf("SELECT %s FROM %s", selectCols, table)
		if where != "" {
			q += " WHERE " + where
		}
		q += orderBy + " LIMIT 1;"

		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, q)
		out, err := cmd.Output()
		if err != nil {
			return "", ""
		}
		line := strings.TrimSpace(string(out))
		if line == "" {
			return "", ""
		}
		fields := strings.Split(line, "\t")
		id := strings.TrimSpace(fields[0])
		updated := ""
		if len(fields) > 1 {
			updated = strings.TrimSpace(fields[1])
		}
		return id, updated
	}

	if projectCol != "" {
		if id, updated := tryQuery(fmt.Sprintf("%s='%s'", projectCol, sqlEscape(projectID))); id != "" {
			return id, updated
		}
	}
	if dirCol != "" {
		if id, updated := tryQuery(fmt.Sprintf("%s='%s'", dirCol, sqlEscape(projectPath))); id != "" {
			return id, updated
		}
	}
	if projectCol == "" && dirCol == "" {
		return tryQuery("")
	}
	return "", ""
}

// withinRecentTimeout reports whether a timestamp string (RFC3339, common
// SQLite datetime formats, or epoch seconds/milliseconds/microseconds) is
// within agent.RecentSessionTimeout of now. Unparseable timestamps are
// treated as recent so an unexpected timestamp format doesn't hide an
// otherwise-valid session.
func withinRecentTimeout(ts string) bool {
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, ts); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}
	if n, err := strconv.ParseInt(ts, 10, 64); err == nil {
		var t time.Time
		switch {
		case n > 1_000_000_000_000_000: // microseconds
			t = time.UnixMicro(n)
		case n > 1_000_000_000_000: // milliseconds
			t = time.UnixMilli(n)
		default: // seconds
			t = time.Unix(n, 0)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}
	return true
}

// fetchMessages returns all messages for a session as a JSON array. The
// array is built dynamically from whatever columns the message table
// exposes: if a single JSON blob column is present (e.g. "data") it is used
// directly (with the row id patched in), otherwise a JSON object is built
// from all columns. This avoids depending on one specific schema shape.
func fetchMessages(dbPath, table string, cols []string, sessionID string) []byte {
	sessionIDCol := pickColumn(cols, "session_id", "sessionid")
	if sessionIDCol == "" {
		return nil
	}
	createdCol := pickColumn(cols,
		"time_created", "timecreated", "created_at", "createdat", "created",
		"time_updated", "timeupdated")
	dataCol := pickColumn(cols, "data", "content", "json", "body")
	msgIDCol := pickColumn(cols, "id")

	orderBy := ""
	if createdCol != "" {
		orderBy = fmt.Sprintf(" ORDER BY %s", createdCol)
	}

	var selectExpr string
	switch {
	case dataCol != "" && msgIDCol != "":
		selectExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, msgIDCol)
	case dataCol != "":
		selectExpr = dataCol
	default:
		parts := make([]string, 0, len(cols))
		for _, c := range cols {
			parts = append(parts, fmt.Sprintf("'%s', %s", c, c))
		}
		selectExpr = fmt.Sprintf("json_object(%s)", strings.Join(parts, ", "))
	}

	q := fmt.Sprintf(
		"SELECT json_group_array(%s) FROM %s WHERE %s='%s'%s;",
		selectExpr, table, sessionIDCol, sqlEscape(sessionID), orderBy,
	)

	cmd := exec.Command("sqlite3", dbPath, q)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	data := []byte(strings.TrimSpace(string(out)))
	// sqlite3 returns "[null]" when no rows match
	if len(data) == 0 || string(data) == "[null]" || string(data) == "[]" {
		return nil
	}
	return data
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
