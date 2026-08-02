```go
package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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
// OpenCode's on-disk storage layout (and, for the SQLite backend, its
// schema) has shifted across releases, so both discovery paths below are
// written to tolerate structural drift rather than assuming one fixed shape.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first.
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite storage.
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// discoverFromFlatFiles tries flat-file session discovery. OpenCode's
// on-disk session layout has changed across releases (e.g. nesting session
// files under additional subdirectories, or using a project-id scheme that
// no longer matches our own git-root-commit computation), so rather than
// assuming a fixed directory shape we walk the entire session storage tree
// and match each file against the target project by its embedded
// "directory" or "projectID" field.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	if _, err := os.Stat(sessionRoot); err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	now := time.Now()

	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(sessionRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}
		if bestSessionID != "" && !modTime.After(bestModTime) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var meta sessionInfo
		if err := json.Unmarshal(data, &meta); err != nil {
			return nil
		}

		matches := (meta.Directory != "" && agent.PathsEqual(meta.Directory, projectPath)) ||
			(meta.ProjectID != "" && meta.ProjectID == projectID)
		if !matches {
			return nil
		}

		id := meta.ID
		if id == "" {
			id = strings.TrimSuffix(d.Name(), ".json")
		}

		bestSessionID = id
		bestModTime = modTime
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	// The transcript path for OpenCode is the message directory. Older
	// versions store it at storage/message/<sessionID>; newer versions have
	// been observed nesting it under storage/session/message/<sessionID>.
	msgDir, _ := GetMessageDir(bestSessionID)
	if !dirExists(msgDir) {
		if nested := filepath.Join(sessionRoot, "message", bestSessionID); dirExists(nested) {
			msgDir = nested
		}
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// discoverFromSQLite queries OpenCode's SQLite database for the most recent
// session. Table and column names have been observed to vary across
// releases, so the schema is introspected via PRAGMA table_info at runtime
// instead of assuming fixed names.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findOpenCodeDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := detectTable(dbPath, []string{"session", "sessions"})
	if sessionTable == "" {
		return nil, nil
	}

	sessionCols := tableColumns(dbPath, sessionTable)
	idCol := firstPresent(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := firstPresent(sessionCols, "time_updated", "updated_at", "updatedAt", "time_created", "created_at", "createdAt")

	orderBy := ""
	if timeCol != "" {
		orderBy = fmt.Sprintf("ORDER BY %s DESC", timeCol)
	}

	where := ""
	if col := firstPresent(sessionCols, "project_id", "projectID", "project"); col != "" {
		where = fmt.Sprintf("WHERE %s='%s'", col, escapeSQLLiteral(projectID))
	} else if col := firstPresent(sessionCols, "directory", "cwd", "path"); col != "" {
		where = fmt.Sprintf("WHERE %s='%s'", col, escapeSQLLiteral(projectPath))
	}

	// Find most recent session for this project
	sessionID := runSQLite(dbPath, fmt.Sprintf(`SELECT %s FROM %s %s %s LIMIT 1;`, idCol, sessionTable, where, orderBy))
	if sessionID == "" && where != "" {
		// The project filter column may use a different value scheme than we
		// compute (e.g. a new hashing algorithm) — fall back to the most
		// recent session across all projects rather than finding nothing.
		sessionID = runSQLite(dbPath, fmt.Sprintf(`SELECT %s FROM %s %s LIMIT 1;`, idCol, sessionTable, orderBy))
	}
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout)
	if timeCol != "" {
		timeStr := runSQLite(dbPath, fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, timeCol, sessionTable, idCol, escapeSQLLiteral(sessionID)))
		if recent, ok := isRecentTimestamp(timeStr); ok && !recent {
			return nil, nil
		}
		// If we can't parse the time, proceed anyway — better to try than skip.
	}

	messageTable := detectTable(dbPath, []string{"message", "messages"})
	if messageTable == "" {
		return nil, nil
	}
	messageCols := tableColumns(dbPath, messageTable)
	msgSessionCol := firstPresent(messageCols, "session_id", "sessionID", "session")
	if msgSessionCol == "" {
		return nil, nil
	}
	dataCol := firstPresent(messageCols, "data", "parts", "content")
	if dataCol == "" {
		return nil, nil
	}
	msgIDCol := firstPresent(messageCols, "id")
	msgOrderCol := firstPresent(messageCols, "time_created", "created_at", "createdAt", "time_updated", "updated_at")

	selectExpr := dataCol
	if dataCol == "data" && msgIDCol != "" {
		selectExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, msgIDCol)
	}

	msgOrderBy := ""
	if msgOrderCol != "" {
		msgOrderBy = "ORDER BY " + msgOrderCol
	}

	// Get messages for this session as a JSON array
	msgOutput := runSQLite(dbPath, fmt.Sprintf(
		`SELECT json_group_array(%s) FROM %s WHERE %s='%s' %s;`,
		selectExpr, messageTable, msgSessionCol, escapeSQLLiteral(sessionID), msgOrderBy,
	))

	// sqlite3 returns "[null]" when no rows match
	if msgOutput == "" || msgOutput == "[null]" || msgOutput == "[]" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: []byte(msgOutput),
	}, nil
}

// runSQLite executes a single query against dbPath and returns trimmed
// stdout, or "" if the query fails (e.g. an unknown table/column) or the
// sqlite3 binary is unavailable.
func runSQLite(dbPath, query string) string {
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// findOpenCodeDB locates OpenCode's SQLite database. The filename and
// location have varied across releases, so known candidates are checked
// first and, failing that, the data directory is searched for the most
// recently modified *.db/*.sqlite file.
func findOpenCodeDB(dataDir string) string {
	candidates := []string{
		filepath.Join(dataDir, "opencode.db"),
		filepath.Join(dataDir, "db.sqlite"),
		filepath.Join(dataDir, "storage.db"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}

	var best string
	var bestMod time.Time
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := strings.ToLower(d.Name())
		if !strings.HasSuffix(name, ".db") && !strings.HasSuffix(name, ".sqlite") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if best == "" || info.ModTime().After(bestMod) {
			best = path
			bestMod = info.ModTime()
		}
		return nil
	})
	return best
}

// detectTable returns the first candidate table name present in the database.
func detectTable(dbPath string, candidates []string) string {
	for _, c := range candidates {
		q := fmt.Sprintf(`SELECT name FROM sqlite_master WHERE type='table' AND name='%s';`, c)
		if runSQLite(dbPath, q) != "" {
			return c
		}
	}
	return ""
}

// tableColumns returns the column names of a table via PRAGMA table_info.
func tableColumns(dbPath, table string) []string {
	out := runSQLite(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if out == "" {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\t")
		// PRAGMA table_info columns: cid, name, type, notnull, dflt_value, pk
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// firstPresent returns the first candidate that exists in cols, or "".
func firstPresent(cols []string, candidates ...string) string {
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

// escapeSQLLiteral escapes single quotes for safe inclusion in a SQL string literal.
func escapeSQLLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// isRecentTimestamp parses a timestamp string in one of the formats OpenCode
// has used across releases (RFC3339 variants, or a Unix epoch in seconds or
// milliseconds) and reports whether it falls within the recent-session
// window. ok is false when the format isn't recognized, in which case the
// caller should proceed rather than treat the session as stale.
func isRecentTimestamp(s string) (recent bool, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return false, false
	}

	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout, true
		}
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		t := time.Unix(n, 0)
		if n > 1_000_000_000_000 { // milliseconds since epoch
			t = time.UnixMilli(n)
		}
		return time.Since(t) <= agent.RecentSessionTimeout, true
	}

	return false, false
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

	// Try "parts" as an array of typed entries (used by some OpenCode
	// versions in place of "content"), e.g.
	// [{"type":"text","text":"..."}] or [{"type":"text","data":{"text":"..."}}]
	if partsRaw, ok := raw["parts"]; ok {
		var parts []struct {
			Type string          `json:"type"`
			Text string          `json:"text"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(partsRaw, &parts); err == nil {
			var blocks []agent.ContentBlock
			for _, p := range parts {
				if p.Type != "" && p.Type != "text" {
					continue
				}
				text := p.Text
				if text == "" && len(p.Data) > 0 {
					var d struct {
						Text string `json:"text"`
					}
					if err := json.Unmarshal(p.Data, &d); err == nil {
						text = d.Text
					}
				}
				if text != "" {
					blocks = append(blocks, agent.ContentBlock{Type: "text", Text: text})
				}
			}
			if len(blocks) > 0 {
				msg.Content = blocks
				return msg
			}
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
