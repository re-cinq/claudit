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
// It walks the session directory (bounded depth) rather than assuming
// session files sit directly at the top level, since OpenCode has changed
// how it buckets session files across releases.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return nil, nil
	}

	if _, err := os.Stat(sessionDir); err != nil {
		return nil, nil
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout
	var bestSessionID string
	var bestModTime time.Time

	const maxDepth = 3
	rootDepth := strings.Count(filepath.Clean(sessionDir), string(filepath.Separator))

	_ = filepath.WalkDir(sessionDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if d.IsDir() {
			if path != sessionDir && strings.Count(filepath.Clean(path), string(filepath.Separator))-rootDepth > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}

		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > recentTimeout {
			return nil
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = strings.TrimSuffix(d.Name(), ".json")
			bestModTime = modTime
		}
		return nil
	})

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

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session belonging to the given project.
//
// OpenCode's on-disk storage has moved between a single shared database and
// per-project databases across releases, and the session/message table
// schemas have shifted too. Rather than hardcoding a layout and column
// names that can silently go stale as OpenCode evolves, this locates the
// database and introspects its schema at runtime, matching on whichever
// project/time/id columns actually exist.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	dbPath := findOpenCodeDB(dataDir, projectID)
	if dbPath == "" {
		return nil, nil
	}

	sessionTable := firstExistingTable(dbPath, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}

	sessionCols := sqliteTableColumns(dbPath, sessionTable)
	idCol := firstColumn(sessionCols, "id", "session_id", "sessionID")
	if idCol == "" {
		return nil, nil
	}
	timeCol := firstColumn(sessionCols, "time_updated", "updated", "time_modified", "modified", "time_created", "created", "mtime", "timestamp")
	projectCol := firstColumn(sessionCols, "project_id", "projectID", "project")
	dirCol := firstColumn(sessionCols, "directory", "cwd", "path")

	var sessionID string
	switch {
	case projectCol != "":
		sessionID = sqliteQuerySession(dbPath, sessionTable, idCol, timeCol, projectCol, projectID)
	case dirCol != "":
		sessionID = sqliteQuerySession(dbPath, sessionTable, idCol, timeCol, dirCol, projectPath)
	default:
		// Unknown or per-project schema: fall back to the most recently
		// touched session in this database.
		sessionID = sqliteQuerySession(dbPath, sessionTable, idCol, timeCol, "", "")
	}
	if sessionID == "" {
		return nil, nil
	}

	if timeCol != "" {
		timeStr := sqliteScalar(dbPath, fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, timeCol, sessionTable, idCol, escapeSQLiteValue(sessionID)))
		if updated, ok := parseOpenCodeTime(timeStr); ok && time.Since(updated) > agent.RecentSessionTimeout {
			return nil, nil
		}
	}

	messageTable := firstExistingTable(dbPath, "message", "messages")
	transcriptData := sqliteMessagesJSON(dbPath, messageTable, sessionID)
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

// findOpenCodeDB locates OpenCode's SQLite database. It checks the known
// shared and per-project locations first, then falls back to a bounded
// search under the data directory for any *.db file, in case a newer
// release relocated it.
func findOpenCodeDB(dataDir, projectID string) string {
	candidates := []string{
		filepath.Join(dataDir, "opencode.db"),
		filepath.Join(dataDir, "project", projectID, "opencode.db"),
		filepath.Join(dataDir, "storage", "opencode.db"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	const maxDepth = 3
	root := filepath.Clean(dataDir)
	rootDepth := strings.Count(root, string(filepath.Separator))

	var best string
	var bestMod time.Time
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != dataDir && strings.Count(filepath.Clean(path), string(filepath.Separator))-rootDepth > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".db") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if best == "" || info.ModTime().After(bestMod) {
			best = path
			bestMod = info.ModTime()
		}
		return nil
	})
	return best
}

// firstExistingTable returns the first candidate name that exists as a
// table in the given SQLite database, or "" if none do.
func firstExistingTable(dbPath string, candidates ...string) string {
	out, err := exec.Command("sqlite3", dbPath, ".tables").Output()
	if err != nil {
		return ""
	}
	tables := make(map[string]bool)
	for _, t := range strings.Fields(string(out)) {
		tables[t] = true
	}
	for _, c := range candidates {
		if tables[c] {
			return c
		}
	}
	return ""
}

// sqliteTableColumns returns the column names of a SQLite table.
func sqliteTableColumns(dbPath, table string) []string {
	out, err := exec.Command("sqlite3", "-separator", "|", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table)).Output()
	if err != nil {
		return nil
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
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

// firstColumn returns the first candidate present in cols, or "" if none are.
func firstColumn(cols []string, candidates ...string) string {
	set := make(map[string]bool, len(cols))
	for _, c := range cols {
		set[c] = true
	}
	for _, c := range candidates {
		if set[c] {
			return c
		}
	}
	return ""
}

// escapeSQLiteValue escapes single quotes for inclusion in a SQL string literal.
func escapeSQLiteValue(v string) string {
	return strings.ReplaceAll(v, "'", "''")
}

// sqliteQuerySession returns the id of the most recently updated session,
// optionally filtered by filterCol=filterVal.
func sqliteQuerySession(dbPath, table, idCol, timeCol, filterCol, filterVal string) string {
	query := fmt.Sprintf("SELECT %s FROM %s", idCol, table)
	if filterCol != "" {
		query += fmt.Sprintf(" WHERE %s='%s'", filterCol, escapeSQLiteValue(filterVal))
	}
	if timeCol != "" {
		query += fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}
	query += " LIMIT 1;"
	return sqliteScalar(dbPath, query)
}

// sqliteScalar runs a query expected to return a single scalar value.
func sqliteScalar(dbPath, query string) string {
	out, err := exec.Command("sqlite3", dbPath, query).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// parseOpenCodeTime parses a timestamp column value in any of the formats
// OpenCode has been observed to use, including Unix seconds/milliseconds.
func parseOpenCodeTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}

	return time.Time{}, false
}

// sqliteMessagesJSON returns the messages for a session as a JSON array,
// discovering the message table's actual column names at runtime.
func sqliteMessagesJSON(dbPath, table, sessionID string) []byte {
	if table == "" {
		return nil
	}

	cols := sqliteTableColumns(dbPath, table)
	if len(cols) == 0 {
		return nil
	}

	idCol := firstColumn(cols, "id", "message_id")
	sessionCol := firstColumn(cols, "session_id", "sessionID", "session")
	dataCol := firstColumn(cols, "data", "content", "body")
	timeCol := firstColumn(cols, "time_created", "created", "time_updated", "updated", "timestamp")
	if idCol == "" || sessionCol == "" || dataCol == "" {
		return nil
	}

	query := fmt.Sprintf(
		`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s'`,
		dataCol, idCol, table, sessionCol, escapeSQLiteValue(sessionID),
	)
	if timeCol != "" {
		query += fmt.Sprintf(" ORDER BY %s", timeCol)
	}
	query += ";"

	out := sqliteScalar(dbPath, query)
	if out == "" || out == "[null]" || out == "[]" {
		return nil
	}
	return []byte(out)
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
