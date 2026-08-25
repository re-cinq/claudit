```go
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
// It tries, in order: flat file storage (pre-v1.2 OpenCode), a recursive
// scan of the flat file storage tree (for layouts that don't nest sessions
// under a project-ID subdirectory), and finally the SQLite backend
// (OpenCode v1.2+).
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (pre-v1.2 OpenCode)
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	// Fall back to a recursive scan of the session storage tree, in case
	// sessions aren't nested under a project-ID subdirectory the way
	// GetSessionDir assumes.
	if session, err := discoverFromFlatFilesRecursive(dataDir, projectPath); err == nil && session != nil {
		return session, nil
	}

	// Fall back to SQLite (OpenCode v1.2+)
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

// discoverFromFlatFilesRecursive walks the entire session storage tree
// (regardless of nesting depth) looking for a session JSON file whose
// recorded "directory" field matches projectPath. This covers layouts
// where sessions aren't grouped under a project-ID subdirectory the way
// GetSessionDir/discoverFromFlatFiles assumes.
func discoverFromFlatFilesRecursive(dataDir, projectPath string) (*agent.SessionInfo, error) {
	root := filepath.Join(dataDir, "storage", "session")
	now := time.Now()

	var bestSessionID string
	var bestModTime time.Time
	found := false

	_ = filepath.Walk(root, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}

		var session struct {
			ID        string `json:"id"`
			Directory string `json:"directory"`
			ProjectID string `json:"projectID"`
		}
		if err := json.Unmarshal(data, &session); err != nil || session.ID == "" {
			return nil
		}
		if session.Directory == "" || !agent.PathsEqual(session.Directory, projectPath) {
			return nil
		}

		if !found || modTime.After(bestModTime) {
			bestSessionID = session.ID
			bestModTime = modTime
			found = true
		}
		return nil
	})

	if !found {
		return nil, nil
	}

	msgDir, _ := GetMessageDir(bestSessionID)
	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session belonging to projectPath. Rather than assuming fixed
// table/column names, it introspects the actual schema at runtime (via
// PRAGMA table_info) so renamed columns across OpenCode releases don't
// silently break discovery. It also prefers matching on a recorded working
// directory over an opaque project ID where possible, since that doesn't
// depend on replicating OpenCode's internal project ID algorithm.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	tables, err := sqliteTables(dbPath)
	if err != nil {
		return nil, nil
	}

	sessionTable := pickTable(tables, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}

	sessionCols, err := sqliteColumns(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	dirCol := pickColumn(sessionCols, "directory", "cwd", "workdir", "worktree", "path")
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project")
	timeCol := pickColumn(sessionCols, "time_updated", "updated_at", "time_created", "created_at", "timestamp", "mtime", "time")
	dataCol := pickColumn(sessionCols, "data")

	selectCols := []string{quoteIdent(idCol)}
	for _, c := range []string{dirCol, projectCol, dataCol, timeCol} {
		if c != "" {
			selectCols = append(selectCols, quoteIdent(c))
		}
	}

	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(selectCols, ", "), quoteIdent(sessionTable))
	if timeCol != "" {
		query += fmt.Sprintf(" ORDER BY %s DESC", quoteIdent(timeCol))
	}
	query += " LIMIT 50;"

	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) == 0 {
		return nil, nil
	}

	now := time.Now()

	for _, row := range rows {
		sessionID, _ := row[idCol].(string)
		if sessionID == "" {
			continue
		}

		if !sqliteRowMatchesProject(row, dirCol, projectCol, dataCol, projectID, projectPath) {
			continue
		}

		if timeCol != "" {
			if t, ok := parseSQLiteTime(row[timeCol]); ok && now.Sub(t) > agent.RecentSessionTimeout {
				continue
			}
		}

		transcriptData, err := fetchSQLiteMessages(dbPath, tables, sessionID)
		if err != nil || len(transcriptData) == 0 {
			continue
		}

		return &agent.SessionInfo{
			SessionID:      sessionID,
			TranscriptPath: "", // no file path for SQLite
			StartedAt:      time.Now().Format(time.RFC3339),
			ProjectPath:    projectPath,
			TranscriptData: transcriptData,
		}, nil
	}

	return nil, nil
}

// sqliteRowMatchesProject reports whether a session row belongs to
// projectPath, preferring a direct working-directory comparison over an
// opaque project ID match.
func sqliteRowMatchesProject(row map[string]interface{}, dirCol, projectCol, dataCol, projectID, projectPath string) bool {
	if dirCol != "" {
		if dir, ok := row[dirCol].(string); ok && dir != "" && agent.PathsEqual(dir, projectPath) {
			return true
		}
	}
	if projectCol != "" {
		if pid, ok := row[projectCol].(string); ok && pid != "" && pid == projectID {
			return true
		}
	}
	if dataCol != "" {
		if raw, ok := row[dataCol].(string); ok && raw != "" {
			var blob struct {
				Directory string `json:"directory"`
				CWD       string `json:"cwd"`
				ProjectID string `json:"projectID"`
			}
			if err := json.Unmarshal([]byte(raw), &blob); err == nil {
				if blob.Directory != "" && agent.PathsEqual(blob.Directory, projectPath) {
					return true
				}
				if blob.CWD != "" && agent.PathsEqual(blob.CWD, projectPath) {
					return true
				}
				if blob.ProjectID != "" && blob.ProjectID == projectID {
					return true
				}
			}
		}
	}
	return false
}

// parseSQLiteTime tries several timestamp encodings OpenCode has used.
func parseSQLiteTime(v interface{}) (time.Time, bool) {
	switch val := v.(type) {
	case string:
		formats := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
		for _, f := range formats {
			if t, err := time.Parse(f, val); err == nil {
				return t, true
			}
		}
	case float64:
		if val > 1e12 {
			return time.UnixMilli(int64(val)), true
		}
		if val > 0 {
			return time.Unix(int64(val), 0), true
		}
	}
	return time.Time{}, false
}

// fetchSQLiteMessages returns the messages for a session as a JSON array,
// adapting to the actual message table schema rather than assuming fixed
// column names.
func fetchSQLiteMessages(dbPath string, tables []string, sessionID string) ([]byte, error) {
	msgTable := pickTable(tables, "message", "messages")
	if msgTable == "" {
		return nil, fmt.Errorf("no message table found")
	}

	msgCols, err := sqliteColumns(dbPath, msgTable)
	if err != nil || len(msgCols) == 0 {
		return nil, fmt.Errorf("could not read message table columns")
	}

	idCol := pickColumn(msgCols, "id")
	sessionCol := pickColumn(msgCols, "session_id", "sessionid", "session")
	dataCol := pickColumn(msgCols, "data")
	timeCol := pickColumn(msgCols, "time_created", "created_at", "timestamp", "time")

	if sessionCol == "" || dataCol == "" {
		return nil, fmt.Errorf("message table missing expected columns")
	}

	selectExpr := quoteIdent(dataCol)
	if idCol != "" {
		selectExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", quoteIdent(dataCol), quoteIdent(idCol))
	}

	query := fmt.Sprintf(
		"SELECT json_group_array(%s) FROM %s WHERE %s=%s",
		selectExpr, quoteIdent(msgTable), quoteIdent(sessionCol), sqliteString(sessionID),
	)
	if timeCol != "" {
		query += fmt.Sprintf(" ORDER BY %s", quoteIdent(timeCol))
	}
	query += ";"

	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	data := strings.TrimSpace(string(out))
	if data == "[null]" || data == "[]" || data == "" {
		return nil, nil
	}
	return []byte(data), nil
}

// sqliteTables returns the list of table names in the database.
func sqliteTables(dbPath string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, ".tables")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// sqliteColumns returns the column names of a table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) ([]string, error) {
	cmd := exec.Command("sqlite3", "-json", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(table)))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	cols := make([]string, 0, len(rows))
	for _, r := range rows {
		if name, ok := r["name"].(string); ok {
			cols = append(cols, name)
		}
	}
	return cols, nil
}

// pickTable returns the first candidate present in tables.
func pickTable(tables []string, candidates ...string) string {
	set := make(map[string]bool, len(tables))
	for _, t := range tables {
		set[t] = true
	}
	for _, c := range candidates {
		if set[c] {
			return c
		}
	}
	return ""
}

// pickColumn returns the first candidate (case-insensitive) present in
// cols, in its original casing.
func pickColumn(cols []string, candidates ...string) string {
	lower := make(map[string]string, len(cols))
	for _, c := range cols {
		lower[strings.ToLower(c)] = c
	}
	for _, cand := range candidates {
		if actual, ok := lower[strings.ToLower(cand)]; ok {
			return actual
		}
	}
	return ""
}

// quoteIdent quotes a SQLite identifier for safe interpolation into a query.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqliteString quotes a string literal for safe interpolation into a SQLite query.
func sqliteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
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
