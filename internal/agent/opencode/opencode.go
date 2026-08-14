package opencode

import (
	"bytes"
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
//
// OpenCode's on-disk storage has changed shape across releases (flat JSON
// files, a SQLite database, different data-directory locations), and the
// exact table/column names and project-identifier scheme have drifted even
// within the SQLite era. Rather than hard-code one snapshot of that format,
// we try progressively looser strategies and take the first hit:
//  1. Legacy flat-file storage under the global data dir (pre-v1.2).
//  2. A SQLite database, wherever it happens to live, introspected at
//     runtime instead of assuming fixed table/column names.
//  3. The most recently touched session-looking file anywhere under the
//     data dir, as a last resort.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (pre-v1.2 OpenCode)
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}
	projectID := GetProjectID(projectPath)

	// Fall back to SQLite (OpenCode v1.2+). The database has been observed
	// both in the global XDG data directory and in a project-local
	// .opencode directory, depending on version.
	dbCandidates := []string{
		filepath.Join(dataDir, "opencode.db"),
		filepath.Join(projectPath, ".opencode", "opencode.db"),
	}
	for _, dbPath := range dbCandidates {
		if session, err := discoverFromSQLite(dbPath, projectID, projectPath); err == nil && session != nil {
			return session, nil
		}
	}

	// Last resort: the exact flat-file layout or SQLite schema may have
	// changed in ways we don't recognize. Look for the most recently
	// modified session-looking file anywhere under the data directory.
	return discoverFromRecentFiles(dataDir, projectPath)
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

// discoverFromRecentFiles walks the OpenCode data directory looking for the
// most recently modified file whose path suggests it's a session record.
// This is a resilient fallback for when the flat-file directory layout or
// SQLite schema assumptions no longer match the installed OpenCode version.
func discoverFromRecentFiles(dataDir, projectPath string) (*agent.SessionInfo, error) {
	if dataDir == "" {
		return nil, nil
	}
	if _, err := os.Stat(dataDir); err != nil {
		return nil, nil
	}

	now := time.Now()
	var bestPath string
	var bestModTime time.Time

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		lowerPath := strings.ToLower(path)
		if !strings.HasSuffix(lowerPath, ".json") && !strings.HasSuffix(lowerPath, ".jsonl") {
			return nil
		}
		if !strings.Contains(lowerPath, "session") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}

		if bestPath == "" || modTime.After(bestModTime) {
			bestPath = path
			bestModTime = modTime
		}
		return nil
	})

	if bestPath == "" {
		return nil, nil
	}

	sessionID := strings.TrimSuffix(filepath.Base(bestPath), filepath.Ext(bestPath))

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: bestPath,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// discoverFromSQLite queries an OpenCode SQLite database for the most
// recent session. Table and column names are discovered at runtime via
// sqlite_master/PRAGMA introspection rather than assumed, since these have
// changed between OpenCode releases. The project-identifier filter is
// tried first and, if it matches nothing, dropped entirely — the scheme
// used to compute that identifier has also changed across versions, and
// falling back to "most recent session" is far better than silently
// producing no note at all.
func discoverFromSQLite(dbPath, projectID, projectPath string) (*agent.SessionInfo, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, sessionCols, err := findTable(dbPath, "session")
	if err != nil || sessionTable == "" {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	orderCol := pickColumn(sessionCols, "updated", "modified", "time", "created")
	projectCol := pickColumn(sessionCols, "project", "dir", "path", "cwd", "workspace")

	sessionID, orderVal := querySessionRow(dbPath, sessionTable, idCol, orderCol, projectCol, projectID)
	if sessionID == "" && projectCol != "" {
		// The project-identifier scheme may not match this OpenCode
		// version's storage; retry without filtering by it.
		sessionID, orderVal = querySessionRow(dbPath, sessionTable, idCol, orderCol, "", "")
	}
	if sessionID == "" {
		return nil, nil
	}

	if orderVal != "" && !isRecentValue(orderVal) {
		return nil, nil
	}

	transcriptData := dumpMessagesForSession(dbPath, sessionID)

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptData: transcriptData,
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// dumpMessagesForSession finds the message-like table in the database and
// dumps rows belonging to sessionID as a JSON array, using whatever column
// names the table actually has (no assumptions about "content" vs "parts"
// vs "data", etc). Returns "[]" if no matching table/rows are found.
func dumpMessagesForSession(dbPath, sessionID string) []byte {
	empty := []byte("[]")

	msgTable, msgCols, err := findTable(dbPath, "message")
	if err != nil || msgTable == "" {
		return empty
	}

	sessionCol := pickColumn(msgCols, "session_id", "sessionid", "session")
	orderCol := pickColumn(msgCols, "created", "time", "index", "seq", "id")

	query := fmt.Sprintf("SELECT * FROM %s", quoteIdent(msgTable))
	if sessionCol != "" && sessionID != "" {
		query += fmt.Sprintf(" WHERE %s = '%s'", quoteIdent(sessionCol), sqlEscape(sessionID))
	}
	if orderCol != "" {
		query += " ORDER BY " + quoteIdent(orderCol)
	}
	query += ";"

	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return empty
	}
	data := bytes.TrimSpace(out)
	if len(data) == 0 {
		return empty
	}
	return data
}

// querySessionRow selects a single row's id (and, if present, its order
// column value) from table, optionally filtered by filterCol = filterVal.
func querySessionRow(dbPath, table, idCol, orderCol, filterCol, filterVal string) (id, orderVal string) {
	selectCols := quoteIdent(idCol)
	if orderCol != "" {
		selectCols += ", " + quoteIdent(orderCol)
	}

	query := fmt.Sprintf("SELECT %s FROM %s", selectCols, quoteIdent(table))
	if filterCol != "" && filterVal != "" {
		query += fmt.Sprintf(" WHERE %s = '%s'", quoteIdent(filterCol), sqlEscape(filterVal))
	}
	if orderCol != "" {
		query += " ORDER BY " + quoteIdent(orderCol) + " DESC"
	} else {
		query += " ORDER BY rowid DESC"
	}
	query += " LIMIT 1;"

	out, err := sqliteQuery(dbPath, query)
	if err != nil || out == "" {
		return "", ""
	}

	parts := strings.Split(out, "|")
	id = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		orderVal = strings.TrimSpace(parts[1])
	}
	return id, orderVal
}

// findTable finds the table whose name best matches keyword (e.g.
// "session" matches "session" or "sessions") and returns its column names.
func findTable(dbPath, keyword string) (string, []string, error) {
	tables, err := listTables(dbPath)
	if err != nil {
		return "", nil, err
	}

	var best string
	for _, t := range tables {
		lower := strings.ToLower(t)
		if lower == keyword || lower == keyword+"s" {
			best = t
			break
		}
	}
	if best == "" {
		for _, t := range tables {
			if strings.Contains(strings.ToLower(t), keyword) {
				best = t
				break
			}
		}
	}
	if best == "" {
		return "", nil, nil
	}

	cols, err := tableColumns(dbPath, best)
	if err != nil {
		return "", nil, err
	}
	return best, cols, nil
}

// listTables returns the names of all tables in the SQLite database.
func listTables(dbPath string) ([]string, error) {
	out, err := sqliteQuery(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return nil, err
	}

	var tables []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			tables = append(tables, line)
		}
	}
	return tables, nil
}

// tableColumns returns the column names of table via PRAGMA table_info.
func tableColumns(dbPath, table string) ([]string, error) {
	out, err := sqliteQuery(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(table)))
	if err != nil {
		return nil, err
	}

	var cols []string
	for _, line := range strings.Split(out, "\n") {
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

// pickColumn returns the first column in cols matching one of keywords,
// preferring exact (case-insensitive) matches before substring matches.
func pickColumn(cols []string, keywords ...string) string {
	for _, kw := range keywords {
		for _, c := range cols {
			if strings.EqualFold(c, kw) {
				return c
			}
		}
	}
	for _, kw := range keywords {
		lowerKw := strings.ToLower(kw)
		for _, c := range cols {
			if strings.Contains(strings.ToLower(c), lowerKw) {
				return c
			}
		}
	}
	return ""
}

// sqliteQuery runs a single SQL statement against dbPath using the sqlite3
// CLI with "|"-separated list output and returns the trimmed output.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", "-separator", "|", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// quoteIdent quotes a SQL identifier (table/column name) for safe use in a
// generated query.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqlEscape escapes a value for safe use inside a single-quoted SQL string
// literal.
func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// isRecentValue reports whether value (a timestamp in one of several known
// formats, or an epoch integer at second/millisecond/microsecond/nanosecond
// resolution) falls within agent.RecentSessionTimeout of now. If the format
// isn't recognized, it returns true — we'd rather surface a possibly-stale
// session than silently drop a valid one because of a formatting change.
func isRecentValue(value string) bool {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, value); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		var t time.Time
		switch {
		case n > 1e17:
			t = time.Unix(0, n) // nanoseconds
		case n > 1e14:
			t = time.Unix(0, n*1e3) // microseconds
		case n > 1e11:
			t = time.Unix(0, n*1e6) // milliseconds
		default:
			t = time.Unix(n, 0) // seconds
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	return true
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
