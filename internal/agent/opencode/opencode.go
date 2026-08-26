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

// discoverFromFlatFiles tries the legacy flat file session discovery. It first
// looks under shiftlog's own computed project ID directory, then falls back to
// scanning every project's session files for one whose own recorded working
// directory matches projectPath. The fallback protects against OpenCode changing
// its project ID scheme (shiftlog replicates the root-commit-hash algorithm,
// which can drift from upstream across releases).
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	if session := discoverFlatFilesByProjectDir(projectPath); session != nil {
		return session, nil
	}
	return discoverFlatFilesByDirectoryField(projectPath), nil
}

// discoverFlatFilesByProjectDir looks for session files under the project
// directory keyed by shiftlog's own computed project ID (root commit hash).
func discoverFlatFilesByProjectDir(projectPath string) *agent.SessionInfo {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return nil
	}

	dirEntries, err := os.ReadDir(sessionDir)
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

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = strings.TrimSuffix(entry.Name(), ".json")
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

// discoverFlatFilesByDirectoryField scans every project's session directory and
// matches on the session file's own recorded working directory. This is a
// fallback for when OpenCode's project ID scheme no longer matches shiftlog's
// own computation, e.g. after an upstream storage change.
func discoverFlatFilesByDirectoryField(projectPath string) *agent.SessionInfo {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil
	}

	baseDir := filepath.Join(dataDir, "storage", "session")
	projectDirs, err := os.ReadDir(baseDir)
	if err != nil {
		return nil
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout
	var bestSessionID string
	var bestModTime time.Time

	for _, projectDir := range projectDirs {
		if !projectDir.IsDir() {
			continue
		}

		sessionFiles, err := os.ReadDir(filepath.Join(baseDir, projectDir.Name()))
		if err != nil {
			continue
		}

		for _, sf := range sessionFiles {
			if sf.IsDir() || !strings.HasSuffix(sf.Name(), ".json") {
				continue
			}

			info, err := sf.Info()
			if err != nil {
				continue
			}

			modTime := info.ModTime()
			if now.Sub(modTime) > recentTimeout {
				continue
			}
			if bestSessionID != "" && !modTime.After(bestModTime) {
				continue
			}

			data, err := os.ReadFile(filepath.Join(baseDir, projectDir.Name(), sf.Name()))
			if err != nil {
				continue
			}

			var raw map[string]interface{}
			if err := json.Unmarshal(data, &raw); err != nil {
				continue
			}

			if !sessionDirectoryMatches(raw, projectPath) {
				continue
			}

			id, _ := raw["id"].(string)
			if id == "" {
				id = strings.TrimSuffix(sf.Name(), ".json")
			}

			bestSessionID = id
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

// sessionDirectoryMatches checks common field names OpenCode may use across
// releases to record a session's working directory.
func sessionDirectoryMatches(raw map[string]interface{}, projectPath string) bool {
	for _, key := range []string{"directory", "cwd", "worktree", "path", "root"} {
		if v, ok := raw[key].(string); ok && v != "" && agent.PathsEqual(v, projectPath) {
			return true
		}
	}
	return false
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session. OpenCode's internal SQLite schema (table and column names) has been
// observed to change across releases, so the schema is discovered via
// introspection instead of assuming fixed names. Sessions are matched primarily
// by their own recorded working directory, falling back to shiftlog's computed
// project ID, and finally to the single most recently touched session.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, sessionCols := findSQLiteTable(dbPath, "session")
	if sessionTable == "" {
		return nil, nil
	}

	idCol := pickSQLiteColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := pickSQLiteColumn(sessionCols, "project_id")
	dirCol := pickSQLiteColumn(sessionCols, "directory", "cwd", "path", "worktree")
	timeCol := pickSQLiteColumn(sessionCols, "time_updated", "updated_at", "time_created", "created_at")

	orderExpr := "rowid"
	if timeCol != "" {
		orderExpr = quoteIdent(timeCol)
	}

	query := fmt.Sprintf(`SELECT * FROM %s ORDER BY %s DESC LIMIT 20;`, quoteIdent(sessionTable), orderExpr)
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal(output, &rows); err != nil || len(rows) == 0 {
		return nil, nil
	}

	chosen := selectMatchingSessionRow(rows, projectID, projectPath, projectCol, dirCol)
	if chosen == nil {
		// Best effort: the most recently touched session overall.
		chosen = rows[0]
	}

	if timeCol != "" && !sqliteValueIsRecent(chosen[timeCol]) {
		return nil, nil
	}

	sessionIDVal, _ := chosen[idCol].(string)
	if sessionIDVal == "" {
		return nil, nil
	}

	transcriptData, err := fetchOpenCodeMessagesSQLite(dbPath, sessionIDVal)
	if err != nil || len(transcriptData) == 0 {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionIDVal,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// selectMatchingSessionRow prefers a row whose own directory matches projectPath,
// then one whose project ID matches, among the most recent rows.
func selectMatchingSessionRow(rows []map[string]interface{}, projectID, projectPath, projectCol, dirCol string) map[string]interface{} {
	if dirCol != "" {
		for _, row := range rows {
			if v, ok := row[dirCol].(string); ok && v != "" && agent.PathsEqual(v, projectPath) {
				return row
			}
		}
	}
	if projectCol != "" {
		for _, row := range rows {
			if v, ok := row[projectCol].(string); ok && v == projectID {
				return row
			}
		}
	}
	return nil
}

// fetchOpenCodeMessagesSQLite reads all messages for a session as a JSON array,
// discovering the message table/column schema via introspection.
func fetchOpenCodeMessagesSQLite(dbPath, sessionID string) ([]byte, error) {
	msgTable, msgCols := findSQLiteTable(dbPath, "message")
	if msgTable == "" {
		return nil, fmt.Errorf("message table not found")
	}

	sessionCol := pickSQLiteColumn(msgCols, "session_id")
	idCol := pickSQLiteColumn(msgCols, "id")
	dataCol := pickSQLiteColumn(msgCols, "data", "content", "body")
	timeCol := pickSQLiteColumn(msgCols, "time_created", "created_at")

	if sessionCol == "" || dataCol == "" {
		return nil, fmt.Errorf("message table missing expected columns")
	}

	selectExpr := quoteIdent(dataCol)
	if idCol != "" {
		selectExpr = fmt.Sprintf(`json_patch(%s, json_object('id', %s))`, quoteIdent(dataCol), quoteIdent(idCol))
	}

	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s", quoteIdent(timeCol))
	}

	escapedSessionID := strings.ReplaceAll(sessionID, "'", "''")
	query := fmt.Sprintf(`SELECT json_group_array(%s) FROM %s WHERE %s='%s'%s;`,
		selectExpr, quoteIdent(msgTable), quoteIdent(sessionCol), escapedSessionID, orderClause)

	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	data := strings.TrimSpace(string(output))
	// sqlite3 returns "[null]" or "[]" when no rows match
	if data == "" || data == "[null]" || data == "[]" {
		return nil, nil
	}
	return []byte(data), nil
}

// findSQLiteTable finds a table whose name matches or contains nameHint (OpenCode's
// table names have changed across releases) and returns its name and column names.
func findSQLiteTable(dbPath, nameHint string) (string, []string) {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	output, err := cmd.Output()
	if err != nil {
		return "", nil
	}

	var best string
	for _, name := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if lower == nameHint || lower == nameHint+"s" {
			best = name
			break
		}
		if best == "" && strings.Contains(lower, nameHint) {
			best = name
		}
	}
	if best == "" {
		return "", nil
	}

	cmd = exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(best)))
	output, err = cmd.Output()
	if err != nil {
		return best, nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) >= 2 {
			cols = append(cols, parts[1])
		}
	}
	return best, cols
}

// pickSQLiteColumn returns the actual column name matching one of the candidates,
// comparing case- and underscore-insensitively (e.g. "project_id" matches "projectID").
func pickSQLiteColumn(cols []string, candidates ...string) string {
	normalized := make(map[string]string, len(cols))
	for _, c := range cols {
		key := strings.ToLower(strings.ReplaceAll(c, "_", ""))
		normalized[key] = c
	}
	for _, cand := range candidates {
		key := strings.ToLower(strings.ReplaceAll(cand, "_", ""))
		if actual, ok := normalized[key]; ok {
			return actual
		}
	}
	return ""
}

// quoteIdent quotes a SQL identifier discovered via introspection (never raw
// user input) so it can be safely interpolated into a query.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqliteValueIsRecent checks a timestamp value (unix seconds/millis, or a
// common timestamp string format) against RecentSessionTimeout. Unrecognized
// formats are treated as recent rather than discarding a potentially valid
// session over a parsing gap.
func sqliteValueIsRecent(v interface{}) bool {
	switch val := v.(type) {
	case float64:
		t := time.Unix(int64(val), 0)
		if val > 1e12 {
			t = time.UnixMilli(int64(val))
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	case string:
		layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
		for _, layout := range layouts {
			if t, err := time.Parse(layout, val); err == nil {
				return time.Since(t) <= agent.RecentSessionTimeout
			}
		}
		return true
	default:
		return true
	}
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
