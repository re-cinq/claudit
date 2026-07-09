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
// Both discovery paths introspect the on-disk layout/schema rather than assuming
// a fixed shape, since OpenCode has changed its storage internals across releases.
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
// It searches the whole storage tree for session JSON files (not just the
// canonical storage/session/<projectID> path) since newer OpenCode versions
// have nested project data under an extra "project/<id>" segment.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	storageDir := filepath.Join(dataDir, "storage")
	if info, err := os.Stat(storageDir); err != nil || !info.IsDir() {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout

	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		// Only consider files that live under a "session" directory component.
		if !strings.Contains(filepath.ToSlash(path), "/session") {
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

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var sess sessionInfo
		if err := json.Unmarshal(data, &sess); err != nil {
			return nil
		}

		matches := false
		switch {
		case sess.ProjectID != "" && sess.ProjectID == projectID:
			matches = true
		case sess.Directory != "" && sess.Directory == projectPath:
			matches = true
		case sess.ProjectID == "" && sess.Directory == "" && filepath.Base(filepath.Dir(path)) == projectID:
			// Legacy layout: storage/session/<projectID>/<sessionID>.json
			matches = true
		}
		if !matches {
			return nil
		}

		sessionID := sess.ID
		if sessionID == "" {
			sessionID = strings.TrimSuffix(d.Name(), ".json")
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = sessionID
			bestModTime = modTime
		}
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	msgDir := findMessageDir(dataDir, bestSessionID)
	if msgDir == "" {
		msgDir, _ = GetMessageDir(bestSessionID)
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// findMessageDir locates the message directory for a session anywhere under
// dataDir, falling back to the canonical storage/message/<sessionID> path.
func findMessageDir(dataDir, sessionID string) string {
	canonical := filepath.Join(dataDir, "storage", "message", sessionID)
	if info, err := os.Stat(canonical); err == nil && info.IsDir() {
		return canonical
	}

	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || !d.IsDir() {
			return nil
		}
		if d.Name() == sessionID && strings.Contains(filepath.ToSlash(filepath.Dir(path)), "/message") {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
// The database location and schema are introspected rather than assumed fixed, since
// column names and file placement have changed across OpenCode releases.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findSQLiteDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionCols, err := sqliteTableColumns(dbPath, "session")
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := pickColumn(sessionCols,
		"time_updated", "updated_at", "timeupdated", "updated",
		"time_created", "created_at", "timecreated", "created")
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project")
	dirCol := pickColumn(sessionCols, "directory", "cwd", "path", "worktree", "dir")

	var where string
	switch {
	case projectCol != "":
		where = fmt.Sprintf("%s = '%s'", projectCol, sqlEscape(projectID))
	case dirCol != "":
		where = fmt.Sprintf("%s = '%s'", dirCol, sqlEscape(projectPath))
	}

	orderBy := idCol + " DESC"
	if timeCol != "" {
		orderBy = timeCol + " DESC"
	}

	// Find most recent session for this project
	sessionQuery := fmt.Sprintf("SELECT %s FROM session", idCol)
	if where != "" {
		sessionQuery += " WHERE " + where
	}
	sessionQuery += fmt.Sprintf(" ORDER BY %s LIMIT 1;", orderBy)

	sessionID, err := sqliteQuery(dbPath, sessionQuery)
	if err != nil || sessionID == "" {
		// Retry without the project filter in case the matching column
		// holds a value in a different shape than we expect (e.g. a
		// directory alias instead of an absolute path).
		if where == "" {
			return nil, nil
		}
		fallbackQuery := fmt.Sprintf("SELECT %s FROM session ORDER BY %s LIMIT 1;", idCol, orderBy)
		sessionID, err = sqliteQuery(dbPath, fallbackQuery)
		if err != nil || sessionID == "" {
			return nil, nil
		}
	}

	// Check if this session was recent (within timeout)
	if timeCol != "" {
		timeQuery := fmt.Sprintf("SELECT %s FROM session WHERE %s = '%s';", timeCol, idCol, sqlEscape(sessionID))
		if timeOutput, err := sqliteQuery(dbPath, timeQuery); err == nil && timeOutput != "" {
			if !isRecentTimestamp(timeOutput) {
				return nil, nil
			}
		}
		// If we can't read/parse the time, proceed anyway — better to try than skip.
	}

	transcriptData := readSQLiteMessages(dbPath, sessionID)

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// readSQLiteMessages fetches the messages for a session as a JSON array,
// adapting to whatever column names the message table actually has.
func readSQLiteMessages(dbPath, sessionID string) []byte {
	messageCols, err := sqliteTableColumns(dbPath, "message")
	if err != nil || len(messageCols) == 0 {
		return nil
	}

	sessionLinkCol := pickColumn(messageCols, "session_id", "sessionid", "session")
	dataCol := pickColumn(messageCols, "data", "content", "body", "message")
	if sessionLinkCol == "" || dataCol == "" {
		return nil
	}
	msgIDCol := pickColumn(messageCols, "id")
	msgTimeCol := pickColumn(messageCols, "time_created", "created_at", "timecreated", "created", "time_updated", "updated_at")

	selectExpr := dataCol
	if msgIDCol != "" {
		selectExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, msgIDCol)
	}
	orderExpr := "rowid"
	if msgTimeCol != "" {
		orderExpr = msgTimeCol
	}

	msgQuery := fmt.Sprintf(
		"SELECT json_group_array(%s) FROM message WHERE %s = '%s' ORDER BY %s;",
		selectExpr, sessionLinkCol, sqlEscape(sessionID), orderExpr,
	)
	out, err := sqliteQuery(dbPath, msgQuery)
	if err != nil {
		return nil
	}
	out = strings.TrimSpace(out)
	// sqlite3 returns "[null]" when no rows match
	if out == "" || out == "[null]" || out == "[]" {
		return nil
	}
	return []byte(out)
}

// findSQLiteDB locates OpenCode's SQLite database. It prefers the canonical
// opencode.db at the data dir root, but falls back to searching the whole
// data dir tree in case the file has moved or been renamed in newer versions.
func findSQLiteDB(dataDir string) string {
	primary := filepath.Join(dataDir, "opencode.db")
	if info, err := os.Stat(primary); err == nil && !info.IsDir() {
		return primary
	}

	var found string
	var foundMod time.Time
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		name := strings.ToLower(d.Name())
		if !strings.HasSuffix(name, ".db") && !strings.HasSuffix(name, ".sqlite") && !strings.HasSuffix(name, ".sqlite3") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if found == "" || info.ModTime().After(foundMod) {
			found = path
			foundMod = info.ModTime()
		}
		return nil
	})
	return found
}

// sqliteQuery runs a single query against the database and returns trimmed output.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sqliteTableColumns returns the set of column names (lowercased) for a table,
// or an empty map if the table does not exist.
func sqliteTableColumns(dbPath, table string) (map[string]bool, error) {
	out, err := sqliteQuery(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return nil, err
	}
	cols := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) >= 2 {
			cols[strings.ToLower(parts[1])] = true
		}
	}
	return cols, nil
}

// pickColumn returns the first candidate (in the caller's original casing)
// that exists in cols, checked case-insensitively. SQLite resolves unquoted
// ASCII identifiers case-insensitively, so the returned name is safe to use
// verbatim in a query regardless of the schema's actual casing.
func pickColumn(cols map[string]bool, candidates ...string) string {
	for _, c := range candidates {
		if cols[strings.ToLower(c)] {
			return c
		}
	}
	return ""
}

// sqlEscape escapes single quotes for embedding a value in a SQL string literal.
func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// isRecentTimestamp parses a timestamp in any of several formats OpenCode
// might use (RFC3339 variants, space-separated, or epoch seconds/millis) and
// reports whether it falls within the recent-session timeout. Unparseable
// values are treated as recent — better to try than to silently skip.
func isRecentTimestamp(s string) bool {
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		var t time.Time
		switch {
		case n > 1e15: // microseconds
			t = time.UnixMicro(n)
		case n > 1e12: // milliseconds
			t = time.UnixMilli(n)
		default: // seconds
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
