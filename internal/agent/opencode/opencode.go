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
// It first tries flat file storage (pre-v1.2 OpenCode), then falls back to
// SQLite (v1.2+).
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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session belonging to projectPath.
//
// OpenCode's internal database schema has changed across releases (table and
// column names are not stable), so table/column names are detected at runtime
// via sqlite_master/PRAGMA table_info rather than hardcoded. If the schema
// doesn't expose a project-association column we recognize, we fall back to
// the most recently updated session overall rather than reporting nothing —
// in the common case (single active session) this still finds the right one.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	var dbPath string
	for _, candidate := range []string{
		filepath.Join(dataDir, "opencode.db"),
		filepath.Join(projectPath, ".opencode", "opencode.db"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			dbPath = candidate
			break
		}
	}
	if dbPath == "" {
		return nil, nil
	}

	sessionTable, err := detectSQLiteTable(dbPath, "session", "sessions")
	if err != nil || sessionTable == "" {
		return nil, nil
	}
	sessionCols, err := sqliteTableColumns(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := pickSQLiteColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := pickSQLiteColumn(sessionCols, "time_updated", "updated_at", "updated", "modified_at", "time_modified")
	orderBy := timeCol
	if orderBy == "" {
		orderBy = "rowid"
	}
	projectCol := pickSQLiteColumn(sessionCols, "project_id", "directory", "cwd", "project", "workspace", "path")

	var sessionID string
	if projectCol != "" {
		for _, val := range []string{projectID, projectPath} {
			if val == "" {
				continue
			}
			q := fmt.Sprintf(
				`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
				idCol, sessionTable, projectCol, sqliteEscape(val), orderBy,
			)
			if out, err := sqliteQuery(dbPath, q); err == nil && out != "" {
				sessionID = out
				break
			}
		}
	}

	if sessionID == "" {
		// Schema may not expose a project column we recognize; fall back to
		// the most recently updated session regardless of project.
		q := fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s DESC LIMIT 1;`, idCol, sessionTable, orderBy)
		out, err := sqliteQuery(dbPath, q)
		if err != nil || out == "" {
			return nil, nil
		}
		sessionID = out
	}

	// Check if this session was recent (within timeout)
	if timeCol != "" {
		q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, timeCol, sessionTable, idCol, sqliteEscape(sessionID))
		if out, err := sqliteQuery(dbPath, q); err == nil && out != "" && !isRecentTimestamp(out) {
			return nil, nil
		}
		// If we can't read/parse the time, proceed anyway — better to try than skip.
	}

	transcriptData, err := fetchOpenCodeMessages(dbPath, sessionID)
	if err != nil || len(transcriptData) == 0 {
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

// fetchOpenCodeMessages retrieves all messages for a session as a JSON array,
// adapting to whichever columns the message table actually has. Prefers a
// single JSON blob column ("data") if present; otherwise reconstructs a
// JSON object per row from whichever typed columns exist.
func fetchOpenCodeMessages(dbPath, sessionID string) ([]byte, error) {
	messageTable, err := detectSQLiteTable(dbPath, "message", "messages")
	if err != nil || messageTable == "" {
		return nil, fmt.Errorf("message table not found")
	}
	cols, err := sqliteTableColumns(dbPath, messageTable)
	if err != nil || len(cols) == 0 {
		return nil, fmt.Errorf("could not read message table columns")
	}

	sessionIDCol := pickSQLiteColumn(cols, "session_id", "sessionid", "sessionID")
	if sessionIDCol == "" {
		return nil, fmt.Errorf("no session_id column found")
	}
	orderBy := pickSQLiteColumn(cols, "time_created", "created_at", "created", "time")
	if orderBy == "" {
		orderBy = "rowid"
	}

	var selectExpr string
	if dataCol := pickSQLiteColumn(cols, "data"); dataCol != "" {
		if idCol := pickSQLiteColumn(cols, "id"); idCol != "" {
			selectExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, idCol)
		} else {
			selectExpr = dataCol
		}
	} else {
		var fields []string
		for _, name := range []string{"id", "role", "content", "parts", "model"} {
			if pickSQLiteColumn(cols, name) == name {
				fields = append(fields, fmt.Sprintf("'%s', %s", name, name))
			}
		}
		if len(fields) == 0 {
			return nil, fmt.Errorf("no usable message columns found")
		}
		selectExpr = fmt.Sprintf("json_object(%s)", strings.Join(fields, ", "))
	}

	q := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM %s WHERE %s='%s' ORDER BY %s;`,
		selectExpr, messageTable, sessionIDCol, sqliteEscape(sessionID), orderBy,
	)
	out, err := sqliteQuery(dbPath, q)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	// sqlite3 returns "[null]" when no rows match
	if out == "" || out == "[null]" || out == "[]" {
		return nil, nil
	}
	return []byte(out), nil
}

// sqliteQuery runs a single statement against dbPath and returns trimmed stdout.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sqliteEscape escapes single quotes for safe inline use in a SQL string literal.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// detectSQLiteTable returns the first of the given candidate table names that
// actually exists in the database, or "" if none do.
func detectSQLiteTable(dbPath string, candidates ...string) (string, error) {
	quoted := make([]string, len(candidates))
	for i, c := range candidates {
		quoted[i] = "'" + sqliteEscape(c) + "'"
	}
	q := fmt.Sprintf(`SELECT name FROM sqlite_master WHERE type='table' AND name IN (%s) LIMIT 1;`, strings.Join(quoted, ","))
	out, err := sqliteQuery(dbPath, q)
	if err != nil {
		return "", err
	}
	lines := strings.SplitN(out, "\n", 2)
	return strings.TrimSpace(lines[0]), nil
}

// sqliteTableColumns returns the column names of a table via PRAGMA table_info.
func sqliteTableColumns(dbPath, table string) ([]string, error) {
	out, err := sqliteQuery(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// pickSQLiteColumn returns the first candidate present in cols, or "" if none match.
func pickSQLiteColumn(cols []string, candidates ...string) string {
	set := make(map[string]bool, len(cols))
	for _, c := range cols {
		set[c] = true
	}
	for _, cand := range candidates {
		if set[cand] {
			return cand
		}
	}
	return ""
}

// isRecentTimestamp reports whether raw (in any of the formats OpenCode has
// used for its session timing columns, including raw epoch seconds/millis)
// falls within the recent-session window. Unparseable values are treated as
// recent so we don't block discovery on a format we don't recognize.
func isRecentTimestamp(raw string) bool {
	raw = strings.TrimSpace(raw)

	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		var t time.Time
		if n > 1_000_000_000_000 {
			t = time.UnixMilli(n)
		} else {
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
