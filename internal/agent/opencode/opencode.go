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

// sqliteScanLimit bounds how many recent rows we pull back when scanning a
// table for a value match, so a large history doesn't blow up query size.
const sqliteScanLimit = 500

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
//
// OpenCode's SQLite schema has changed table/column names across releases
// (e.g. the "session"/"message" tables gaining, losing, or renaming columns
// such as project_id/time_updated/data). Rather than hardcode one schema
// version, we discover tables by name and match rows by value instead of by
// column name, so discovery keeps working across schema renames.
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

	sessionRows, err := querySQLiteJSON(dbPath, fmt.Sprintf(
		"SELECT * FROM %s ORDER BY rowid DESC LIMIT %d;", sessionTable, sqliteScanLimit,
	))
	if err != nil || len(sessionRows) == 0 {
		return nil, nil
	}

	var sessionRow map[string]json.RawMessage
	for _, row := range sessionRows {
		flat := flattenRow(row)
		if rowHasValue(flat, projectID) {
			sessionRow = flat
			break
		}
	}
	if sessionRow == nil {
		return nil, nil
	}

	sessionID := stringFieldCI(sessionRow, "id")
	if sessionID == "" {
		return nil, nil
	}

	// Skip stale sessions; if we can't find any timestamp field, proceed
	// anyway — better to try than skip.
	if !isRecentSessionRow(sessionRow) {
		return nil, nil
	}

	messageTable := findSQLiteTable(dbPath, "message")
	if messageTable == "" {
		return nil, nil
	}

	msgRows, err := querySQLiteJSON(dbPath, fmt.Sprintf(
		"SELECT * FROM %s ORDER BY rowid ASC;", messageTable,
	))
	if err != nil {
		return nil, nil
	}

	var entries []json.RawMessage
	for _, row := range msgRows {
		flat := flattenRow(row)
		if !rowHasValue(flat, sessionID) {
			continue
		}
		data, err := json.Marshal(flat)
		if err != nil {
			continue
		}
		entries = append(entries, data)
	}
	if len(entries) == 0 {
		return nil, nil
	}

	transcriptData, err := json.Marshal(entries)
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

// findSQLiteTable returns the name of a table (or view) in dbPath matching
// hint, preferring an exact (case-insensitive) match and falling back to the
// first name containing hint as a substring (e.g. "sessions", "opencode_session").
func findSQLiteTable(dbPath, hint string) string {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type IN ('table','view');")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	hint = strings.ToLower(hint)
	var contains string
	for _, name := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if lower == hint {
			return name
		}
		if contains == "" && strings.Contains(lower, hint) {
			contains = name
		}
	}
	return contains
}

// querySQLiteJSON runs a query against dbPath and returns each row as a map
// of column name to raw JSON value.
func querySQLiteJSON(dbPath, query string) ([]map[string]json.RawMessage, error) {
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, nil
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// flattenRow merges any nested JSON blob columns (e.g. a "data"/"payload"/
// "body" column holding a JSON-encoded object, whether stored as a native
// JSON value or a double-encoded string) into the top level of the row, so
// callers can look up fields without knowing whether a given release stores
// them as flat columns or inside a blob. The row's own columns win over
// values unpacked from a blob (mirrors OpenCode's own json_patch(data, ...)
// pattern of letting the outer id column override any nested id).
func flattenRow(row map[string]json.RawMessage) map[string]json.RawMessage {
	merged := map[string]json.RawMessage{}
	blobKeys := map[string]bool{}

	for key, raw := range row {
		lower := strings.ToLower(key)
		if !strings.Contains(lower, "data") && !strings.Contains(lower, "payload") && !strings.Contains(lower, "body") {
			continue
		}
		inner := decodeJSONObject(raw)
		if inner == nil {
			continue
		}
		for k, v := range inner {
			merged[k] = v
		}
		blobKeys[key] = true
	}

	for key, raw := range row {
		if blobKeys[key] {
			continue
		}
		merged[key] = raw
	}

	return merged
}

// decodeJSONObject decodes raw as a JSON object, unwrapping one level of
// string-encoding if raw is a JSON string containing an object.
func decodeJSONObject(raw json.RawMessage) map[string]json.RawMessage {
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		if err := json.Unmarshal([]byte(s), &obj); err == nil {
			return obj
		}
	}

	return nil
}

// rowHasValue reports whether any top-level string value in row equals target.
func rowHasValue(row map[string]json.RawMessage, target string) bool {
	if target == "" {
		return false
	}
	for _, raw := range row {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s == target {
			return true
		}
	}
	return false
}

// stringFieldCI returns the string value of the field named name (matched
// case-insensitively), or "" if not found.
func stringFieldCI(row map[string]json.RawMessage, name string) string {
	for key, raw := range row {
		if !strings.EqualFold(key, name) {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	return ""
}

// isRecentSessionRow reports whether row has a timestamp-like field within
// agent.RecentSessionTimeout of now. If no timestamp field can be found at
// all, it returns true (proceed anyway — better to try than skip).
func isRecentSessionRow(row map[string]json.RawMessage) bool {
	now := time.Now()
	foundTimestamp := false
	for key, raw := range row {
		lower := strings.ToLower(key)
		if !strings.Contains(lower, "time") && !strings.Contains(lower, "updated") &&
			!strings.Contains(lower, "created") && !strings.Contains(lower, "date") {
			continue
		}
		t, ok := parseFlexibleTime(raw)
		if !ok {
			continue
		}
		foundTimestamp = true
		diff := now.Sub(t)
		if diff < 0 {
			diff = -diff
		}
		if diff <= agent.RecentSessionTimeout {
			return true
		}
	}
	return !foundTimestamp
}

// parseFlexibleTime attempts to parse raw as a timestamp, accepting either a
// numeric Unix timestamp (seconds, milliseconds, or microseconds) or a
// handful of common string time formats.
func parseFlexibleTime(raw json.RawMessage) (time.Time, bool) {
	var num float64
	if err := json.Unmarshal(raw, &num); err == nil {
		if num <= 0 {
			return time.Time{}, false
		}
		switch {
		case num > 1e15:
			return time.UnixMicro(int64(num)), true
		case num > 1e12:
			return time.UnixMilli(int64(num)), true
		default:
			return time.Unix(int64(num), 0), true
		}
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		formats := []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05.000Z",
			"2006-01-02 15:04:05",
		}
		for _, f := range formats {
			if t, err := time.Parse(f, s); err == nil {
				return t, true
			}
		}
	}

	return time.Time{}, false
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
