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

	// Fall back to SQLite (OpenCode v1.2+). The database location and its
	// schema have both moved around across OpenCode releases, so probe every
	// known location and introspect the schema at runtime rather than
	// assuming a fixed table/column layout.
	projectID := GetProjectID(projectPath)
	for _, dbPath := range sqliteCandidatePaths(projectPath) {
		session, err := discoverFromSQLite(dbPath, projectID, projectPath)
		if err != nil {
			return nil, err
		}
		if session != nil {
			return session, nil
		}
	}

	return nil, nil
}

// sqliteCandidatePaths returns the OpenCode SQLite database locations to
// probe, in priority order.
func sqliteCandidatePaths(projectPath string) []string {
	var candidates []string

	if dataDir, err := GetDataDir(); err == nil {
		candidates = append(candidates, filepath.Join(dataDir, "opencode.db"))
	}

	candidates = append(candidates, filepath.Join(projectPath, ".opencode", "opencode.db"))

	return candidates
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

// discoverFromSQLite queries an OpenCode SQLite database for the most recent
// session belonging to projectPath. The table/column names are detected at
// runtime via sqlite_master/PRAGMA table_info since they have changed across
// OpenCode releases.
func discoverFromSQLite(dbPath, projectID, projectPath string) (*agent.SessionInfo, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := sqliteFindTable(dbPath, []string{"session", "sessions"})
	if sessionTable == "" {
		return nil, nil
	}

	sessionCols := sqliteTableColumns(dbPath, sessionTable)
	idCol := sqliteFindColumn(sessionCols, []string{"id"})
	if idCol == "" {
		return nil, nil
	}
	projectCol := sqliteFindColumn(sessionCols, []string{
		"project_id", "projectid", "project", "directory", "cwd", "worktree", "path",
	})
	timeCol := sqliteFindColumn(sessionCols, []string{
		"time_updated", "updated_at", "time_updated_at", "updated",
		"time_modified", "modified_at", "time_created", "created_at",
	})

	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}

	var sessionID string
	if projectCol != "" {
		// Different OpenCode versions key sessions either by the computed
		// project ID (root commit hash) or by the raw project directory
		// path — match either.
		query := fmt.Sprintf(
			"SELECT %s FROM %s WHERE %s='%s' OR %s='%s'%s LIMIT 1;",
			idCol, sessionTable,
			projectCol, escapeSQLiteLiteral(projectID),
			projectCol, escapeSQLiteLiteral(projectPath),
			orderClause,
		)
		if out, err := sqliteQuery(dbPath, query); err == nil {
			sessionID = firstLine(out)
		}
	}

	if sessionID == "" {
		// No project scoping available (or no match) — fall back to the
		// most recently updated session in the database.
		query := fmt.Sprintf("SELECT %s FROM %s%s LIMIT 1;", idCol, sessionTable, orderClause)
		out, err := sqliteQuery(dbPath, query)
		if err != nil || strings.TrimSpace(out) == "" {
			return nil, nil
		}
		sessionID = firstLine(out)
	}

	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recently updated (best effort — if we can't
	// determine recency, proceed anyway rather than skip a real session).
	if timeCol != "" {
		timeQuery := fmt.Sprintf(
			"SELECT %s FROM %s WHERE %s='%s';",
			timeCol, sessionTable, idCol, escapeSQLiteLiteral(sessionID),
		)
		if out, err := sqliteQuery(dbPath, timeQuery); err == nil {
			if !isRecentTimestamp(strings.TrimSpace(out)) {
				return nil, nil
			}
		}
	}

	messageTable := sqliteFindTable(dbPath, []string{"message", "messages"})
	if messageTable == "" {
		return &agent.SessionInfo{
			SessionID:   sessionID,
			StartedAt:   time.Now().Format(time.RFC3339),
			ProjectPath: projectPath,
		}, nil
	}

	messageCols := sqliteTableColumns(dbPath, messageTable)
	msgIDCol := sqliteFindColumn(messageCols, []string{"id"})
	sessionRefCol := sqliteFindColumn(messageCols, []string{"session_id", "sessionid", "session"})
	dataCol := sqliteFindColumn(messageCols, []string{"data", "content", "parts", "body"})
	msgOrderCol := sqliteFindColumn(messageCols, []string{"time_created", "created_at", "time", "time_updated", "updated_at"})

	if sessionRefCol == "" || dataCol == "" {
		return &agent.SessionInfo{
			SessionID:   sessionID,
			StartedAt:   time.Now().Format(time.RFC3339),
			ProjectPath: projectPath,
		}, nil
	}

	// Get messages for this session as a JSON array, embedding the row's id
	// into each message object (if available) so the transcript parser can
	// see it, matching the id+data shape it already expects.
	idExpr := dataCol
	if msgIDCol != "" {
		idExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, msgIDCol)
	}
	orderBy := ""
	if msgOrderCol != "" {
		orderBy = fmt.Sprintf(" ORDER BY %s", msgOrderCol)
	}
	msgQuery := fmt.Sprintf(
		"SELECT json_group_array(%s) FROM %s WHERE %s='%s'%s;",
		idExpr, messageTable, sessionRefCol, escapeSQLiteLiteral(sessionID), orderBy,
	)
	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if string(transcriptData) == "[null]" || string(transcriptData) == "[]" || len(transcriptData) == 0 {
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

// sqliteQuery runs a query against an OpenCode SQLite database and returns
// the raw, trimmed stdout.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", "-separator", "|", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sqliteFindTable returns the first candidate table name that exists in the
// database, or "" if none do.
func sqliteFindTable(dbPath string, candidates []string) string {
	out, err := sqliteQuery(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return ""
	}
	existing := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		existing[strings.ToLower(strings.TrimSpace(line))] = true
	}
	for _, c := range candidates {
		if existing[strings.ToLower(c)] {
			return c
		}
	}
	return ""
}

// sqliteTableColumns returns the column names for a table via PRAGMA
// table_info, or nil if the table doesn't exist or has no columns.
func sqliteTableColumns(dbPath, table string) []string {
	out, err := sqliteQuery(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil || out == "" {
		return nil
	}
	var cols []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// sqliteFindColumn returns the first candidate column name present in cols
// (case-insensitive), or "" if none are present.
func sqliteFindColumn(cols []string, candidates []string) string {
	present := make(map[string]string, len(cols))
	for _, c := range cols {
		present[strings.ToLower(c)] = c
	}
	for _, c := range candidates {
		if actual, ok := present[strings.ToLower(c)]; ok {
			return actual
		}
	}
	return ""
}

// escapeSQLiteLiteral escapes a value for embedding in a single-quoted
// SQLite string literal.
func escapeSQLiteLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// firstLine returns the first line of s, trimmed.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// isRecentTimestamp reports whether raw (an RFC3339-ish string or a Unix
// epoch in seconds or milliseconds) is within agent.RecentSessionTimeout of
// now. If raw can't be parsed at all, it returns true — better to proceed
// with a real session than to skip one because of an unrecognized format.
func isRecentTimestamp(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}

	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		t := time.Unix(n, 0)
		if n > 1e12 {
			t = time.UnixMilli(n)
		} else if n > 1e10 {
			t = time.Unix(0, n*int64(time.Millisecond))
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, raw); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	return true
}

// discoverFromSQLiteLegacy is retained for reference of the prior fixed
// schema; unused now that discoverFromSQLite introspects the schema.

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
