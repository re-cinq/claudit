package opencode

import (
	"encoding/json"
	"fmt"
	"io"
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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
//
// OpenCode's on-disk schema is not a stable contract: the database file has moved
// within the data directory before, and table/column names have been renamed across
// releases (e.g. project_id -> directory, or message content moving out of a "data"
// blob into a separate "part" table). Rather than hardcoding names that can silently
// go stale, the table/column names and DB location are discovered at query time.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findOpenCodeDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	tables, err := sqliteTableNames(dbPath)
	if err != nil || len(tables) == 0 {
		return nil, nil
	}

	sessionTable := pickTable(tables, "session")
	if sessionTable == "" {
		return nil, nil
	}
	messageTable := pickTable(tables, "message")

	sessionCols, err := sqliteTableColumns(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	updatedCol := pickColumn(sessionCols, "time_updated", "updated_at", "updatedat", "updated", "time_modified")
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project", "directory", "cwd", "path")

	// Hash-based project columns (project_id) are compared against the git root
	// commit hash; path-based columns (directory/cwd/path) are compared against
	// the absolute project path.
	projectFilter := projectID
	lowerProjectCol := strings.ToLower(projectCol)
	if strings.Contains(lowerProjectCol, "dir") || strings.Contains(lowerProjectCol, "cwd") || strings.Contains(lowerProjectCol, "path") {
		projectFilter = projectPath
	}

	orderBy := idCol
	if updatedCol != "" {
		orderBy = updatedCol
	}

	var sessionQuery string
	if projectCol != "" {
		sessionQuery = fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
			idCol, sessionTable, projectCol, projectFilter, orderBy,
		)
	} else {
		// No recognizable project column — fall back to the most recently
		// active session overall rather than giving up entirely.
		sessionQuery = fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s DESC LIMIT 1;`, idCol, sessionTable, orderBy)
	}

	sessionID, err := sqliteQuery(dbPath, sessionQuery)
	if err != nil || sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout). If we can't determine
	// the time, proceed anyway — better to try than skip a valid session.
	if updatedCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, updatedCol, sessionTable, idCol, sessionID)
		if timeStr, err := sqliteQuery(dbPath, timeQuery); err == nil && timeStr != "" && !isRecentTimestamp(timeStr) {
			return nil, nil
		}
	}

	transcriptData := buildTranscriptData(dbPath, messageTable, sessionID)
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

// buildTranscriptData assembles a JSON array of message data for a session.
// OpenCode has stored per-message content as a JSON blob directly on the
// message row, and, in newer releases, split content out into a separate
// "part" table referencing the message. Both layouts are supported.
func buildTranscriptData(dbPath, messageTable, sessionID string) []byte {
	if messageTable == "" {
		return nil
	}

	msgCols, err := sqliteTableColumns(dbPath, messageTable)
	if err != nil || len(msgCols) == 0 {
		return nil
	}

	msgIDCol := pickColumn(msgCols, "id")
	sessionRefCol := pickColumn(msgCols, "session_id", "sessionid", "session")
	createdCol := pickColumn(msgCols, "time_created", "created_at", "createdat", "created")
	dataCol := pickColumn(msgCols, "data", "content", "body")
	if msgIDCol == "" || sessionRefCol == "" {
		return nil
	}

	msgOrderBy := msgIDCol
	if createdCol != "" {
		msgOrderBy = createdCol
	}

	var query string
	if dataCol != "" {
		query = fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s' ORDER BY %s;`,
			dataCol, msgIDCol, messageTable, sessionRefCol, sessionID, msgOrderBy,
		)
	} else if partTable := findPartTable(dbPath, messageTable); partTable != "" {
		partCols, err := sqliteTableColumns(dbPath, partTable)
		if err != nil || len(partCols) == 0 {
			return nil
		}
		partMsgRefCol := pickColumn(partCols, "message_id", "messageid", "message")
		partTextCol := pickColumn(partCols, "text", "content", "data")
		if partMsgRefCol == "" || partTextCol == "" {
			return nil
		}
		partTypeCol := pickColumn(partCols, "type")
		typeExpr := "'text'"
		if partTypeCol != "" {
			typeExpr = "p." + partTypeCol
		}
		roleCol := pickColumn(msgCols, "role")
		roleExpr := "''"
		if roleCol != "" {
			roleExpr = "m." + roleCol
		}
		timeExpr := "json_object()"
		if createdCol != "" {
			timeExpr = fmt.Sprintf("json_object('created', m.%s)", createdCol)
		}

		query = fmt.Sprintf(
			`SELECT json_group_array(json_object('id', m.%s, 'role', %s, 'time', %s, 'content', `+
				`(SELECT json_group_array(json_object('type', %s, 'text', p.%s)) FROM %s p WHERE p.%s = m.%s))) `+
				`FROM %s m WHERE m.%s='%s' ORDER BY m.%s;`,
			msgIDCol, roleExpr, timeExpr,
			typeExpr, partTextCol, partTable, partMsgRefCol, msgIDCol,
			messageTable, sessionRefCol, sessionID, msgOrderBy,
		)
	} else {
		return nil
	}

	out, err := sqliteQuery(dbPath, query)
	if err != nil || out == "" || out == "[null]" || out == "[]" {
		return nil
	}
	return []byte(out)
}

// findOpenCodeDB locates the OpenCode SQLite database within the data directory.
// It tries the known path first, then falls back to searching for any "*opencode*.db"
// file in case the storage layout has moved.
func findOpenCodeDB(dataDir string) string {
	direct := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(direct); err == nil {
		return direct
	}

	var found string
	_ = filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || found != "" || info == nil || info.IsDir() {
			return nil
		}
		name := strings.ToLower(filepath.Base(path))
		if strings.HasSuffix(name, ".db") && strings.Contains(name, "opencode") {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// findPartTable finds a table (other than messageTable) whose name suggests it
// stores message content parts.
func findPartTable(dbPath, messageTable string) string {
	tables, err := sqliteTableNames(dbPath)
	if err != nil {
		return ""
	}
	for _, t := range tables {
		if t == messageTable {
			continue
		}
		if strings.Contains(strings.ToLower(t), "part") {
			return t
		}
	}
	return ""
}

// sqliteQuery runs a single SQLite statement and returns its trimmed output.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sqliteTableNames lists the tables in the database.
func sqliteTableNames(dbPath string) ([]string, error) {
	out, err := sqliteQuery(dbPath, `SELECT name FROM sqlite_master WHERE type='table';`)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// sqliteTableColumns lists the column names of a table via PRAGMA table_info.
func sqliteTableColumns(dbPath, table string) ([]string, error) {
	cmd := exec.Command("sqlite3", "-separator", "|", dbPath, fmt.Sprintf(`PRAGMA table_info(%s);`, table))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 && fields[1] != "" {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// pickColumn returns the first column matching a preferred name exactly
// (case-insensitive), falling back to the first column that merely contains
// a preferred name, so renamed-but-similar columns still resolve.
func pickColumn(cols []string, preferred ...string) string {
	for _, p := range preferred {
		for _, c := range cols {
			if strings.EqualFold(c, p) {
				return c
			}
		}
	}
	for _, p := range preferred {
		for _, c := range cols {
			if strings.Contains(strings.ToLower(c), strings.ToLower(p)) {
				return c
			}
		}
	}
	return ""
}

// pickTable returns the table matching preferred exactly (case-insensitive),
// falling back to the first table containing it (excluding part tables).
func pickTable(tables []string, preferred string) string {
	for _, t := range tables {
		if strings.EqualFold(t, preferred) {
			return t
		}
	}
	for _, t := range tables {
		lower := strings.ToLower(t)
		if strings.Contains(lower, preferred) && !strings.Contains(lower, "part") {
			return t
		}
	}
	return ""
}

// isRecentTimestamp reports whether timeStr, in any of the formats OpenCode has
// used for session timestamps, falls within the recent-session window. Unknown
// formats are treated as recent — better to try storing than to silently skip
// a valid session over a parsing mismatch.
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
