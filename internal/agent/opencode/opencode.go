```go
package opencode

import (
	"encoding/json"
	"fmt"
	"io"
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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session belonging to this project.
//
// OpenCode's internal SQLite schema (table/column names) has changed across
// releases, so instead of hardcoding exact table and column names, we
// introspect the schema at runtime: find a table that looks like a session
// table, a table that looks like a message table, and match rows generically
// by scanning all of their fields rather than assuming one specific column
// holds the project id, timestamp, or foreign key.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := sqliteFindTable(dbPath, []string{"session", "sessions"}, "session", []string{"message", "part"})
	if sessionTable == "" {
		return nil, nil
	}

	sessionRows, err := sqliteQueryJSON(dbPath, fmt.Sprintf("SELECT * FROM %s;", sessionTable))
	if err != nil || len(sessionRows) == 0 {
		return nil, nil
	}

	// Prefer sessions whose row contains our project id somewhere. If the
	// schema doesn't key sessions by project the way we expect anymore, fall
	// back to considering every session and picking the most recent one —
	// better to guess than to silently drop a real session.
	var matched []map[string]interface{}
	for _, row := range sessionRows {
		if rowHasValue(row, projectID) {
			matched = append(matched, row)
		}
	}
	candidates := sessionRows
	if len(matched) > 0 {
		candidates = matched
	}

	var best map[string]interface{}
	bestTime := ""
	for _, row := range candidates {
		t := rowTimeValue(row)
		if best == nil || t > bestTime {
			best = row
			bestTime = t
		}
	}
	if best == nil {
		return nil, nil
	}

	sessionID, ok := fieldString(best, "id", "sessionid", "session_id")
	if !ok || sessionID == "" {
		return nil, nil
	}

	if bestTime != "" {
		if t, ok := parseFlexibleTime(bestTime); ok && time.Since(t) > agent.RecentSessionTimeout {
			return nil, nil
		}
	}

	messageTable := sqliteFindTable(dbPath, []string{"message", "messages"}, "message", []string{"part"})
	if messageTable == "" {
		return nil, nil
	}

	msgRows, err := sqliteQueryJSON(dbPath, fmt.Sprintf("SELECT * FROM %s;", messageTable))
	if err != nil || len(msgRows) == 0 {
		return nil, nil
	}

	var ownMessages []map[string]interface{}
	for _, row := range msgRows {
		if rowHasValue(row, sessionID) {
			ownMessages = append(ownMessages, row)
		}
	}
	if len(ownMessages) == 0 {
		return nil, nil
	}

	sort.SliceStable(ownMessages, func(i, j int) bool {
		return rowTimeValue(ownMessages[i]) < rowTimeValue(ownMessages[j])
	})

	messages := make([]json.RawMessage, 0, len(ownMessages))
	for _, row := range ownMessages {
		messages = append(messages, buildMessageJSON(row))
	}

	transcriptData, err := json.Marshal(messages)
	if err != nil || len(messages) == 0 {
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

// sqliteQueryJSON runs a SQL query against the sqlite3 CLI and decodes the
// result rows generically, without assuming any particular column set.
func sqliteQueryJSON(dbPath, query string) ([]map[string]interface{}, error) {
	cmd := exec.Command("sqlite3", "-json", dbPath, "PRAGMA busy_timeout=3000; "+query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, nil
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// sqliteFindTable finds a table name in the database, preferring an exact
// (case-insensitive) match against preferredNames, then falling back to the
// first table whose name contains `contains` but none of the `exclude`
// substrings. Returns "" if nothing matches.
func sqliteFindTable(dbPath string, preferredNames []string, contains string, exclude []string) string {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	var tables []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			tables = append(tables, line)
		}
	}

	for _, name := range preferredNames {
		for _, t := range tables {
			if strings.EqualFold(t, name) {
				return t
			}
		}
	}

	for _, t := range tables {
		lower := strings.ToLower(t)
		if !strings.Contains(lower, contains) {
			continue
		}
		excluded := false
		for _, ex := range exclude {
			if strings.Contains(lower, ex) {
				excluded = true
				break
			}
		}
		if !excluded {
			return t
		}
	}

	return ""
}

// fieldString returns the first value found under any of the given
// case-insensitive key names, coerced to a string.
func fieldString(row map[string]interface{}, keys ...string) (string, bool) {
	for _, k := range keys {
		for rk, v := range row {
			if !strings.EqualFold(rk, k) {
				continue
			}
			switch val := v.(type) {
			case string:
				if val != "" {
					return val, true
				}
			case float64:
				return strconv.FormatFloat(val, 'f', -1, 64), true
			}
		}
	}
	return "", false
}

// rowHasValue reports whether any field in row stringifies to exactly value.
// Used to match rows against a foreign key (project id, session id) without
// needing to know which column holds it.
func rowHasValue(row map[string]interface{}, value string) bool {
	if value == "" {
		return false
	}
	for _, v := range row {
		switch val := v.(type) {
		case string:
			if val == value {
				return true
			}
		case float64:
			if strconv.FormatFloat(val, 'f', -1, 64) == value {
				return true
			}
		}
	}
	return false
}

// rowTimeValue returns a lexicographically-comparable representation of the
// most "recent-looking" time-ish field in row (any key containing "time",
// "updated", or "created"), for sorting/recency purposes. Numeric epoch
// values are zero-padded so they sort correctly as strings. Returns "" if no
// such field is found.
func rowTimeValue(row map[string]interface{}) string {
	best := ""
	for k, v := range row {
		lower := strings.ToLower(k)
		if !strings.Contains(lower, "time") && !strings.Contains(lower, "updated") && !strings.Contains(lower, "created") {
			continue
		}

		var s string
		switch val := v.(type) {
		case string:
			s = val
		case float64:
			s = fmt.Sprintf("%020.0f", val)
		default:
			continue
		}

		if s > best {
			best = s
		}
	}
	return best
}

// parseFlexibleTime parses a timestamp produced by rowTimeValue, trying
// common string formats first and falling back to epoch seconds/milliseconds.
func parseFlexibleTime(s string) (time.Time, bool) {
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

	digits := strings.TrimLeft(s, "0")
	if digits == "" {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	if len(digits) >= 13 {
		return time.UnixMilli(n), true
	}
	return time.Unix(n, 0), true
}

// buildMessageJSON reconstructs the JSON payload for a message row. Some
// schemas nest the actual message (role/content/time/etc.) inside a text
// column (e.g. "data") as JSON-encoded text; others store those fields as
// flat columns directly on the row. We prefer the nested blob when present
// (patching in the row's id if the blob lacks one), and fall back to
// marshaling the row itself.
func buildMessageJSON(row map[string]interface{}) json.RawMessage {
	if raw, ok := fieldString(row, "data"); ok {
		var inner map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &inner); err == nil {
			if _, hasID := inner["id"]; !hasID {
				if id, ok := fieldString(row, "id"); ok {
					inner["id"] = id
				}
			}
			if data, err := json.Marshal(inner); err == nil {
				return data
			}
		}
	}

	data, err := json.Marshal(row)
	if err != nil {
		return json.RawMessage("{}")
	}
	return data
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
