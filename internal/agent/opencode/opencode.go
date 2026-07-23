```go
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

// discoverFromFlatFiles tries the legacy flat file session discovery. It first
// checks the project-scoped directory (storage/session/<projectID>/*.json,
// used by OpenCode pre-v1.2). If that finds nothing, it falls back to a
// recursive scan of storage/session for a session file that references this
// project via its "directory" or "projectID" field. The recursive scan exists
// because newer OpenCode releases have changed how sessions are laid out on
// disk (e.g. no longer nesting them under a project-ID directory), and
// scanning by content rather than assuming a fixed path keeps discovery
// working across those layout changes.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	if session, err := a.discoverFromProjectDir(projectPath); err != nil {
		return nil, err
	} else if session != nil {
		return session, nil
	}
	return a.discoverFromSessionScan(projectPath)
}

// discoverFromProjectDir looks for session files directly under
// storage/session/<projectID>/.
func (a *Agent) discoverFromProjectDir(projectPath string) (*agent.SessionInfo, error) {
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

// discoverFromSessionScan recursively walks storage/session looking for a
// session JSON file whose "directory" or "projectID" field matches this
// project, regardless of where in the tree it lives. This covers layouts
// where sessions are stored flat (storage/session/<sessionID>.json) or under
// a different grouping than a project-ID directory.
func (a *Agent) discoverFromSessionScan(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionsRoot := filepath.Join(dataDir, "storage", "session")
	projectID := GetProjectID(projectPath)

	now := time.Now()
	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(sessionsRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
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
		if bestSessionID != "" && !modTime.After(bestModTime) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var sess sessionInfo
		if err := json.Unmarshal(data, &sess); err != nil {
			return nil
		}

		matches := (sess.Directory != "" && agent.PathsEqual(sess.Directory, projectPath)) ||
			(sess.ProjectID != "" && sess.ProjectID == projectID)
		if !matches {
			return nil
		}

		id := sess.ID
		if id == "" {
			id = strings.TrimSuffix(d.Name(), ".json")
		}

		bestSessionID = id
		bestModTime = modTime
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	msgDir, _ := GetMessageDir(bestSessionID)

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session belonging to this project. Column names are introspected via
// PRAGMA table_info rather than hardcoded, so this keeps working across
// OpenCode releases that rename or restructure the session/message tables
// (e.g. moving from a project_id column to a directory-based column).
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionCols := sqliteTableColumns(dbPath, "session")
	idCol := sqliteFirstExistingColumn(sessionCols, "id")
	if idCol == "" {
		// Unknown/legacy schema shape we can't introspect - try the
		// original hardcoded query as a last resort.
		return discoverFromSQLiteLegacy(dbPath, projectID, projectPath)
	}

	scopeCol, scopeVal := sqliteScopeColumn(sessionCols, projectID, projectPath)
	timeCol := sqliteFirstExistingColumn(sessionCols,
		"time_updated", "timeUpdated", "updated", "updated_at",
		"time_created", "timeCreated", "created", "created_at")

	var where string
	if scopeCol != "" {
		where = fmt.Sprintf("WHERE %s='%s'", scopeCol, escapeSQLiteString(scopeVal))
	}
	orderBy := ""
	if timeCol != "" {
		orderBy = fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}

	sessionQuery := fmt.Sprintf("SELECT %s FROM session %s%s LIMIT 1;", idCol, where, orderBy)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		if scopeCol != "" {
			// The scoping column may hold values in a format we didn't
			// anticipate (e.g. a path when we expected a hash); retry
			// unscoped rather than giving up on discovery entirely.
			sessionQuery = fmt.Sprintf("SELECT %s FROM session%s LIMIT 1;", idCol, orderBy)
			cmd = exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
			sessionOutput, err = cmd.Output()
		}
		if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
			return nil, nil
		}
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	if timeCol != "" {
		timeQuery := fmt.Sprintf("SELECT %s FROM session WHERE %s='%s';", timeCol, idCol, escapeSQLiteString(sessionID))
		cmd = exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			if !isRecentTimestamp(strings.TrimSpace(string(timeOutput))) {
				return nil, nil
			}
		}
	}

	transcriptData := sqliteMessagesForSession(dbPath, sessionID)
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

// discoverFromSQLiteLegacy is the original OpenCode v1.2-era query, used when
// the session table's schema can't be introspected (e.g. sqlite3 doesn't
// support -json, or PRAGMA table_info returns nothing usable).
func discoverFromSQLiteLegacy(dbPath, projectID, projectPath string) (*agent.SessionInfo, error) {
	sessionQuery := fmt.Sprintf(
		`SELECT id FROM session WHERE project_id='%s' ORDER BY time_updated DESC LIMIT 1;`,
		escapeSQLiteString(projectID),
	)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	timeQuery := fmt.Sprintf(`SELECT time_updated FROM session WHERE id='%s';`, escapeSQLiteString(sessionID))
	cmd = exec.Command("sqlite3", dbPath, timeQuery)
	if timeOutput, err := cmd.Output(); err == nil {
		if !isRecentTimestamp(strings.TrimSpace(string(timeOutput))) {
			return nil, nil
		}
	}

	transcriptData := sqliteMessagesForSession(dbPath, sessionID)
	if len(transcriptData) == 0 {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "",
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// sqliteMessagesForSession returns the messages for a session as a JSON
// array, adapting to the message table's actual column names.
func sqliteMessagesForSession(dbPath, sessionID string) []byte {
	msgCols := sqliteTableColumns(dbPath, "message")

	sessionRefCol := sqliteFirstExistingColumn(msgCols, "session_id", "sessionID", "session")
	dataCol := sqliteFirstExistingColumn(msgCols, "data", "content", "message")
	msgIDCol := sqliteFirstExistingColumn(msgCols, "id")
	msgTimeCol := sqliteFirstExistingColumn(msgCols, "time_created", "timeCreated", "created", "created_at")

	if sessionRefCol == "" || dataCol == "" {
		// Fall back to the legacy assumed schema.
		sessionRefCol = "session_id"
		dataCol = "data"
		msgIDCol = "id"
		msgTimeCol = "time_created"
	}

	idExpr := dataCol
	if msgIDCol != "" {
		idExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, msgIDCol)
	}
	msgOrder := ""
	if msgTimeCol != "" {
		msgOrder = fmt.Sprintf(" ORDER BY %s", msgTimeCol)
	}

	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM message WHERE %s='%s'%s;`,
		idExpr, sessionRefCol, escapeSQLiteString(sessionID), msgOrder,
	)
	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if string(transcriptData) == "[null]" || string(transcriptData) == "[]" || len(transcriptData) == 0 {
		return nil
	}

	return transcriptData
}

// sqliteTableColumns returns the column names of a SQLite table, or nil if
// the table doesn't exist or can't be introspected.
func sqliteTableColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", "-json", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(output, &rows); err != nil {
		return nil
	}

	cols := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Name != "" {
			cols = append(cols, r.Name)
		}
	}
	return cols
}

// sqliteFirstExistingColumn returns the first candidate column name present
// in cols, or "" if none match.
func sqliteFirstExistingColumn(cols []string, candidates ...string) string {
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

// sqliteScopeColumn picks the column used to scope sessions to a project and
// the value to filter on. It prefers a directory/path-style column matched
// against the working directory, falling back to a project-id-style column
// matched against the git-root-derived project ID.
func sqliteScopeColumn(cols []string, projectID, projectPath string) (col, val string) {
	if c := sqliteFirstExistingColumn(cols, "directory", "path", "cwd", "worktree"); c != "" {
		return c, projectPath
	}
	if c := sqliteFirstExistingColumn(cols, "project_id", "projectID", "project"); c != "" {
		return c, projectID
	}
	return "", ""
}

// escapeSQLiteString escapes single quotes for safe embedding in a SQL
// string literal.
func escapeSQLiteString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// isRecentTimestamp reports whether a timestamp string, in any of the
// formats OpenCode has used for session timestamps, falls within the
// recent-session window. Unparseable timestamps are treated as recent so
// discovery isn't blocked by an unrecognized timestamp format.
func isRecentTimestamp(timeStr string) bool {
	if timeStr == "" {
		return true
	}

	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	// Some schemas store timestamps as integer epoch seconds or
	// milliseconds rather than formatted strings.
	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		t := time.UnixMilli(n)
		if t.Year() < 2001 || t.Year() > 2100 {
			t = time.Unix(n, 0)
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
```
