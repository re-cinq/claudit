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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session belonging to this project.
//
// OpenCode's SQLite schema (table names, column names, and even the database
// file location) has changed across releases — e.g. session.project_id has
// been renamed/replaced by a directory-based column, and the db file has
// moved relative to the data directory. Rather than hardcoding names that
// drift with every release, we introspect the actual schema at query time via
// sqlite_master / pragma_table_info and adapt to whatever we find.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findOpenCodeDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, sessionCols := introspectTable(dbPath, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project", "directory", "dir", "cwd", "path")
	timeCol := pickColumn(sessionCols, "time_updated", "updated_at", "updatedat", "timeupdated", "time_created", "created_at")

	orderCol := timeCol
	if orderCol == "" {
		orderCol = idCol
	}

	var sessionID string
	if projectCol != "" {
		query := fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s = '%s' OR %s = '%s' ORDER BY %s DESC LIMIT 1;`,
			quoteIdent(idCol), quoteIdent(sessionTable),
			quoteIdent(projectCol), escapeSQLValue(projectID),
			quoteIdent(projectCol), escapeSQLValue(projectPath),
			quoteIdent(orderCol),
		)
		sessionID = sqliteQueryOne(dbPath, query)
	}
	if sessionID == "" {
		// No project-scoping column found, or nothing matched it (e.g. the
		// project identity scheme changed) — fall back to the single most
		// recent session across the database.
		query := fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s DESC LIMIT 1;`,
			quoteIdent(idCol), quoteIdent(sessionTable), quoteIdent(orderCol))
		sessionID = sqliteQueryOne(dbPath, query)
	}
	if sessionID == "" {
		return nil, nil
	}

	// Recency check (best-effort; if we can't determine recency, proceed anyway).
	if timeCol != "" {
		timeVal := sqliteQueryOne(dbPath, fmt.Sprintf(`SELECT %s FROM %s WHERE %s = '%s';`,
			quoteIdent(timeCol), quoteIdent(sessionTable), quoteIdent(idCol), escapeSQLValue(sessionID)))
		if t, ok := parseSQLiteTime(timeVal); ok && time.Since(t) > agent.RecentSessionTimeout {
			return nil, nil
		}
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: buildTranscriptFromSQLite(dbPath, sessionID),
	}, nil
}

// findOpenCodeDB locates the OpenCode SQLite database under the data
// directory. The exact file name/location has moved across releases, so we
// check known locations first and fall back to a shallow search.
func findOpenCodeDB(dataDir string) string {
	candidates := []string{
		filepath.Join(dataDir, "opencode.db"),
		filepath.Join(dataDir, "opencode.sqlite"),
		filepath.Join(dataDir, "db.sqlite"),
		filepath.Join(dataDir, "storage", "opencode.db"),
		filepath.Join(dataDir, "storage", "db.sqlite"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}

	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() {
			rel, relErr := filepath.Rel(dataDir, path)
			if relErr == nil && strings.Count(rel, string(filepath.Separator)) > 2 {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".db") || strings.HasSuffix(d.Name(), ".sqlite") {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// introspectTable finds the first matching table (by name, case-insensitive)
// from a list of candidates and returns its actual name and column names.
func introspectTable(dbPath string, candidates ...string) (string, []string) {
	tables := sqliteQueryLines(dbPath, `SELECT name FROM sqlite_master WHERE type='table';`)
	byLower := make(map[string]string, len(tables))
	for _, t := range tables {
		byLower[strings.ToLower(t)] = t
	}

	var table string
	for _, c := range candidates {
		if actual, ok := byLower[strings.ToLower(c)]; ok {
			table = actual
			break
		}
	}
	if table == "" {
		return "", nil
	}

	cols := sqliteQueryLines(dbPath, fmt.Sprintf(`SELECT name FROM pragma_table_info('%s');`, escapeSQLValue(table)))
	return table, cols
}

// pickColumn returns the actual column name matching one of the candidates,
// preferring exact (case-insensitive) matches and falling back to substring
// matches so renamed-but-similar columns are still found.
func pickColumn(columns []string, candidates ...string) string {
	byLower := make(map[string]string, len(columns))
	for _, c := range columns {
		byLower[strings.ToLower(c)] = c
	}

	for _, cand := range candidates {
		if actual, ok := byLower[strings.ToLower(cand)]; ok {
			return actual
		}
	}
	for _, cand := range candidates {
		lc := strings.ToLower(cand)
		for colLower, actual := range byLower {
			if strings.Contains(colLower, lc) {
				return actual
			}
		}
	}
	return ""
}

// buildTranscriptFromSQLite reads all messages for a session and returns them
// as a JSON array compatible with ParseTranscript. Message table/column names
// are introspected the same way as the session table, and the message body
// column is embedded as JSON (parsed if it's JSON text, quoted otherwise) so
// both "full message object" and "typed parts array" schemas survive.
func buildTranscriptFromSQLite(dbPath, sessionID string) []byte {
	empty := []byte("[]")

	msgTable, msgCols := introspectTable(dbPath, "message", "messages")
	if msgTable == "" {
		return empty
	}

	idCol := pickColumn(msgCols, "id")
	sessionCol := pickColumn(msgCols, "session_id", "sessionid", "session")
	dataCol := pickColumn(msgCols, "data", "content", "parts", "body")
	roleCol := pickColumn(msgCols, "role", "type")
	timeCol := pickColumn(msgCols, "time_created", "created_at", "createdat", "time")

	if sessionCol == "" || dataCol == "" {
		return empty
	}

	var fields []string
	if idCol != "" {
		fields = append(fields, fmt.Sprintf(`'id', %s`, quoteIdent(idCol)))
	}
	if roleCol != "" {
		fields = append(fields, fmt.Sprintf(`'role', %s`, quoteIdent(roleCol)))
	}
	fields = append(fields, fmt.Sprintf(
		`'content', CASE WHEN json_valid(%s) THEN json(%s) ELSE json_quote(%s) END`,
		quoteIdent(dataCol), quoteIdent(dataCol), quoteIdent(dataCol)))
	if timeCol != "" {
		fields = append(fields, fmt.Sprintf(`'time', json_object('created', %s)`, quoteIdent(timeCol)))
	}

	orderCol := timeCol
	if orderCol == "" {
		orderCol = idCol
	}
	if orderCol == "" {
		orderCol = sessionCol
	}

	query := fmt.Sprintf(
		`SELECT json_group_array(json_object(%s)) FROM %s WHERE %s = '%s' ORDER BY %s;`,
		strings.Join(fields, ", "), quoteIdent(msgTable),
		quoteIdent(sessionCol), escapeSQLValue(sessionID), quoteIdent(orderCol),
	)

	out := sqliteQueryOne(dbPath, query)
	if out == "" || out == "null" {
		return empty
	}
	return []byte(out)
}

// sqliteQueryLines runs a query and returns each non-empty output line.
func sqliteQueryLines(dbPath, query string) []string {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// sqliteQueryOne runs a query and returns the first non-empty output line.
func sqliteQueryOne(dbPath, query string) string {
	lines := sqliteQueryLines(dbPath, query)
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

// quoteIdent quotes a SQL identifier (table/column name) for safe interpolation.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// escapeSQLValue escapes a string for use inside a single-quoted SQL literal.
func escapeSQLValue(v string) string {
	return strings.ReplaceAll(v, "'", "''")
}

// parseSQLiteTime parses a time value that may be an ISO-ish string or a
// Unix timestamp in seconds, milliseconds, or microseconds.
func parseSQLiteTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		switch {
		case n > 1e15:
			return time.UnixMicro(n), true
		case n > 1e12:
			return time.UnixMilli(n), true
		case n > 1e9:
			return time.Unix(n, 0), true
		}
	}

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
