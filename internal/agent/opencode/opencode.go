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

// sqliteRow is a single row of a SQLite query result, as decoded from the
// sqlite3 CLI's "-json" output mode (column name -> value).
type sqliteRow map[string]interface{}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session belonging to this project.
//
// OpenCode has changed its SQLite table/column names across releases (e.g.
// the exact spelling of the project and timestamp columns), so rather than
// hardcoding a schema we read rows generically via "-json" output and
// pattern-match on column values/keys. If no session can be matched to this
// specific project (e.g. because OpenCode now identifies projects
// differently than our git-root-commit heuristic), we fall back to the most
// recently created session in the database, which is correct for the common
// case of a single active project.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := findTable(dbPath, "session")
	if sessionTable == "" {
		return nil, nil
	}

	sessionRows, err := queryJSONRows(dbPath, fmt.Sprintf("SELECT * FROM %s ORDER BY rowid DESC LIMIT 200;", sessionTable))
	if err != nil || len(sessionRows) == 0 {
		return nil, nil
	}

	// Prefer a session matching this project; fall back to the most recent
	// session overall if none matches (OpenCode may identify projects
	// differently than our git-root-commit heuristic).
	chosen := sessionRows[0]
	for _, row := range sessionRows {
		if rowHasValue(row, projectID, projectPath, filepath.Base(projectPath)) {
			chosen = row
			break
		}
	}

	if !rowRecentEnough(chosen) {
		return nil, nil
	}

	sessionID, ok := rowID(chosen)
	if !ok || sessionID == "" {
		return nil, nil
	}

	messageTable := findTable(dbPath, "message")
	if messageTable == "" {
		return nil, nil
	}

	messageRows, err := queryJSONRows(dbPath, fmt.Sprintf("SELECT * FROM %s ORDER BY rowid ASC LIMIT 5000;", messageTable))
	if err != nil || len(messageRows) == 0 {
		return nil, nil
	}

	var blobs []map[string]interface{}
	for _, row := range messageRows {
		if !rowHasValue(row, sessionID) {
			continue
		}
		if blob, ok := extractMessageBlob(row); ok {
			blobs = append(blobs, blob)
		}
	}
	if len(blobs) == 0 {
		return nil, nil
	}

	transcriptData, err := json.Marshal(blobs)
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

// queryJSONRows runs a query against the SQLite database via the sqlite3 CLI
// and decodes the result using "-json" mode, which self-describes column
// names and is resilient to schema/column-naming changes between releases.
func queryJSONRows(dbPath, query string) ([]sqliteRow, error) {
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, nil
	}
	var rows []sqliteRow
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// findTable returns the name of the table matching name (exact or plural,
// case-insensitively), falling back to any table whose name contains name
// as a substring. Returns "" if no candidate is found.
func findTable(dbPath, name string) string {
	rows, err := queryJSONRows(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return ""
	}
	var fallback string
	for _, row := range rows {
		tableName, _ := row["name"].(string)
		if tableName == "" {
			continue
		}
		if strings.EqualFold(tableName, name) || strings.EqualFold(tableName, name+"s") {
			return tableName
		}
		if fallback == "" && strings.Contains(strings.ToLower(tableName), name) {
			fallback = tableName
		}
	}
	return fallback
}

// rowID returns the row's "id" column, matched case-insensitively.
func rowID(row sqliteRow) (string, bool) {
	for k, v := range row {
		if strings.EqualFold(k, "id") {
			if s, ok := v.(string); ok && s != "" {
				return s, true
			}
		}
	}
	return "", false
}

// rowHasValue reports whether any string column in row exactly matches one
// of targets. Used to match a session row to a project, or a message row to
// a session, without needing to know the exact column name OpenCode uses.
func rowHasValue(row sqliteRow, targets ...string) bool {
	for _, v := range row {
		s, ok := v.(string)
		if !ok {
			continue
		}
		for _, t := range targets {
			if t != "" && s == t {
				return true
			}
		}
	}
	return false
}

// rowRecentEnough reports whether row was updated/created within
// agent.RecentSessionTimeout, based on any column whose name looks
// timestamp-related. If no such column can be parsed, it assumes the row is
// recent (better to try than to skip).
func rowRecentEnough(row sqliteRow) bool {
	for k, v := range row {
		lk := strings.ToLower(k)
		if !strings.Contains(lk, "time") && !strings.Contains(lk, "update") &&
			!strings.Contains(lk, "creat") && !strings.Contains(lk, "modif") {
			continue
		}
		if t, ok := parseFlexibleTime(v); ok {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}
	return true
}

// parseFlexibleTime parses a timestamp value that may be a string in one of
// several common formats, or a numeric Unix timestamp in seconds,
// milliseconds, microseconds, or nanoseconds.
func parseFlexibleTime(v interface{}) (time.Time, bool) {
	switch val := v.(type) {
	case string:
		formats := []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05.000Z",
			"2006-01-02 15:04:05",
		}
		for _, f := range formats {
			if t, err := time.Parse(f, val); err == nil {
				return t, true
			}
		}
	case float64:
		n := int64(val)
		switch {
		case n > 1e18:
			return time.Unix(0, n), true // nanoseconds
		case n > 1e15:
			return time.Unix(0, n*1e3), true // microseconds
		case n > 1e12:
			return time.Unix(0, n*1e6), true // milliseconds
		case n > 1e9:
			return time.Unix(n, 0), true // seconds
		}
	}
	return time.Time{}, false
}

// extractMessageBlob finds the JSON-object-valued column in a message row
// (OpenCode stores the message body as a JSON text column whose name has
// varied across releases) and merges the row's own id into it, matching the
// shape ParseTranscript expects.
func extractMessageBlob(row sqliteRow) (map[string]interface{}, bool) {
	var blob map[string]interface{}
	for _, v := range row {
		s, ok := v.(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if !strings.HasPrefix(s, "{") {
			continue
		}
		var candidate map[string]interface{}
		if err := json.Unmarshal([]byte(s), &candidate); err != nil {
			continue
		}
		_, hasRole := candidate["role"]
		_, hasType := candidate["type"]
		_, hasContent := candidate["content"]
		if hasRole || hasType || hasContent {
			blob = candidate
			break
		}
	}
	if blob == nil {
		return nil, false
	}
	if _, ok := blob["id"]; !ok {
		if id, ok := rowID(row); ok {
			blob["id"] = id
		}
	}
	return blob, true
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
