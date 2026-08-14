```go
package opencode

import (
	"bytes"
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
// Recurses one level into subdirectories, since some OpenCode releases nest
// per-message data under an extra directory (e.g. message/<id>/parts/*.json).
func (a *Agent) parseMessageDir(dir string) (*agent.Transcript, error) {
	var entries []agent.TranscriptEntry
	if err := a.collectMessageFiles(dir, 1, &entries); err != nil {
		return nil, err
	}
	return &agent.Transcript{Entries: entries}, nil
}

func (a *Agent) collectMessageFiles(dir string, depthRemaining int, entries *[]agent.TranscriptEntry) error {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, de := range dirEntries {
		path := filepath.Join(dir, de.Name())

		if de.IsDir() {
			if depthRemaining > 0 {
				_ = a.collectMessageFiles(path, depthRemaining-1, entries)
			}
			continue
		}

		if strings.HasSuffix(de.Name(), ".jsonl") {
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			transcript, err := a.ParseTranscript(f)
			_ = f.Close()
			if err == nil {
				*entries = append(*entries, transcript.Entries...)
			}
			continue
		}

		if !strings.HasSuffix(de.Name(), ".json") {
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}

		entry := parseOpenCodeEntry(raw, data)
		if entry.Type != "" {
			*entries = append(*entries, entry)
		}
	}

	return nil
}

// DiscoverSession finds an active or recent OpenCode session.
// It first tries flat file storage, then falls back to SQLite. OpenCode's
// on-disk storage layout and schema have changed across releases, so both
// paths are written to tolerate drift rather than assume one fixed shape.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// discoverFromFlatFiles tries the flat file session discovery.
//
// The preferred layout is <dataDir>/storage/session/<projectID>/<sessionID>.json,
// but some OpenCode releases store session files without the per-project
// subdirectory. To tolerate that, we walk the whole storage/session tree and
// match candidates either by their parent directory name (legacy layout) or
// by a "directory"/"projectID" field inside the session file itself.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	sessionRoot := filepath.Join(dataDir, "storage", "session")

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout
	var bestPath string
	var bestModTime time.Time

	_ = filepath.WalkDir(sessionRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > recentTimeout {
			return nil
		}

		if !sessionFileMatchesProject(path, projectID, projectPath) {
			return nil
		}

		if bestPath == "" || modTime.After(bestModTime) {
			bestPath = path
			bestModTime = modTime
		}
		return nil
	})

	if bestPath == "" {
		return nil, nil
	}

	sessionID := strings.TrimSuffix(filepath.Base(bestPath), ".json")

	// The transcript path for OpenCode is the message directory
	msgDir, _ := GetMessageDir(sessionID)

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// sessionFileMatchesProject reports whether a session JSON file belongs to
// the given project: either its parent directory is named after the
// project ID (storage/session/<projectID>/<sessionID>.json), or the file's
// own "directory"/"projectID" field matches the current project.
func sessionFileMatchesProject(path, projectID, projectPath string) bool {
	if filepath.Base(filepath.Dir(path)) == projectID {
		return true
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	var info sessionInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return false
	}

	if info.Directory != "" && info.Directory == projectPath {
		return true
	}
	if info.ProjectID != "" && info.ProjectID == projectID {
		return true
	}
	return false
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session. Table and column names are discovered at runtime (via
// sqlite_master / PRAGMA table_info) instead of being hardcoded, since
// OpenCode's internal schema has changed across releases and hardcoded names
// silently stop matching anything rather than erroring loudly.
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
	messageTable := findSQLiteTable(dbPath, "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols := sqliteColumns(dbPath, sessionTable)
	idCol := pickColumn(sessionCols, "id")
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project")
	sessionTimeCol := pickColumn(sessionCols, "time_updated", "updated_at", "updatedat", "time_created", "created_at", "createdat")
	if idCol == "" {
		return nil, nil
	}

	orderBy := "rowid"
	if sessionTimeCol != "" {
		orderBy = sessionTimeCol
	}

	var sessionQuery string
	if projectCol != "" {
		sessionQuery = fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
			idCol, sessionTable, projectCol, projectID, orderBy,
		)
	} else {
		sessionQuery = fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s DESC LIMIT 1;`, idCol, sessionTable, orderBy)
	}

	sessionOutput, err := sqliteQuery(dbPath, sessionQuery)
	if err != nil || sessionOutput == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(strings.Split(sessionOutput, "\n")[0])
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout), when we know how to.
	if sessionTimeCol != "" {
		timeOutput, err := sqliteQuery(dbPath, fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, sessionTimeCol, sessionTable, idCol, sessionID))
		if err == nil && !withinRecentTimeout(timeOutput) {
			return nil, nil
		}
	}

	msgCols := sqliteColumns(dbPath, messageTable)
	msgIDCol := pickColumn(msgCols, "id")
	sessionFKCol := pickColumn(msgCols, "session_id", "sessionid")
	dataCol := pickColumn(msgCols, "data", "content", "parts", "body", "message")
	msgTimeCol := pickColumn(msgCols, "time_created", "created_at", "createdat", "time_updated", "updated_at")
	if msgIDCol == "" || sessionFKCol == "" || dataCol == "" {
		return nil, nil
	}

	msgOrderBy := "rowid"
	if msgTimeCol != "" {
		msgOrderBy = msgTimeCol
	}

	msgQuery := fmt.Sprintf(
		`SELECT %s AS id, %s AS data FROM %s WHERE %s='%s' ORDER BY %s;`,
		msgIDCol, dataCol, messageTable, sessionFKCol, sessionID, msgOrderBy,
	)
	cmd := exec.Command("sqlite3", "-json", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(msgOutput, &rows); err != nil || len(rows) == 0 {
		return nil, nil
	}

	var messages []json.RawMessage
	for _, row := range rows {
		msg := decodeSQLiteMessageData(row["data"])
		if msg == nil {
			continue
		}

		var id string
		if rawID, ok := row["id"]; ok {
			_ = json.Unmarshal(rawID, &id)
		}
		if id != "" {
			if idJSON, err := json.Marshal(id); err == nil {
				msg["id"] = idJSON
			}
		}

		encoded, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		messages = append(messages, encoded)
	}

	if len(messages) == 0 {
		return nil, nil
	}

	transcriptData, err := json.Marshal(messages)
	if err != nil {
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

// sqliteQuery runs a single query via the sqlite3 CLI and returns trimmed stdout.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// findSQLiteTable returns the table whose name best matches the given noun:
// an exact match (singular or plural) is preferred, otherwise any table name
// containing the noun (e.g. "app_sessions" for noun "session").
func findSQLiteTable(dbPath, noun string) string {
	out, err := sqliteQuery(dbPath, `SELECT name FROM sqlite_master WHERE type='table';`)
	if err != nil {
		return ""
	}

	var contains string
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if lower == noun || lower == noun+"s" {
			return name
		}
		if contains == "" && strings.Contains(lower, noun) {
			contains = name
		}
	}
	return contains
}

// sqliteColumns returns a table's column names via PRAGMA table_info.
func sqliteColumns(dbPath, table string) []string {
	out, err := sqliteQuery(dbPath, fmt.Sprintf(`PRAGMA table_info(%s);`, table))
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
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

// pickColumn returns the first column (in its real case) whose lowercased
// name matches one of the given candidates, or "" if none match.
func pickColumn(cols []string, candidates ...string) string {
	byLower := make(map[string]string, len(cols))
	for _, c := range cols {
		byLower[strings.ToLower(c)] = c
	}
	for _, cand := range candidates {
		if actual, ok := byLower[cand]; ok {
			return actual
		}
	}
	return ""
}

// decodeSQLiteMessageData decodes a message row's data column into a JSON
// object. The column is usually stored as a JSON-encoded string (so it comes
// back from `sqlite3 -json` as a quoted string that needs one more decode),
// but tolerate it already being a JSON object too.
func decodeSQLiteMessageData(raw json.RawMessage) map[string]json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}

	var obj map[string]json.RawMessage
	if trimmed[0] == '{' {
		if err := json.Unmarshal(trimmed, &obj); err == nil {
			return obj
		}
		return nil
	}

	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return nil
	}
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil
	}
	return obj
}

// withinRecentTimeout reports whether a raw timestamp value (epoch seconds,
// epoch milliseconds, or one of a few known string formats) is within
// agent.RecentSessionTimeout of now. Unparseable or empty values are treated
// as "recent enough" so an unfamiliar timestamp format doesn't block discovery.
func withinRecentTimeout(raw string) bool {
	timeStr := strings.TrimSpace(raw)
	if timeStr == "" {
		return true
	}

	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		var t time.Time
		if n > 1_000_000_000_000 {
			t = time.UnixMilli(n)
		} else {
			t = time.Unix(n, 0)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	layouts := []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, timeStr); err == nil {
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
```
