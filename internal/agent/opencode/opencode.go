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
// It first tries flat file storage (pre-v1.2), then falls back to SQLite
// (v1.2+). Newer OpenCode releases have moved storage around and renamed
// columns between minor versions, so the SQLite database location and
// schema are both discovered dynamically rather than assumed.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (pre-v1.2 OpenCode)
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	projectID := GetProjectID(projectPath)

	// Some OpenCode releases keep a project-local database alongside the
	// repo (<project>/.opencode/opencode.db) rather than the global XDG
	// data directory. Try that first since it's the more specific location.
	localDBPath := filepath.Join(projectPath, ".opencode", "opencode.db")
	if session, err := discoverFromSQLite(localDBPath, projectID, projectPath); session != nil || err != nil {
		return session, err
	}

	// Fall back to the global XDG data directory (OpenCode v1.2+).
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}
	globalDBPath := filepath.Join(dataDir, "opencode.db")
	return discoverFromSQLite(globalDBPath, projectID, projectPath)
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

// discoverFromSQLite queries an OpenCode SQLite database for the most
// recent session belonging to projectID/projectPath. The table and column
// names are discovered at runtime via sqlite_master/PRAGMA table_info
// instead of being hardcoded, since these have changed between OpenCode
// releases (e.g. session/message vs sessions/messages, project_id vs a
// directory-based column, time_updated vs updated_at).
func discoverFromSQLite(dbPath, projectID, projectPath string) (*agent.SessionInfo, error) {
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
	sessionCols := sqliteTableColumns(dbPath, sessionTable)
	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}

	projectCol, projectColUsesPath := pickProjectColumn(sessionCols)
	orderCol := pickColumn(sessionCols,
		"time_updated", "updated_at", "updatedat", "time_created", "created_at", "createdat", "mtime")

	orderExpr := "rowid"
	if orderCol != "" {
		orderExpr = orderCol
	}

	buildSessionQuery := func(withFilter bool) string {
		where := ""
		if withFilter && projectCol != "" {
			val := projectID
			if projectColUsesPath {
				val = projectPath
			}
			where = fmt.Sprintf(" WHERE %s=%s", projectCol, sqlQuote(val))
		}
		return fmt.Sprintf(`SELECT %s FROM %s%s ORDER BY %s DESC LIMIT 1;`, idCol, sessionTable, where, orderExpr)
	}

	// Find most recent session for this project. If filtering by the
	// guessed project column/value comes up empty, retry unfiltered rather
	// than silently giving up (better to try than skip).
	sessionOutput, err := sqliteQuery(dbPath, buildSessionQuery(true))
	if err != nil || strings.TrimSpace(sessionOutput) == "" {
		sessionOutput, err = sqliteQuery(dbPath, buildSessionQuery(false))
	}
	if err != nil || strings.TrimSpace(sessionOutput) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(sessionOutput)

	// Check if this session was recent (within timeout). Best effort: if we
	// can't find/parse a timestamp column, proceed anyway.
	if orderCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s=%s;`, orderCol, sessionTable, idCol, sqlQuote(sessionID))
		if timeOutput, err := sqliteQuery(dbPath, timeQuery); err == nil {
			if t, ok := parseFlexibleOpenCodeTime(strings.TrimSpace(timeOutput)); ok {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
		}
	}

	messageTable := findSQLiteTable(dbPath, "message", "messages")
	if messageTable == "" {
		return nil, nil
	}
	messageCols := sqliteTableColumns(dbPath, messageTable)
	sessionCol := pickColumn(messageCols, "session_id", "sessionid", "sessionID")
	if sessionCol == "" {
		return nil, nil
	}
	msgOrderCol := pickColumn(messageCols,
		"time_created", "created_at", "createdat", "time", "time_updated", "updated_at", "mtime")

	msgOrderExpr := "rowid"
	if msgOrderCol != "" {
		msgOrderExpr = msgOrderCol
	}

	// Dump each message row as-is (whatever columns it actually has) rather
	// than assuming a specific "data"/"parts"/"content" column, so this
	// keeps working across message schema changes too.
	msgQuery := fmt.Sprintf(`SELECT * FROM %s WHERE %s=%s ORDER BY %s;`, messageTable, sessionCol, sqlQuote(sessionID), msgOrderExpr)
	msgOutput, err := sqliteQueryJSON(dbPath, msgQuery)
	if err != nil {
		return nil, nil
	}

	transcriptData := strings.TrimSpace(msgOutput)
	if transcriptData == "" || transcriptData == "[]" || transcriptData == "null" || transcriptData == "[null]" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: []byte(transcriptData),
	}, nil
}

// sqliteQuery runs a query against dbPath and returns its raw stdout.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// sqliteQueryJSON runs a query against dbPath and returns its stdout as
// JSON (each row becomes a JSON object using its actual column names).
func sqliteQueryJSON(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// findSQLiteTable returns the first candidate name that exists as a table
// in dbPath, or "" if none of them do.
func findSQLiteTable(dbPath string, candidates ...string) string {
	for _, name := range candidates {
		if !isSafeSQLIdent(name) {
			continue
		}
		query := fmt.Sprintf(`SELECT name FROM sqlite_master WHERE type='table' AND name=%s LIMIT 1;`, sqlQuote(name))
		out, err := sqliteQuery(dbPath, query)
		if err == nil && strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out)
		}
	}
	return ""
}

// sqliteTableColumns returns the column names for table in dbPath.
func sqliteTableColumns(dbPath, table string) []string {
	if !isSafeSQLIdent(table) {
		return nil
	}
	out, err := sqliteQuery(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) >= 2 && isSafeSQLIdent(fields[1]) {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// pickColumn returns the actual column name (preserving its casing) that
// case-insensitively matches the first matching candidate, or "".
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

// pickProjectColumn finds the column used to scope sessions to a project.
// usesPath reports whether the column stores a filesystem path/cwd (match
// against projectPath) rather than a project ID hash (match against
// projectID).
func pickProjectColumn(cols []string) (col string, usesPath bool) {
	if c := pickColumn(cols, "project_id", "projectid", "projectID"); c != "" {
		return c, false
	}
	if c := pickColumn(cols, "directory", "cwd", "path", "worktree", "project_path", "projectpath", "project_dir"); c != "" {
		return c, true
	}
	return "", false
}

// isSafeSQLIdent reports whether s is safe to interpolate unquoted into a
// SQL statement as an identifier (used for table/column names sourced from
// the database's own schema, which can't be passed as bound parameters via
// the sqlite3 CLI).
func isSafeSQLIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// sqlQuote quotes and escapes s as a SQL string literal.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// parseFlexibleOpenCodeTime parses a timestamp value that may be an
// RFC3339-ish string or a Unix epoch integer (seconds, milliseconds, or
// microseconds), returning ok=false if the format isn't recognized.
func parseFlexibleOpenCodeTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}

	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
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
