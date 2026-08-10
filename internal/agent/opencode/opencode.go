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
// OpenCode's on-disk session format has changed across versions (flat JSON
// files nested by project, flat JSON files with in-record project fields,
// and a SQLite database with varying column names), so each strategy is
// tried in turn rather than assuming one fixed layout.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (covers both older and newer OpenCode)
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite (some OpenCode versions store sessions in opencode.db)
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// sessionFileInfo describes a discovered OpenCode session file.
type sessionFileInfo struct {
	sessionID string
	modTime   time.Time
}

// discoverFromFlatFiles tries flat file session discovery, supporting both
// the older layout (sessions nested in a per-project directory named after
// the project ID) and newer layouts (sessions stored flat, with the project
// recorded as a field inside each session file instead of via directory
// nesting).
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)

	// Layout 1: sessions nested under a per-project directory.
	nestedDir := filepath.Join(dataDir, "storage", "session", projectID)
	if info := mostRecentSessionFile(nestedDir); info != nil {
		return flatFileSessionInfo(info, projectPath), nil
	}

	// Layout 2: sessions stored flat; match project via fields recorded
	// inside each session file rather than by directory nesting.
	flatDir := filepath.Join(dataDir, "storage", "session")
	if info := mostRecentMatchingSessionFile(flatDir, projectID, projectPath); info != nil {
		return flatFileSessionInfo(info, projectPath), nil
	}

	return nil, nil
}

// flatFileSessionInfo builds a SessionInfo from a discovered flat session file.
func flatFileSessionInfo(info *sessionFileInfo, projectPath string) *agent.SessionInfo {
	// The transcript path for OpenCode is the message directory
	msgDir, _ := GetMessageDir(info.sessionID)
	return &agent.SessionInfo{
		SessionID:      info.sessionID,
		TranscriptPath: msgDir,
		StartedAt:      info.modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// mostRecentSessionFile returns the most recently modified *.json session
// file in dir, provided it falls within the recent-session timeout.
func mostRecentSessionFile(dir string) *sessionFileInfo {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	now := time.Now()
	var best *sessionFileInfo

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

		if best == nil || modTime.After(best.modTime) {
			best = &sessionFileInfo{
				sessionID: strings.TrimSuffix(entry.Name(), ".json"),
				modTime:   modTime,
			}
		}
	}

	return best
}

// mostRecentMatchingSessionFile scans a flat directory of session JSON files
// (not nested per project) and returns the most recent one whose recorded
// project identifier or directory matches the current project.
func mostRecentMatchingSessionFile(dir, projectID, projectPath string) *sessionFileInfo {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	now := time.Now()
	var best *sessionFileInfo

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
		if best != nil && !modTime.After(best.modTime) {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		if !sessionMatchesProject(raw, projectID, projectPath) {
			continue
		}

		id := strings.TrimSuffix(entry.Name(), ".json")
		if idRaw, ok := raw["id"]; ok {
			var s string
			if json.Unmarshal(idRaw, &s) == nil && s != "" {
				id = s
			}
		}

		best = &sessionFileInfo{sessionID: id, modTime: modTime}
	}

	return best
}

// sessionMatchesProject reports whether a session record belongs to the given
// project, checking field names used for project identification across
// different OpenCode versions.
func sessionMatchesProject(raw map[string]json.RawMessage, projectID, projectPath string) bool {
	for _, field := range []string{"projectID", "project_id", "project"} {
		v, ok := raw[field]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) == nil && s == projectID {
			return true
		}
	}

	for _, field := range []string{"directory", "path", "cwd", "worktree"} {
		v, ok := raw[field]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) == nil && s == projectPath {
			return true
		}
	}

	return false
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session. Table and column names are introspected at query time since they
// have changed across OpenCode releases (e.g. project identification moving
// from a "project_id" hash column to a plain "directory" path column, or
// table/column renames).
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := firstExistingSQLiteTable(dbPath, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}

	sessionID := findRecentSQLiteSessionID(dbPath, sessionTable, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	messageTable := firstExistingSQLiteTable(dbPath, "message", "messages")
	if messageTable == "" {
		return nil, nil
	}

	transcriptData := sqliteSessionMessages(dbPath, messageTable, sessionID)
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

// findRecentSQLiteSessionID finds the most recent session ID for a project,
// adapting to whichever project-identifying and timestamp columns the
// installed OpenCode version's session table actually has.
func findRecentSQLiteSessionID(dbPath, table, projectID, projectPath string) string {
	columns := sqliteTableColumns(dbPath, table)
	if len(columns) == 0 {
		return ""
	}

	orderCol := ""
	for _, c := range []string{"time_updated", "updated", "time_modified", "time_created", "created"} {
		if columns[c] {
			orderCol = c
			break
		}
	}

	tryColumns := func(candidates []string, value string) string {
		for _, col := range candidates {
			if !columns[col] {
				continue
			}
			order := ""
			if orderCol != "" {
				order = fmt.Sprintf(" ORDER BY %s DESC", orderCol)
			}
			q := fmt.Sprintf(`SELECT id FROM %s WHERE %s='%s'%s LIMIT 1;`,
				table, col, sqliteEscape(value), order)
			if id := sqliteScalar(dbPath, q); id != "" {
				if orderCol == "" || sqliteSessionRecent(dbPath, table, orderCol, id) {
					return id
				}
			}
		}
		return ""
	}

	// Prefer matching by a hash-style project identifier column.
	if id := tryColumns([]string{"project_id", "projectID", "project"}, projectID); id != "" {
		return id
	}

	// Fall back to matching by a path-style column.
	if id := tryColumns([]string{"directory", "path", "cwd", "worktree"}, projectPath); id != "" {
		return id
	}

	// Last resort: most recent session regardless of project. Reasonable when
	// the data directory is scoped to a single project (as in tests) even if
	// no recognizable project column was found.
	order := ""
	if orderCol != "" {
		order = fmt.Sprintf(" ORDER BY %s DESC", orderCol)
	}
	q := fmt.Sprintf(`SELECT id FROM %s%s LIMIT 1;`, table, order)
	if id := sqliteScalar(dbPath, q); id != "" && (orderCol == "" || sqliteSessionRecent(dbPath, table, orderCol, id)) {
		return id
	}

	return ""
}

// sqliteSessionRecent checks whether a session's timestamp column indicates
// it was updated within the recent-session window. If the timestamp can't be
// read or parsed, it assumes the session is recent enough — better to try
// than to skip a valid session over a formatting difference.
func sqliteSessionRecent(dbPath, table, timeCol, sessionID string) bool {
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE id='%s';`, timeCol, table, sqliteEscape(sessionID))
	out := sqliteScalar(dbPath, q)
	if out == "" {
		return true
	}

	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, out); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	// Unix epoch, used by some OpenCode versions (seconds or milliseconds).
	if n, err := strconv.ParseInt(out, 10, 64); err == nil {
		t := time.Unix(n, 0)
		if n > 1e12 {
			t = time.UnixMilli(n)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	return true
}

// sqliteSessionMessages fetches all messages for a session as a JSON array,
// adapting to whichever message columns are present. Returns nil if no
// messages could be retrieved.
func sqliteSessionMessages(dbPath, table, sessionID string) []byte {
	columns := sqliteTableColumns(dbPath, table)
	if len(columns) == 0 {
		return nil
	}

	dataExpr := ""
	if columns["data"] {
		dataExpr = "data"
	} else {
		var parts []string
		for _, c := range []string{"role", "content", "text", "time_created", "time"} {
			if columns[c] {
				parts = append(parts, fmt.Sprintf("'%s', %s", c, c))
			}
		}
		if len(parts) == 0 {
			return nil
		}
		dataExpr = "json_object(" + strings.Join(parts, ", ") + ")"
	}

	orderClause := ""
	for _, c := range []string{"time_created", "time_updated", "created"} {
		if columns[c] {
			orderClause = " ORDER BY " + c
			break
		}
	}

	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(%s, json_object('id', id))) FROM %s WHERE session_id='%s'%s;`,
		dataExpr, table, sqliteEscape(sessionID), orderClause,
	)
	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if len(transcriptData) == 0 || string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
		return nil
	}

	return transcriptData
}

// firstExistingSQLiteTable returns the first table name from names that
// exists (and has columns) in the database, or "" if none do.
func firstExistingSQLiteTable(dbPath string, names ...string) string {
	for _, name := range names {
		if cols := sqliteTableColumns(dbPath, name); len(cols) > 0 {
			return name
		}
	}
	return ""
}

// sqliteTableColumns returns the set of column names for a table in the
// OpenCode SQLite database. Returns nil if the table doesn't exist, is
// empty, or the query fails.
func sqliteTableColumns(dbPath, table string) map[string]bool {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("SELECT name FROM pragma_table_info('%s');", table))
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil
	}

	cols := make(map[string]bool)
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			cols[line] = true
		}
	}
	return cols
}

// sqliteScalar runs a query expected to return a single scalar value,
// returning "" if the query fails or returns no rows.
func sqliteScalar(dbPath, query string) string {
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// sqliteEscape escapes single quotes for safe inline use in SQLite string literals.
func sqliteEscape(s string) string {
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
