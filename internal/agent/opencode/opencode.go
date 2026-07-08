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
// OpenCode's SQLite schema is not guaranteed to expose typed SQL columns for
// fields like the project id or timestamps (recent versions keep most fields
// inside a JSON blob column, similar to how the message table already stores
// its payload in a "data" column). Querying a column that doesn't exist
// causes sqlite3 to exit non-zero, which previously made discovery silently
// return nothing. To stay resilient across schema changes, rows are pulled
// generically via `sqlite3 -json` and inspected in Go instead of relying on
// specific SQL column names.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessions, err := sqliteJSONRows(dbPath, "session", "sessions")
	if err != nil || len(sessions) == 0 {
		return nil, nil
	}

	var (
		bestID   string
		bestTime time.Time
		found    bool
	)

	for _, row := range sessions {
		if !rowMatchesProject(row, projectID, projectPath) {
			continue
		}
		id, _ := row["id"].(string)
		if id == "" {
			continue
		}
		t := rowTimestamp(row, "time_updated", "timeUpdated", "updated", "time", "time_created", "timeCreated", "created")
		if !found || t.After(bestTime) {
			bestID, bestTime, found = id, t, true
		}
	}

	if !found {
		return nil, nil
	}

	// Check if this session was recent (within timeout). If we couldn't
	// determine a timestamp at all, proceed anyway — better to try than skip.
	if !bestTime.IsZero() && time.Since(bestTime) > agent.RecentSessionTimeout {
		return nil, nil
	}

	messages, err := sqliteJSONRows(dbPath, "message", "messages")
	if err != nil {
		return nil, nil
	}

	var sessionMessages []map[string]interface{}
	for _, m := range messages {
		if rowBelongsToSession(m, bestID) {
			sessionMessages = append(sessionMessages, m)
		}
	}

	if len(sessionMessages) == 0 {
		return nil, nil
	}

	sort.Slice(sessionMessages, func(i, j int) bool {
		ti := rowTimestamp(sessionMessages[i], "time_created", "timeCreated", "created", "time", "time_updated", "timeUpdated", "updated")
		tj := rowTimestamp(sessionMessages[j], "time_created", "timeCreated", "created", "time", "time_updated", "timeUpdated", "updated")
		return ti.Before(tj)
	})

	transcriptData, err := json.Marshal(sessionMessages)
	if err != nil {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// sqliteJSONRows queries each candidate table name (in order) until one
// succeeds, returning its rows as generic JSON objects. This avoids
// hardcoding a single table name that may not match the running OpenCode
// version's schema. Any nested "data" column (OpenCode's JSON-blob storage
// convention) is flattened into the row so its fields are addressable like
// any other column.
func sqliteJSONRows(dbPath string, tableNames ...string) ([]map[string]interface{}, error) {
	var lastErr error
	for _, table := range tableNames {
		cmd := exec.Command("sqlite3", "-json", dbPath, fmt.Sprintf("SELECT * FROM %s;", table))
		output, err := cmd.Output()
		if err != nil {
			lastErr = err
			continue
		}

		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return nil, nil
		}

		var rows []map[string]interface{}
		if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
			lastErr = err
			continue
		}

		for _, row := range rows {
			mergeDataBlob(row)
		}

		return rows, nil
	}

	return nil, lastErr
}

// mergeDataBlob flattens a JSON-encoded "data" column (if present) into the
// row so its fields are addressable alongside real SQL columns.
func mergeDataBlob(row map[string]interface{}) {
	blob, ok := row["data"].(string)
	if !ok || blob == "" {
		return
	}

	var inner map[string]interface{}
	if err := json.Unmarshal([]byte(blob), &inner); err != nil {
		return
	}

	for k, v := range inner {
		if _, exists := row[k]; !exists {
			row[k] = v
		}
	}
}

// rowMatchesProject reports whether a session row belongs to the given
// project, checking common field name variants for the project identifier
// or working directory. If none of those fields can be found on the row, it
// is accepted so discovery can still fall back to "most recently updated
// session" rather than finding nothing at all.
func rowMatchesProject(row map[string]interface{}, projectID, projectPath string) bool {
	for _, key := range []string{"projectID", "project_id", "projectId"} {
		if v, ok := row[key].(string); ok && v != "" {
			return v == projectID
		}
	}

	for _, key := range []string{"directory", "cwd", "path", "worktree"} {
		if v, ok := row[key].(string); ok && v != "" {
			return v == projectPath
		}
	}

	return true
}

// rowBelongsToSession reports whether a message row belongs to the given
// session, checking common field name variants for the session reference.
func rowBelongsToSession(row map[string]interface{}, sessionID string) bool {
	for _, key := range []string{"session_id", "sessionID", "sessionId"} {
		if v, ok := row[key].(string); ok {
			return v == sessionID
		}
	}
	return false
}

// rowTimestamp looks up the first matching timestamp field among keys
// (checked both at the top level and nested under a "time" object) and
// parses it as a Unix timestamp (seconds or milliseconds) or a common
// string time format. Returns the zero Time if nothing could be parsed.
func rowTimestamp(row map[string]interface{}, keys ...string) time.Time {
	if t, ok := parseTimestampFrom(row, keys...); ok {
		return t
	}

	if nested, ok := row["time"].(map[string]interface{}); ok {
		if t, ok := parseTimestampFrom(nested, keys...); ok {
			return t
		}
	}

	return time.Time{}
}

// parseTimestampFrom checks the given keys on m in order and returns the
// first value that parses as a timestamp.
func parseTimestampFrom(m map[string]interface{}, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		v, ok := m[key]
		if !ok {
			continue
		}
		if t, ok := parseTimestampValue(v); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseTimestampValue parses a JSON-decoded value as either a Unix
// timestamp (seconds or milliseconds, as a JSON number) or a common
// string time format.
func parseTimestampValue(v interface{}) (time.Time, bool) {
	switch val := v.(type) {
	case float64:
		if val <= 0 {
			return time.Time{}, false
		}
		if val > 1e12 {
			return time.UnixMilli(int64(val)), true
		}
		return time.Unix(int64(val), 0), true
	case string:
		layouts := []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05.000Z",
			"2006-01-02 15:04:05",
		}
		for _, layout := range layouts {
			if t, err := time.Parse(layout, val); err == nil {
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
```
