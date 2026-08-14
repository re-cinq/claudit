package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

// OpenCode's SQLite schema (table names, column names, and how a project's
// identity is recorded) has changed across releases. These candidate lists
// let discovery try several known spellings instead of hardcoding one exact
// schema, so a future rename doesn't silently break discovery again.
var (
	sessionTableNames  = []string{"session", "sessions"}
	sessionIDKeys      = []string{"id", "ID", "session_id", "sessionID"}
	sessionProjectKeys = []string{"project_id", "projectID", "project", "projectId"}
	sessionTimeKeys    = []string{"time_updated", "timeUpdated", "updated_at", "updatedAt", "time_created", "timeCreated", "created_at", "createdAt"}

	messageTableNames  = []string{"message", "messages"}
	messageIDKeys      = []string{"id", "ID"}
	messageSessionKeys = []string{"session_id", "sessionID"}
	messageTimeKeys    = []string{"time_created", "timeCreated", "created_at", "createdAt", "time_updated", "timeUpdated"}
	messageDataKeys    = []string{"data", "content", "message"}
)

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
// It reads rows generically via `sqlite3 -json` and matches against several
// known key spellings rather than one hardcoded schema, since the exact
// table/column names (and even the database filename) have moved between
// OpenCode releases.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findOpenCodeDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	var sessionRows []map[string]interface{}
	for _, table := range sessionTableNames {
		rows, ok := runSQLiteJSON(dbPath, "SELECT * FROM "+table+";")
		if ok && len(rows) > 0 {
			sessionRows = rows
			break
		}
	}
	if len(sessionRows) == 0 {
		return nil, nil
	}

	sessionID, updatedAt := pickMostRecentSession(sessionRows, projectID)
	if sessionID == "" {
		return nil, nil
	}

	if !isRecentTimestamp(updatedAt) {
		return nil, nil
	}

	transcriptData := fetchSessionMessages(dbPath, sessionID)
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

// findOpenCodeDB locates OpenCode's SQLite database. The default filename is
// "opencode.db" directly under the data dir, but this has moved before, so
// fall back to searching the data dir for any *.db file.
func findOpenCodeDB(dataDir string) string {
	defaultPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(defaultPath); err == nil {
		return defaultPath
	}

	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".db") {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// runSQLiteJSON runs a query against the OpenCode database and decodes the
// result as JSON rows. ok is false if the query itself failed (e.g. the
// table/column referenced doesn't exist in this OpenCode version's schema).
func runSQLiteJSON(dbPath, query string) (rows []map[string]interface{}, ok bool) {
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, false
	}

	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, true
	}

	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil, false
	}
	return rows, true
}

// firstStringField returns the first non-empty value found under any of keys,
// normalizing numeric JSON values (SQLite INTEGER/REAL columns) to strings.
func firstStringField(row map[string]interface{}, keys []string) string {
	for _, key := range keys {
		v, ok := row[key]
		if !ok {
			continue
		}
		switch val := v.(type) {
		case string:
			if val != "" {
				return val
			}
		case float64:
			return strconv.FormatFloat(val, 'f', -1, 64)
		}
	}
	return ""
}

// pickMostRecentSession picks the session most likely to be the one just
// used in the project. It prefers a session whose project field matches
// projectID, but OpenCode has changed how project identity is stored across
// versions, so if nothing matches (or there's no project field at all) it
// falls back to the most recently updated session overall.
func pickMostRecentSession(rows []map[string]interface{}, projectID string) (id string, updatedAt string) {
	var bestAny, bestAnyTime string
	var bestProject, bestProjectTime string

	for _, row := range rows {
		rowID := firstStringField(row, sessionIDKeys)
		if rowID == "" {
			continue
		}
		updated := firstStringField(row, sessionTimeKeys)
		project := firstStringField(row, sessionProjectKeys)

		if bestAny == "" || updated >= bestAnyTime {
			bestAny, bestAnyTime = rowID, updated
		}
		if project != "" && project == projectID && (bestProject == "" || updated >= bestProjectTime) {
			bestProject, bestProjectTime = rowID, updated
		}
	}

	if bestProject != "" {
		return bestProject, bestProjectTime
	}
	return bestAny, bestAnyTime
}

// isRecentTimestamp reports whether ts (in one of several formats OpenCode
// has used for timestamps) is within the recent-session window. If ts is
// empty or can't be parsed, it returns true so we don't drop an otherwise
// valid session over an unrecognized timestamp format.
func isRecentTimestamp(ts string) bool {
	if ts == "" {
		return true
	}
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, ts); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}
	return true
}

// fetchSessionMessages retrieves all messages for sessionID as a JSON array
// matching the shape ParseTranscript expects. It first tries a targeted
// WHERE query across known table/column spellings, then falls back to
// scanning every message row in Go if none of those match this version's
// schema.
func fetchSessionMessages(dbPath, sessionID string) []byte {
	for _, table := range messageTableNames {
		for _, col := range messageSessionKeys {
			query := fmt.Sprintf("SELECT * FROM %s WHERE %s = '%s';", table, col, sqliteEscape(sessionID))
			if rows, ok := runSQLiteJSON(dbPath, query); ok && len(rows) > 0 {
				return marshalMessages(rows)
			}
		}
	}

	for _, table := range messageTableNames {
		rows, ok := runSQLiteJSON(dbPath, "SELECT * FROM "+table+";")
		if !ok {
			continue
		}
		var matched []map[string]interface{}
		for _, row := range rows {
			if firstStringField(row, messageSessionKeys) == sessionID {
				matched = append(matched, row)
			}
		}
		if len(matched) > 0 {
			return marshalMessages(matched)
		}
	}

	return nil
}

// sqliteEscape escapes single quotes for use inside a single-quoted SQLite
// string literal.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// marshalMessages sorts message rows by creation time (falling back to
// result order when no usable timestamp is found) and marshals them into a
// JSON array matching what ParseTranscript expects, unwrapping a JSON-blob
// "data"/"content"/"message" column when the row stores one.
func marshalMessages(rows []map[string]interface{}) []byte {
	type ordered struct {
		index int
		time  string
		row   map[string]interface{}
	}

	items := make([]ordered, len(rows))
	for i, row := range rows {
		items[i] = ordered{index: i, time: firstStringField(row, messageTimeKeys), row: row}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].time != items[j].time {
			return items[i].time < items[j].time
		}
		return items[i].index < items[j].index
	})

	var messages []json.RawMessage
	for _, item := range items {
		id := firstStringField(item.row, messageIDKeys)

		payload := item.row
		for _, key := range messageDataKeys {
			v, ok := item.row[key]
			if !ok {
				continue
			}
			if blob, ok := v.(string); ok {
				var parsed map[string]interface{}
				if err := json.Unmarshal([]byte(blob), &parsed); err == nil {
					payload = parsed
				}
			} else if nested, ok := v.(map[string]interface{}); ok {
				payload = nested
			}
			break
		}

		if id != "" {
			payload["id"] = id
		}

		data, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		messages = append(messages, data)
	}

	if len(messages) == 0 {
		return nil
	}
	out, err := json.Marshal(messages)
	if err != nil {
		return nil
	}
	return out
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
