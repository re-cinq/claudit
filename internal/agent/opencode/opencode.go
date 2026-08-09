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
// OpenCode's on-disk storage layout (flat JSON files vs. SQLite, project
// nesting, table/column names) has changed across releases, so discovery
// tries several strategies and degrades gracefully: once a session id is
// found, failing to also recover its messages should not throw the whole
// session away, since shiftlog can still record a note referencing it.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (older OpenCode versions).
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite (newer OpenCode versions).
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// discoverFromFlatFiles tries flat-file session discovery. It first checks
// the legacy per-project directory layout (storage/session/<projectID>/),
// then falls back to a flat layout (storage/session/*.json) where each
// session file is matched to this project by its own "projectID" or
// "directory" field, since newer OpenCode versions may not nest session
// files under a project-specific directory at all.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}
	projectID := GetProjectID(projectPath)

	scopedDir := filepath.Join(dataDir, "storage", "session", projectID)
	if session := findRecentSessionFile(scopedDir, projectPath, projectID, false); session != nil {
		return session, nil
	}

	flatDir := filepath.Join(dataDir, "storage", "session")
	if session := findRecentSessionFile(flatDir, projectPath, projectID, true); session != nil {
		return session, nil
	}

	return nil, nil
}

// findRecentSessionFile scans dir for the most recently modified *.json
// session file within the recent-session timeout. When matchContent is
// true, dir is assumed to hold sessions for many projects, so a file is
// only considered when its own JSON body identifies it as belonging to
// this project (used for flat, non-project-nested storage layouts).
func findRecentSessionFile(dir, projectPath, projectID string, matchContent bool) *agent.SessionInfo {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil
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

		sessionID := strings.TrimSuffix(entry.Name(), ".json")

		if matchContent {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil || !sessionMatchesProject(data, projectPath, projectID) {
				continue
			}
			if id := extractSessionID(data); id != "" {
				sessionID = id
			}
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = sessionID
			bestModTime = modTime
		}
	}

	if bestSessionID == "" {
		return nil
	}

	// The transcript path for OpenCode is the message directory
	msgDir, _ := GetMessageDir(bestSessionID)

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// sessionMatchesProject reports whether a session JSON file's body
// identifies it as belonging to projectPath/projectID, checking several
// plausible field names since the exact schema is version-dependent.
func sessionMatchesProject(data []byte, projectPath, projectID string) bool {
	var probe struct {
		ProjectID string `json:"projectID"`
		Directory string `json:"directory"`
		CWD       string `json:"cwd"`
		Path      string `json:"path"`
		Worktree  string `json:"worktree"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	if probe.ProjectID != "" && probe.ProjectID == projectID {
		return true
	}
	for _, d := range []string{probe.Directory, probe.CWD, probe.Path, probe.Worktree} {
		if d != "" && agent.PathsEqual(d, projectPath) {
			return true
		}
	}
	return false
}

// extractSessionID reads the "id" field from a session JSON file body, if present.
func extractSessionID(data []byte) string {
	var probe struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return ""
	}
	return probe.ID
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session belonging to this project. Table and column names are
// discovered at runtime via sqlite_master/PRAGMA table_info rather than
// hardcoded, since OpenCode's schema has changed names (e.g. singular vs.
// plural table names) across releases. Once a session id is found, a
// failure to also resolve its messages degrades to an empty transcript
// rather than discarding the session entirely.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := findSQLiteTable(dbPath, "session")
	if sessionTable == "" {
		return nil, nil
	}

	sessionCols := sqliteColumns(dbPath, sessionTable)
	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := pickColumn(sessionCols,
		"time_updated", "updated_at", "updatedat", "updated",
		"time_created", "created_at", "createdat", "created")
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project")
	dirCol := pickColumn(sessionCols, "directory", "cwd", "path", "worktree")

	sessionID, updatedAt := findRecentSQLiteSession(dbPath, sessionTable, idCol, timeCol, projectCol, dirCol, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	info := &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: []byte("[]"),
	}
	if updatedAt != "" {
		info.StartedAt = updatedAt
	}

	messageTable := findSQLiteTable(dbPath, "message")
	if messageTable == "" {
		return info, nil
	}

	messageCols := sqliteColumns(dbPath, messageTable)
	sessionRefCol := pickColumn(messageCols, "session_id", "sessionid", "session")
	dataCol := pickColumn(messageCols, "data", "content", "parts", "body")
	if sessionRefCol == "" || dataCol == "" {
		return info, nil
	}
	idColMsg := pickColumn(messageCols, "id")
	orderCol := pickColumn(messageCols,
		"time_created", "created_at", "createdat", "created",
		"time_updated", "updated_at")

	selectExpr := quoteIdent(dataCol)
	if idColMsg != "" {
		selectExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", quoteIdent(dataCol), quoteIdent(idColMsg))
	}

	orderClause := ""
	if orderCol != "" {
		orderClause = " ORDER BY " + quoteIdent(orderCol)
	}

	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM %s WHERE %s=%s%s;`,
		selectExpr, quoteIdent(messageTable), quoteIdent(sessionRefCol), sqliteQuote(sessionID), orderClause,
	)
	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err == nil {
		data := strings.TrimSpace(string(msgOutput))
		// sqlite3 returns "[null]" when no rows match
		if data != "" && data != "[null]" && data != "[]" {
			info.TranscriptData = []byte(data)
		}
	}

	return info, nil
}

// findRecentSQLiteSession returns the id (and, if available, the
// update/create timestamp) of the most recent session row. When a
// project-identifying column was found, the query filters on it; otherwise
// it falls back to the single most recent session across the database,
// which is still correct in the common case of one active session.
func findRecentSQLiteSession(dbPath, table, idCol, timeCol, projectCol, dirCol, projectID, projectPath string) (sessionID, updatedAt string) {
	var where string
	switch {
	case projectCol != "":
		where = " WHERE " + quoteIdent(projectCol) + "=" + sqliteQuote(projectID)
	case dirCol != "":
		where = " WHERE " + quoteIdent(dirCol) + "=" + sqliteQuote(projectPath)
	}

	selectCols := quoteIdent(idCol)
	orderClause := " ORDER BY rowid DESC"
	if timeCol != "" {
		selectCols += ", " + quoteIdent(timeCol)
		orderClause = " ORDER BY " + quoteIdent(timeCol) + " DESC"
	}

	query := fmt.Sprintf(`SELECT %s FROM %s%s%s LIMIT 1;`, selectCols, quoteIdent(table), where, orderClause)
	cmd := exec.Command("sqlite3", "-separator", "|", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return "", ""
	}

	line := strings.TrimSpace(string(output))
	if line == "" {
		return "", ""
	}

	parts := strings.SplitN(line, "|", 2)
	sessionID = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		updatedAt = strings.TrimSpace(parts[1])
	}
	return sessionID, updatedAt
}

// findSQLiteTable resolves the actual name of a table matching base
// (e.g. "session"), accepting exact, pluralized, or substring matches
// so schema renames between OpenCode versions don't break discovery.
func findSQLiteTable(dbPath, base string) string {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	lowerBase := strings.ToLower(base)
	var contains string
	for _, name := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		name = strings.TrimSpace(name)
		lower := strings.ToLower(name)
		if lower == lowerBase || lower == lowerBase+"s" {
			return name
		}
		if contains == "" && strings.Contains(lower, lowerBase) {
			contains = name
		}
	}
	return contains
}

// sqliteColumns returns the column names of table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) []string {
	if table == "" {
		return nil
	}
	cmd := exec.Command("sqlite3", "-separator", "|", dbPath, "PRAGMA table_info("+quoteIdent(table)+");")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// pickColumn returns the first candidate (case-insensitive) present in cols.
func pickColumn(cols []string, candidates ...string) string {
	lowerCols := make(map[string]string, len(cols))
	for _, c := range cols {
		lowerCols[strings.ToLower(c)] = c
	}
	for _, cand := range candidates {
		if actual, ok := lowerCols[strings.ToLower(cand)]; ok {
			return actual
		}
	}
	return ""
}

// quoteIdent quotes a SQLite identifier (table/column name).
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqliteQuote quotes a SQLite string literal.
func sqliteQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
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
