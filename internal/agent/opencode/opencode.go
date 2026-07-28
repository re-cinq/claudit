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

// sqlQuote escapes single quotes for safe inline use in a sqlite3 CLI statement.
func sqlQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// ftsShadowSuffixes are suffixes SQLite appends to shadow tables backing
// FTS5 virtual tables (e.g. "session_fts_data"). These should never be
// mistaken for the real session/message tables during name matching.
var ftsShadowSuffixes = []string{"_data", "_idx", "_docsize", "_config", "_content"}

// sqliteTableNames returns table names in dbPath whose name contains substr
// (case-insensitive), excluding FTS5 shadow tables. OpenCode's on-disk
// schema (table/column names) has changed across versions, so callers
// should not assume a single fixed table name.
func sqliteTableNames(dbPath, substr string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var matches []string
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if !strings.Contains(lower, substr) {
			continue
		}
		isShadow := false
		for _, suffix := range ftsShadowSuffixes {
			if strings.HasSuffix(lower, suffix) {
				isShadow = true
				break
			}
		}
		if !isShadow {
			matches = append(matches, name)
		}
	}
	return matches, nil
}

// sqliteColumns returns the column names of table in dbPath.
func sqliteColumns(dbPath, table string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
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

// pickColumn returns the first candidate (case-insensitive) present in cols,
// or "" if none of the candidates match any column.
func pickColumn(cols []string, candidates ...string) string {
	byLower := make(map[string]string, len(cols))
	for _, c := range cols {
		byLower[strings.ToLower(c)] = c
	}
	for _, cand := range candidates {
		if actual, ok := byLower[strings.ToLower(cand)]; ok {
			return actual
		}
	}
	return ""
}

// parseSQLiteTimestamp parses a timestamp value read back from sqlite3's CLI
// output. OpenCode has stored timestamps as RFC3339(-Nano), a plain
// "YYYY-MM-DD HH:MM:SS" string, and as a Unix epoch integer (seconds or
// milliseconds) across different versions, so all are attempted.
func parseSQLiteTimestamp(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", raw); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02 15:04:05", raw); err == nil {
		return t, true
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n), true
		}
		if n > 1e9 {
			return time.Unix(n, 0), true
		}
	}
	return time.Time{}, false
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session belonging to projectPath. OpenCode's SQLite schema (table names,
// column names, and the identifier it uses to scope sessions to a project)
// has changed between releases, so the schema is introspected at runtime via
// sqlite_master/PRAGMA table_info instead of assuming fixed names.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTables, err := sqliteTableNames(dbPath, "session")
	if err != nil || len(sessionTables) == 0 {
		return nil, nil
	}

	var sessionTable, idCol, projectCol, updatedCol string
	for _, t := range sessionTables {
		cols, err := sqliteColumns(dbPath, t)
		if err != nil || len(cols) == 0 {
			continue
		}
		id := pickColumn(cols, "id")
		if id == "" {
			continue
		}
		sessionTable = t
		idCol = id
		projectCol = pickColumn(cols, "project_id", "projectid", "project", "directory", "cwd", "path", "worktree", "workspace_id", "workspace")
		updatedCol = pickColumn(cols, "time_updated", "updated_at", "updatedat", "updated", "time_modified", "modified_at", "mtime")
		break
	}
	if sessionTable == "" {
		return nil, nil
	}

	absProjectPath, err := filepath.Abs(projectPath)
	if err != nil {
		absProjectPath = projectPath
	}
	candidates := []string{projectID}
	if absProjectPath != projectID {
		candidates = append(candidates, absProjectPath)
	}

	var sessionID string

	// Find most recent session for this project.
	if projectCol != "" {
		conds := make([]string, 0, len(candidates))
		for _, c := range candidates {
			conds = append(conds, fmt.Sprintf("%s='%s'", projectCol, sqlQuote(c)))
		}
		query := fmt.Sprintf("SELECT %s FROM %s WHERE %s", idCol, sessionTable, strings.Join(conds, " OR "))
		if updatedCol != "" {
			query += fmt.Sprintf(" ORDER BY %s DESC", updatedCol)
		}
		query += " LIMIT 1;"

		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		if out, err := cmd.Output(); err == nil {
			sessionID = strings.TrimSpace(string(out))
		}
	}

	// If we couldn't scope by project (column missing, or renamed to
	// something we don't recognize), fall back to the single most recently
	// updated session in the database, still bounded by the recency check
	// below.
	if sessionID == "" && updatedCol != "" {
		query := fmt.Sprintf("SELECT %s FROM %s ORDER BY %s DESC LIMIT 1;", idCol, sessionTable, updatedCol)
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		if out, err := cmd.Output(); err == nil {
			sessionID = strings.TrimSpace(string(out))
		}
	}

	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout).
	if updatedCol != "" {
		timeQuery := fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s';", updatedCol, sessionTable, idCol, sqlQuote(sessionID))
		cmd := exec.Command("sqlite3", dbPath, timeQuery)
		if out, err := cmd.Output(); err == nil {
			if t, ok := parseSQLiteTimestamp(string(out)); ok {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
			// If we can't parse the time, proceed anyway — better to try than skip.
		}
	}

	messageTables, err := sqliteTableNames(dbPath, "message")
	if err != nil || len(messageTables) == 0 {
		return nil, nil
	}

	var messageTable, msgIDCol, sessionRefCol, contentCol, createdCol string
	for _, t := range messageTables {
		cols, err := sqliteColumns(dbPath, t)
		if err != nil || len(cols) == 0 {
			continue
		}
		sessionRef := pickColumn(cols, "session_id", "sessionid", "session")
		content := pickColumn(cols, "data", "parts", "content", "body")
		if sessionRef == "" || content == "" {
			continue
		}
		messageTable = t
		sessionRefCol = sessionRef
		contentCol = content
		msgIDCol = pickColumn(cols, "id")
		createdCol = pickColumn(cols, "time_created", "created_at", "createdat", "created")
		break
	}
	if messageTable == "" {
		return nil, nil
	}

	// Get messages for this session as a JSON array. Prefer merging the
	// message id into the content blob (matches OpenCode's historical
	// "data" column, a JSON object per message); if the content column
	// isn't a JSON object (e.g. a "parts" array), fall back to selecting
	// it directly.
	buildQuery := func(selectExpr string) string {
		q := fmt.Sprintf("SELECT json_group_array(%s) FROM %s WHERE %s='%s'",
			selectExpr, messageTable, sessionRefCol, sqlQuote(sessionID))
		if createdCol != "" {
			q += fmt.Sprintf(" ORDER BY %s", createdCol)
		}
		return q + ";"
	}

	var msgOutput []byte
	if msgIDCol != "" {
		selectExpr := fmt.Sprintf("json_patch(%s, json_object('id', %s))", contentCol, msgIDCol)
		cmd := exec.Command("sqlite3", dbPath, buildQuery(selectExpr))
		if out, err := cmd.Output(); err == nil {
			msgOutput = out
		}
	}
	if len(msgOutput) == 0 {
		cmd := exec.Command("sqlite3", dbPath, buildQuery(contentCol))
		out, err := cmd.Output()
		if err != nil {
			return nil, nil
		}
		msgOutput = out
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
