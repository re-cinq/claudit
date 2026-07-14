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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
// OpenCode's SQLite schema has changed across releases (column/table renames as the
// CLI evolves), so this first tries the historically-known column names and falls
// back to introspecting the actual schema at runtime if that yields nothing.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionID := findRecentSessionIDExact(dbPath, projectID)
	if sessionID == "" {
		sessionID = findRecentSessionIDBySchema(dbPath, projectID, projectPath)
	}
	if sessionID == "" {
		return nil, nil
	}

	transcriptData := queryMessagesForSession(dbPath, sessionID)
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

// findRecentSessionIDExact tries the historically-known "session" schema: a
// project_id column and a time_updated column. Returns "" if the schema
// doesn't match or no recent session is found, so the caller can fall back
// to schema introspection.
func findRecentSessionIDExact(dbPath, projectID string) string {
	sessionQuery := fmt.Sprintf(
		`SELECT id FROM session WHERE project_id='%s' ORDER BY time_updated DESC LIMIT 1;`,
		projectID,
	)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return ""
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Check if this session was recent (within timeout)
	timeQuery := fmt.Sprintf(`SELECT time_updated FROM session WHERE id='%s';`, sessionID)
	cmd = exec.Command("sqlite3", dbPath, timeQuery)
	if timeOutput, err := cmd.Output(); err == nil {
		if t, ok := parseFlexibleTime(strings.TrimSpace(string(timeOutput))); ok {
			if time.Since(t) > agent.RecentSessionTimeout {
				return ""
			}
		}
		// If we can't parse the time, proceed anyway — better to try than skip
	}

	return sessionID
}

// findRecentSessionIDBySchema discovers the session table's actual columns at
// runtime and matches on whichever identity/timestamp fields are present. This
// tolerates OpenCode renaming or restructuring the session table across releases
// (e.g. project_id -> directory, time_updated -> updated).
func findRecentSessionIDBySchema(dbPath, projectID, projectPath string) string {
	cmd := exec.Command("sqlite3", "-json", dbPath, "SELECT * FROM session;")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal(output, &rows); err != nil || len(rows) == 0 {
		return ""
	}

	var bestID string
	var bestTime time.Time
	bestIsMatch := false

	for _, row := range rows {
		id, _ := row["id"].(string)
		if id == "" {
			continue
		}
		isMatch := rowMatchesProject(row, projectID, projectPath)
		t := rowTimestamp(row)

		switch {
		case isMatch && !bestIsMatch:
			bestID, bestTime, bestIsMatch = id, t, true
		case isMatch == bestIsMatch && t.After(bestTime):
			bestID, bestTime = id, t
		}
	}

	if bestID == "" {
		return ""
	}
	if !bestTime.IsZero() && time.Since(bestTime) > agent.RecentSessionTimeout {
		return ""
	}
	return bestID
}

// rowMatchesProject checks whether a session row identifies this project,
// trying every plausible column name OpenCode has used for project/directory
// identity across releases.
func rowMatchesProject(row map[string]interface{}, projectID, projectPath string) bool {
	for _, key := range []string{"project_id", "projectID", "project", "directory", "worktree", "cwd", "path"} {
		val, ok := row[key].(string)
		if !ok || val == "" {
			continue
		}
		if val == projectID || val == projectPath {
			return true
		}
	}
	return false
}

// rowTimestamp extracts a comparable timestamp from a session row, trying every
// plausible column name OpenCode has used across releases.
func rowTimestamp(row map[string]interface{}) time.Time {
	for _, key := range []string{"time_updated", "updated", "time", "time_created", "created"} {
		raw, ok := row[key]
		if !ok {
			continue
		}
		if t, ok := parseFlexibleTime(raw); ok {
			return t
		}
	}
	return time.Time{}
}

// parseFlexibleTime parses a timestamp that may be a Unix epoch (seconds or
// milliseconds), an RFC3339-ish string, or a nested {"created":..,"updated":..}
// object, matching the various encodings OpenCode has stored timestamps in.
func parseFlexibleTime(raw interface{}) (time.Time, bool) {
	switch v := raw.(type) {
	case float64:
		if v <= 0 {
			return time.Time{}, false
		}
		if v > 1e12 {
			return time.UnixMilli(int64(v)), true
		}
		return time.Unix(int64(v), 0), true
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return time.Time{}, false
		}
		layouts := []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05.000Z",
			"2006-01-02 15:04:05",
		}
		for _, layout := range layouts {
			if t, err := time.Parse(layout, v); err == nil {
				return t, true
			}
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(v), &obj); err == nil {
			if updated, ok := obj["updated"]; ok {
				if t, ok := parseFlexibleTime(updated); ok {
					return t, true
				}
			}
			if created, ok := obj["created"]; ok {
				return parseFlexibleTime(created)
			}
		}
	case map[string]interface{}:
		if updated, ok := v["updated"]; ok {
			if t, ok := parseFlexibleTime(updated); ok {
				return t, true
			}
		}
		if created, ok := v["created"]; ok {
			return parseFlexibleTime(created)
		}
	}
	return time.Time{}, false
}

// queryMessagesForSession fetches all messages for a session as a JSON array.
// It first tries the historically-known "message" table shape, then falls back
// to schema introspection if that yields nothing.
func queryMessagesForSession(dbPath, sessionID string) []byte {
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE session_id='%s' ORDER BY time_created;`,
		sessionID,
	)
	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	if output, err := cmd.Output(); err == nil {
		data := strings.TrimSpace(string(output))
		// sqlite3 returns "[null]" when no rows match
		if data != "" && data != "[null]" && data != "[]" {
			return []byte(data)
		}
	}

	return queryMessagesBySchema(dbPath, sessionID)
}

// queryMessagesBySchema discovers the message table's actual columns at
// runtime, tolerating a renamed session-id column or a differently-shaped
// data column.
func queryMessagesBySchema(dbPath, sessionID string) []byte {
	cmd := exec.Command("sqlite3", "-json", dbPath, "SELECT * FROM message;")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal(output, &rows); err != nil || len(rows) == 0 {
		return nil
	}

	sessionIDKey := ""
	for _, key := range []string{"session_id", "sessionID", "session"} {
		if _, ok := rows[0][key]; ok {
			sessionIDKey = key
			break
		}
	}
	if sessionIDKey == "" {
		return nil
	}

	var messages []map[string]interface{}
	for _, row := range rows {
		sid, _ := row[sessionIDKey].(string)
		if sid != sessionID {
			continue
		}

		if raw, ok := row["data"].(string); ok && raw != "" {
			var inner map[string]interface{}
			if err := json.Unmarshal([]byte(raw), &inner); err == nil {
				if _, hasID := inner["id"]; !hasID {
					inner["id"] = row["id"]
				}
				messages = append(messages, inner)
				continue
			}
		}
		messages = append(messages, row)
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
