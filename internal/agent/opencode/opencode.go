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
// It first tries flat file storage, then falls back to a SQLite-backed
// store used by newer OpenCode releases. OpenCode's project-ID scheme and
// on-disk schema have both changed across releases, so both strategies
// cross-check candidate sessions against projectPath (via a recorded
// "directory" field / column) rather than trusting a single hardcoded
// project-ID derivation.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (older OpenCode releases)
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to a SQLite-backed store (newer OpenCode releases)
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// discoverFromFlatFiles tries the flat-file session discovery strategy.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	if sessionDir, err := GetSessionDir(projectPath); err == nil {
		if info := scanSessionDir(sessionDir, projectPath, false); info != nil {
			return info, nil
		}
	}

	// Fallback: OpenCode's project ID scheme has changed across versions, so
	// our computed project ID may no longer match the directory OpenCode
	// actually wrote sessions under. Scan sibling project directories and
	// match on each session's own "directory" field instead.
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}
	sessionsRoot := filepath.Join(dataDir, "storage", "session")
	projectDirs, err := os.ReadDir(sessionsRoot)
	if err != nil {
		return nil, nil
	}

	var best *agent.SessionInfo
	var bestModTime time.Time
	for _, pd := range projectDirs {
		if !pd.IsDir() {
			continue
		}
		info := scanSessionDir(filepath.Join(sessionsRoot, pd.Name()), projectPath, true)
		if info == nil {
			continue
		}
		modTime, err := time.Parse(time.RFC3339, info.StartedAt)
		if err != nil {
			continue
		}
		if best == nil || modTime.After(bestModTime) {
			best = info
			bestModTime = modTime
		}
	}

	return best, nil
}

// scanSessionDir finds the most recently modified session JSON file in dir
// that was updated within the recency timeout. When requireDirectoryMatch is
// true, a session is only considered when its own "directory" field
// references projectPath; sessions without that field are skipped in that
// mode since dir may not belong to this project at all. When false (the
// exact project-ID directory, which already scopes by project), sessions
// without a recorded directory field are still accepted.
func scanSessionDir(dir, projectPath string, requireDirectoryMatch bool) *agent.SessionInfo {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil
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

		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			continue
		}

		matches, hasDirectory := sessionDirectoryMatches(filepath.Join(dir, entry.Name()), projectPath)
		if requireDirectoryMatch && !hasDirectory {
			continue
		}
		if hasDirectory && !matches {
			continue
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = strings.TrimSuffix(entry.Name(), ".json")
			bestModTime = modTime
		}
	}

	if bestSessionID == "" {
		return nil
	}

	msgDir, _ := GetMessageDir(bestSessionID)
	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// sessionDirectoryMatches reports whether the session file at path records a
// "directory" field, and if so, whether it matches projectPath.
func sessionDirectoryMatches(path, projectPath string) (matches, hasDirectory bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}

	var info sessionInfo
	if err := json.Unmarshal(data, &info); err != nil || info.Directory == "" {
		return false, false
	}

	return filepath.Clean(info.Directory) == filepath.Clean(projectPath), true
}

// discoverFromSQLite queries an OpenCode SQLite database for the most recent
// session. Table and column names are discovered at runtime rather than
// hardcoded, since OpenCode's on-disk schema is an internal implementation
// detail that has changed across releases (renamed columns, added/removed
// tables, etc). A directory-like column is preferred for matching the
// current project, since project-ID schemes have also changed across
// releases.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := sqliteTableName(dbPath, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}
	sessionCols := sqliteColumns(dbPath, sessionTable)
	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	updatedCol := pickColumn(sessionCols, "time_updated", "updated", "time_created", "created")
	dirCol := pickColumn(sessionCols, "directory", "worktree", "cwd", "path")
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project")

	orderClause := ""
	if updatedCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", updatedCol)
	}

	// Prefer matching on a directory-like column: the working directory is a
	// stable way to identify "this project's" session even if the project-ID
	// scheme itself has changed.
	var sessionID string
	if dirCol != "" {
		q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;`,
			idCol, sessionTable, dirCol, escapeSQLite(projectPath), orderClause)
		sessionID = firstSQLiteValue(dbPath, q)
	}
	if sessionID == "" && projectCol != "" {
		q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;`,
			idCol, sessionTable, projectCol, escapeSQLite(projectID), orderClause)
		sessionID = firstSQLiteValue(dbPath, q)
	}
	if sessionID == "" && dirCol == "" && projectCol == "" && orderClause != "" {
		// No column ties a session to a project at all — fall back to the
		// single most recent session, bounded by the recency check below.
		q := fmt.Sprintf(`SELECT %s FROM %s%s LIMIT 1;`, idCol, sessionTable, orderClause)
		sessionID = firstSQLiteValue(dbPath, q)
	}
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout), when we have a
	// timestamp column to check it against.
	if updatedCol != "" {
		q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`,
			updatedCol, sessionTable, idCol, escapeSQLite(sessionID))
		if timeStr := firstSQLiteValue(dbPath, q); timeStr != "" && !isRecentTimestamp(timeStr) {
			return nil, nil
		}
	}

	messageTable := sqliteTableName(dbPath, "message", "messages")
	if messageTable == "" {
		return nil, nil
	}
	messageCols := sqliteColumns(dbPath, messageTable)
	msgIDCol := pickColumn(messageCols, "id")
	msgSessionCol := pickColumn(messageCols, "session_id", "sessionid")
	msgDataCol := pickColumn(messageCols, "data", "content", "value")
	msgTimeCol := pickColumn(messageCols, "time_created", "created", "time_updated", "updated")
	if msgIDCol == "" || msgSessionCol == "" || msgDataCol == "" {
		return nil, nil
	}

	orderMsg := ""
	if msgTimeCol != "" {
		orderMsg = fmt.Sprintf(" ORDER BY %s", msgTimeCol)
	}

	// Get messages for this session as a JSON array
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s'%s;`,
		msgDataCol, msgIDCol, messageTable, msgSessionCol, escapeSQLite(sessionID), orderMsg,
	)
	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
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

// sqliteTableName returns the first existing table name matching one of the
// candidates (case-insensitive exact match first, then substring match), or
// "" if none exist.
func sqliteTableName(dbPath string, candidates ...string) string {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	tables := strings.Split(strings.TrimSpace(string(out)), "\n")

	for _, cand := range candidates {
		for _, t := range tables {
			if strings.EqualFold(t, cand) {
				return t
			}
		}
	}
	for _, cand := range candidates {
		for _, t := range tables {
			if t != "" && strings.Contains(strings.ToLower(t), strings.ToLower(cand)) {
				return t
			}
		}
	}
	return ""
}

// sqliteColumns returns the column names for table, or nil if unavailable.
func sqliteColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 && fields[1] != "" {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// pickColumn returns the first column in cols matching one of the
// candidates (case-insensitive exact match first, then substring match).
func pickColumn(cols []string, candidates ...string) string {
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.EqualFold(c, cand) {
				return c
			}
		}
	}
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.Contains(strings.ToLower(c), strings.ToLower(cand)) {
				return c
			}
		}
	}
	return ""
}

// escapeSQLite escapes single quotes for use in a SQLite string literal.
func escapeSQLite(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// firstSQLiteValue runs query against dbPath and returns the first line of
// output, or "" on error or empty result.
func firstSQLiteValue(dbPath, query string) string {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return ""
	}
	return strings.TrimSpace(strings.Split(trimmed, "\n")[0])
}

// isRecentTimestamp reports whether timeStr, in any of OpenCode's known
// timestamp formats (ISO-8601 variants, or Unix epoch seconds/milliseconds),
// falls within the recent-session timeout. Unparseable input is treated as
// recent — better to try restoring a session than to silently skip one due
// to an unrecognized format.
func isRecentTimestamp(timeStr string) bool {
	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		t := time.UnixMilli(n)
		if n < 1e12 {
			// Value is too small to be milliseconds since epoch; treat as seconds.
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
