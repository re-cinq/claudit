```go
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
// OpenCode's on-disk storage format has changed across versions, so this
// tries several known layouts in order: flat files partitioned by project
// (pre-v1.2), a shared flat-file "info" directory (later versions), then a
// SQLite database whose schema is introspected at runtime since table and
// column names have also drifted between releases.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
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

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// discoverFromFlatFiles tries known flat-file session discovery layouts.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}
	projectID := GetProjectID(projectPath)

	// Layout A (pre-v1.2): storage/session/<projectID>/<sessionID>.json,
	// messages under storage/message/<sessionID>/.
	sessionDir := filepath.Join(dataDir, "storage", "session", projectID)
	if bestID, bestModTime := latestSessionFile(sessionDir, nil); bestID != "" {
		msgDir, _ := GetMessageDir(bestID)
		return &agent.SessionInfo{
			SessionID:      bestID,
			TranscriptPath: msgDir,
			StartedAt:      bestModTime.Format(time.RFC3339),
			ProjectPath:    projectPath,
		}, nil
	}

	// Layout B (later versions): sessions for all projects live in a shared
	// "info" directory identified by a projectID/directory field, with
	// messages either alongside (storage/session/message/<id>/) or in the
	// legacy message dir (storage/message/<id>/).
	infoDir := filepath.Join(dataDir, "storage", "session", "info")
	matchesProject := func(data []byte) bool {
		var fields struct {
			ProjectID string `json:"projectID"`
			Directory string `json:"directory"`
		}
		if err := json.Unmarshal(data, &fields); err != nil {
			return false
		}
		if fields.ProjectID != "" {
			return fields.ProjectID == projectID
		}
		return fields.Directory != "" && agent.PathsEqual(fields.Directory, projectPath)
	}

	bestID, bestModTime := latestSessionFile(infoDir, matchesProject)
	if bestID == "" {
		return nil, nil
	}

	for _, candidate := range []string{
		filepath.Join(dataDir, "storage", "session", "message", bestID),
		filepath.Join(dataDir, "storage", "message", bestID),
	} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return &agent.SessionInfo{
				SessionID:      bestID,
				TranscriptPath: candidate,
				StartedAt:      bestModTime.Format(time.RFC3339),
				ProjectPath:    projectPath,
			}, nil
		}
	}

	return &agent.SessionInfo{
		SessionID:   bestID,
		StartedAt:   bestModTime.Format(time.RFC3339),
		ProjectPath: projectPath,
	}, nil
}

// latestSessionFile scans dir for the most recently modified *.json file
// within agent.RecentSessionTimeout, optionally filtered by the raw file
// contents via matches. It returns the session ID (filename without
// extension) and mod time, or ("", zero time) if nothing matched.
func latestSessionFile(dir string, matches func([]byte) bool) (string, time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}
	}

	now := time.Now()
	var bestID string
	var bestModTime time.Time

	for _, entry := range entries {
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

		if matches != nil {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil || !matches(data) {
				continue
			}
		}

		if bestID == "" || modTime.After(bestModTime) {
			bestID = strings.TrimSuffix(entry.Name(), ".json")
			bestModTime = modTime
		}
	}

	return bestID, bestModTime
}

// discoverFromSQLite queries an OpenCode SQLite database for the most recent
// session belonging to projectID. Table and column names are discovered at
// runtime (rather than hardcoded) since OpenCode's schema has changed
// between releases.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findSQLiteDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	tables, err := sqliteTables(dbPath)
	if err != nil || len(tables) == 0 {
		return nil, nil
	}

	sessionTable := pickTable(tables, "session")
	messageTable := pickTable(tables, "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols, err := sqliteColumns(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project", "directory", "worktree", "path")
	sessionTimeCol := pickColumn(sessionCols, "time_updated", "updated_at", "updatedat", "updated", "time_created", "created_at")
	if idCol == "" || projectCol == "" {
		return nil, nil
	}

	orderBy := "rowid"
	if sessionTimeCol != "" {
		orderBy = quoteIdent(sessionTimeCol)
	}

	sessionQuery := fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
		quoteIdent(idCol), quoteIdent(sessionTable), quoteIdent(projectCol), projectID, orderBy,
	)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Check if this session was recent (within timeout)
	if sessionTimeCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`,
			quoteIdent(sessionTimeCol), quoteIdent(sessionTable), quoteIdent(idCol), sessionID)
		cmd = exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			if !isRecentTimestamp(strings.TrimSpace(string(timeOutput))) {
				return nil, nil
			}
		}
	}

	messageCols, err := sqliteColumns(dbPath, messageTable)
	if err != nil || len(messageCols) == 0 {
		return nil, nil
	}

	msgIDCol := pickColumn(messageCols, "id")
	sessionRefCol := pickColumn(messageCols, "session_id", "sessionid", "session")
	msgTimeCol := pickColumn(messageCols, "time_created", "created_at", "createdat", "created", "time_updated")
	dataCol := pickColumn(messageCols, "data", "content", "body", "payload", "message")
	if sessionRefCol == "" {
		return nil, nil
	}

	msgOrderBy := "rowid"
	if msgTimeCol != "" {
		msgOrderBy = quoteIdent(msgTimeCol)
	}

	var selectExpr string
	switch {
	case dataCol != "" && msgIDCol != "":
		selectExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", quoteIdent(dataCol), quoteIdent(msgIDCol))
	case dataCol != "":
		selectExpr = quoteIdent(dataCol)
	default:
		// No single JSON blob column - build one from whatever recognisable
		// columns exist on this table.
		var parts []string
		for _, alias := range []string{"id", "role", "type", "content", "text"} {
			if col := pickColumn(messageCols, alias); col != "" {
				parts = append(parts, fmt.Sprintf("'%s', %s", alias, quoteIdent(col)))
			}
		}
		if len(parts) == 0 {
			return nil, nil
		}
		selectExpr = fmt.Sprintf("json_object(%s)", strings.Join(parts, ", "))
	}

	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM %s WHERE %s='%s' ORDER BY %s;`,
		selectExpr, quoteIdent(messageTable), quoteIdent(sessionRefCol), sessionID, msgOrderBy,
	)
	cmd = exec.Command("sqlite3", dbPath, msgQuery)
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

// findSQLiteDB locates OpenCode's SQLite database file, trying known names
// before falling back to any *.db/*.sqlite* file in the data directory.
func findSQLiteDB(dataDir string) string {
	for _, name := range []string{"opencode.db", "opencode.sqlite", "opencode.sqlite3", "storage.db", "db.sqlite", "state.db"} {
		p := filepath.Join(dataDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, pattern := range []string{"*.db", "*.sqlite", "*.sqlite3"} {
		matches, _ := filepath.Glob(filepath.Join(dataDir, pattern))
		if len(matches) > 0 {
			return matches[0]
		}
	}
	return ""
}

// sqliteTables lists all table names in the SQLite database.
func sqliteTables(dbPath string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var tables []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			tables = append(tables, line)
		}
	}
	return tables, nil
}

// sqliteColumns lists column names for a table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) ([]string, error) {
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(table)))
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// pickTable returns the table whose name best matches substr: an exact
// case-insensitive match is preferred, falling back to a substring match.
func pickTable(tables []string, substr string) string {
	for _, t := range tables {
		if strings.EqualFold(t, substr) {
			return t
		}
	}
	for _, t := range tables {
		if strings.Contains(strings.ToLower(t), substr) {
			return t
		}
	}
	return ""
}

// pickColumn returns the first column matching one of the candidate names,
// trying an exact case-insensitive match for each candidate in order before
// falling back to a substring match.
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
			if strings.Contains(strings.ToLower(c), cand) {
				return c
			}
		}
	}
	return ""
}

// quoteIdent quotes a SQLite identifier for safe interpolation into a query.
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// isRecentTimestamp reports whether ts parses as one of OpenCode's known
// timestamp formats and falls within agent.RecentSessionTimeout of now. An
// unparseable timestamp is treated as recent — better to try than to skip.
func isRecentTimestamp(ts string) bool {
	if ts == "" {
		return true
	}

	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, ts); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	// Some schemas store timestamps as integer epoch seconds/milliseconds.
	if n, err := strconv.ParseInt(ts, 10, 64); err == nil {
		t := time.Unix(n, 0)
		if n > 1e12 {
			t = time.UnixMilli(n)
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
```
