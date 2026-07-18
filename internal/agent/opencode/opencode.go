package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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
//
// OpenCode's on-disk layout has changed across releases (the project is
// updated very frequently), so the project-scoped directory computed from
// GetSessionDir is tried first, but if that yields nothing we fall back to
// scanning the wider session storage tree and matching each session file's
// recorded working directory against projectPath. This keeps discovery
// working even if OpenCode's project-id scheme or directory nesting changes
// underneath us.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout

	// 1. Known layout: storage/session/<projectID>/<sessionID>.json
	sessionDir, err := GetSessionDir(projectPath)
	if err == nil {
		if id, modTime := latestSessionInDir(sessionDir, now, recentTimeout); id != "" {
			return flatFileSessionInfo(id, modTime, projectPath), nil
		}
	}

	// 2. Alternate known layout used by some versions:
	//    project/<projectID>/storage/session/<sessionID>.json
	projectID := GetProjectID(projectPath)
	altDir := filepath.Join(dataDir, "project", projectID, "storage", "session")
	if id, modTime := latestSessionInDir(altDir, now, recentTimeout); id != "" {
		return flatFileSessionInfo(id, modTime, projectPath), nil
	}

	// 3. Unknown/changed layout: scan the session storage tree and match by
	//    the session's own recorded directory instead of trusting our
	//    computed project id.
	for _, root := range []string{
		filepath.Join(dataDir, "storage", "session"),
		filepath.Join(dataDir, "project"),
	} {
		if id, modTime := scanForProjectSession(root, projectPath, now, recentTimeout); id != "" {
			return flatFileSessionInfo(id, modTime, projectPath), nil
		}
	}

	return nil, nil
}

// latestSessionInDir returns the id and mtime of the most recently modified
// *.json file directly inside dir, within timeout. Returns "" if none found
// or dir doesn't exist.
func latestSessionInDir(dir string, now time.Time, timeout time.Duration) (string, time.Time) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}
	}

	var bestID string
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
		if now.Sub(modTime) > timeout {
			continue
		}

		if bestID == "" || modTime.After(bestModTime) {
			bestID = strings.TrimSuffix(entry.Name(), ".json")
			bestModTime = modTime
		}
	}

	return bestID, bestModTime
}

// scanForProjectSession walks a session storage root (which may or may not
// be scoped by a project subdirectory, depending on the OpenCode version)
// looking for the most recently modified session file whose recorded
// directory matches projectPath.
func scanForProjectSession(root, projectPath string, now time.Time, timeout time.Duration) (string, time.Time) {
	var bestID string
	var bestModTime time.Time

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// Don't descend into message/part storage - session info files
			// live alongside them, not inside them.
			if d.Name() == "message" || d.Name() == "part" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		modTime := info.ModTime()
		if now.Sub(modTime) > timeout {
			return nil
		}
		if bestID != "" && !modTime.After(bestModTime) {
			return nil
		}

		data, rerr := os.ReadFile(path)
		if rerr != nil || !sessionMatchesProject(data, projectPath) {
			return nil
		}

		bestID = strings.TrimSuffix(d.Name(), ".json")
		bestModTime = modTime
		return nil
	})

	return bestID, bestModTime
}

// sessionMatchesProject reports whether a session JSON file's recorded
// working directory matches projectPath, trying the field names OpenCode
// has used across versions.
func sessionMatchesProject(data []byte, projectPath string) bool {
	var probe struct {
		Directory string `json:"directory"`
		Path      string `json:"path"`
		Cwd       string `json:"cwd"`
		Worktree  string `json:"worktree"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}

	for _, candidate := range []string{probe.Directory, probe.Path, probe.Cwd, probe.Worktree} {
		if candidate != "" && samePath(candidate, projectPath) {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return filepath.Clean(absA) == filepath.Clean(absB)
}

func flatFileSessionInfo(sessionID string, modTime time.Time, projectPath string) *agent.SessionInfo {
	msgDir, _ := GetMessageDir(sessionID)
	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: msgDir,
		StartedAt:      modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
//
// The exact table/column names are not treated as fixed: OpenCode's SQLite
// schema is introspected via PRAGMA table_info so this keeps working across
// schema changes (e.g. project_id being renamed, or projects being
// identified by directory instead of a computed project id).
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionID := findRecentSQLiteSession(dbPath, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout), if we can find a
	// suitable timestamp column. If we can't, proceed anyway — better to
	// try than to silently skip a valid session.
	sessionCols := sqliteColumns(dbPath, "session")
	if timeCol := firstExistingColumn(sessionCols, "time_updated", "updated", "time_created", "created"); timeCol != "" {
		timeQuery := fmt.Sprintf(
			`SELECT %s FROM session WHERE id='%s';`,
			timeCol, escapeSQL(sessionID),
		)
		cmd := exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			timeStr := strings.TrimSpace(string(timeOutput))
			if t, err := time.Parse(time.RFC3339Nano, timeStr); err == nil {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			} else if t, err := time.Parse("2006-01-02T15:04:05.000Z", timeStr); err == nil {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			} else if t, err := time.Parse("2006-01-02 15:04:05", timeStr); err == nil {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
			// If we can't parse the time, proceed anyway.
		}
	}

	transcriptData := fetchSQLiteMessages(dbPath, sessionID)
	if transcriptData == nil {
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

// findRecentSQLiteSession finds the most recent session id for a project,
// trying the historically-known project_id column first and falling back to
// matching by a directory/path-like column when the schema doesn't have one.
func findRecentSQLiteSession(dbPath, projectID, projectPath string) string {
	cols := sqliteColumns(dbPath, "session")
	if len(cols) == 0 {
		return ""
	}

	orderClause := ""
	if orderCol := firstExistingColumn(cols, "time_updated", "updated", "time_created", "created"); orderCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", orderCol)
	}

	if hasColumn(cols, "project_id") {
		query := fmt.Sprintf(
			`SELECT id FROM session WHERE project_id='%s'%s LIMIT 1;`,
			escapeSQL(projectID), orderClause,
		)
		if id := querySQLiteValue(dbPath, query); id != "" {
			return id
		}
	}

	for _, col := range []string{"directory", "path", "cwd", "worktree"} {
		if !hasColumn(cols, col) {
			continue
		}
		query := fmt.Sprintf(
			`SELECT id FROM session WHERE %s='%s'%s LIMIT 1;`,
			col, escapeSQL(projectPath), orderClause,
		)
		if id := querySQLiteValue(dbPath, query); id != "" {
			return id
		}
	}

	return ""
}

// fetchSQLiteMessages retrieves all messages for a session as a JSON array,
// introspecting the message table's columns rather than assuming fixed names.
// Returns nil if no usable columns/messages were found.
func fetchSQLiteMessages(dbPath, sessionID string) []byte {
	cols := sqliteColumns(dbPath, "message")
	sessionCol := firstExistingColumn(cols, "session_id", "sessionID", "session")
	dataCol := firstExistingColumn(cols, "data", "content", "json", "body")
	if sessionCol == "" || dataCol == "" {
		return nil
	}

	selectExpr := dataCol
	if idCol := firstExistingColumn(cols, "id", "message_id", "messageID"); idCol != "" {
		selectExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, idCol)
	}

	orderClause := ""
	if timeCol := firstExistingColumn(cols, "time_created", "created", "time"); timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s", timeCol)
	}

	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM message WHERE %s='%s'%s;`,
		selectExpr, sessionCol, escapeSQL(sessionID), orderClause,
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

// sqliteColumns returns the column names of a table via PRAGMA table_info,
// or nil if the table doesn't exist / sqlite3 fails.
func sqliteColumns(dbPath, table string) []string {
	if !isValidSQLIdentifier(table) {
		return nil
	}
	cmd := exec.Command("sqlite3", "-separator", "|", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
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

// isValidSQLIdentifier restricts table names used to build queries to a safe
// character set, since they can't be parameterized as SQL values.
func isValidSQLIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func hasColumn(cols []string, name string) bool {
	return firstExistingColumn(cols, name) != ""
}

// firstExistingColumn returns the first candidate that exists in cols
// (case-insensitive), or "" if none match.
func firstExistingColumn(cols []string, candidates ...string) string {
	for _, candidate := range candidates {
		for _, c := range cols {
			if strings.EqualFold(c, candidate) {
				return c
			}
		}
	}
	return ""
}

func querySQLiteValue(dbPath, query string) string {
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// escapeSQL escapes single quotes for safe interpolation into a SQL string
// literal (sqlite3 CLI doesn't support parameterized queries here).
func escapeSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
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
