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
//
// OpenCode's SQLite schema has drifted across releases (column names, and
// whether fields live in dedicated columns vs. a JSON blob column), so this
// avoids hardcoding a schema: it dumps rows as JSON (sqlite3 -json), matches
// the project via whichever field looks like a project id/directory, and
// falls back to the most recently touched session if nothing matches.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionID, sessionTime := findRecentSessionSQLite(dbPath, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	// If we know the session's last-updated time, skip stale sessions.
	// If we couldn't determine a time, proceed anyway — better to try than skip.
	if !sessionTime.IsZero() && time.Since(sessionTime) > agent.RecentSessionTimeout {
		return nil, nil
	}

	msgCols := sqliteColumnSet(dbPath, "message")
	sessionCol := firstColumn(msgCols, "session_id", "sessionID", "sessionId", "session")
	if sessionCol == "" {
		sessionCol = "session_id"
	}
	orderCol := firstColumn(msgCols, "time_created", "timeCreated", "created_at", "createdAt")

	var msgQuery string
	if orderCol != "" {
		msgQuery = fmt.Sprintf(`SELECT * FROM message WHERE %s='%s' ORDER BY %s;`, sessionCol, sessionID, orderCol)
	} else {
		msgQuery = fmt.Sprintf(`SELECT * FROM message WHERE %s='%s';`, sessionCol, sessionID)
	}

	cmd := exec.Command("sqlite3", "-json", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	if len(transcriptData) == 0 || string(transcriptData) == "[]" || string(transcriptData) == "null" {
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

// findRecentSessionSQLite returns the id (and last-updated time, if known) of
// the most recent session belonging to projectID/projectPath. It scans rows
// most-recent-first (by rowid) and returns the first project match, falling
// back to the single most recent session overall if none match.
func findRecentSessionSQLite(dbPath, projectID, projectPath string) (string, time.Time) {
	cmd := exec.Command("sqlite3", "-json", dbPath, "SELECT * FROM session ORDER BY rowid DESC LIMIT 500;")
	out, err := cmd.Output()
	if err != nil {
		return "", time.Time{}
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) == 0 {
		return "", time.Time{}
	}

	var fallbackID string
	var fallbackTime time.Time
	for i, row := range rows {
		flat := flattenSQLiteRow(row)
		id := rawSQLiteString(flat, "id")
		if id == "" {
			continue
		}
		t := extractSessionTime(flat)
		if i == 0 {
			fallbackID, fallbackTime = id, t
		}
		if fieldMatchesAny(flat, projectID, "projectID", "project_id", "projectId") ||
			fieldMatchesAny(flat, projectPath, "directory", "worktree", "path", "cwd") {
			return id, t
		}
	}

	return fallbackID, fallbackTime
}

// sqliteColumnSet returns the set of column names for a table, or an empty
// set if introspection fails (e.g. table doesn't exist).
func sqliteColumnSet(dbPath, table string) map[string]bool {
	cols := map[string]bool{}
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return cols
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) >= 2 {
			cols[parts[1]] = true
		}
	}
	return cols
}

// firstColumn returns the first candidate present in cols, or "" if none match.
func firstColumn(cols map[string]bool, candidates ...string) string {
	for _, c := range candidates {
		if cols[c] {
			return c
		}
	}
	return ""
}

// flattenSQLiteRow merges a row's top-level fields with any nested "data"
// blob. OpenCode stores some tables' payloads as dedicated columns and
// others as a JSON-encoded "data" column (either a JSON object or a
// JSON-encoded string). Top-level fields take precedence over data-blob
// fields.
func flattenSQLiteRow(row map[string]json.RawMessage) map[string]json.RawMessage {
	flat := make(map[string]json.RawMessage, len(row))
	for k, v := range row {
		if k == "data" {
			continue
		}
		flat[k] = v
	}

	dataRaw, ok := row["data"]
	if !ok {
		return flat
	}

	var nested map[string]json.RawMessage
	if err := json.Unmarshal(dataRaw, &nested); err != nil {
		var s string
		if err := json.Unmarshal(dataRaw, &s); err == nil && s != "" {
			_ = json.Unmarshal([]byte(s), &nested)
		}
	}
	for k, v := range nested {
		if _, exists := flat[k]; !exists {
			flat[k] = v
		}
	}
	return flat
}

// rawSQLiteString extracts a string value for key from a flattened row.
func rawSQLiteString(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// fieldMatchesAny reports whether any of keys holds the value want.
func fieldMatchesAny(m map[string]json.RawMessage, want string, keys ...string) bool {
	if want == "" {
		return false
	}
	for _, k := range keys {
		if rawSQLiteString(m, k) == want {
			return true
		}
	}
	return false
}

// extractSessionTime resolves a "last updated" time from a flattened session
// row, checking a nested time object ({"updated":...,"created":...}) as well
// as common flat column names, in either epoch or string form.
func extractSessionTime(m map[string]json.RawMessage) time.Time {
	if timeRaw, ok := m["time"]; ok {
		var timeObj map[string]json.RawMessage
		if err := json.Unmarshal(timeRaw, &timeObj); err == nil {
			for _, k := range []string{"updated", "created"} {
				if v, ok := timeObj[k]; ok {
					if t, ok := parseFlexibleTime(v); ok {
						return t
					}
				}
			}
		}
	}

	for _, k := range []string{
		"time_updated", "timeUpdated", "updated_at", "updatedAt",
		"time_created", "timeCreated", "created_at", "createdAt",
	} {
		if v, ok := m[k]; ok {
			if t, ok := parseFlexibleTime(v); ok {
				return t
			}
		}
	}

	return time.Time{}
}

// parseFlexibleTime parses a JSON value as a timestamp, accepting epoch
// numbers (seconds, milliseconds, or microseconds) or common string formats.
func parseFlexibleTime(raw json.RawMessage) (time.Time, bool) {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
		switch {
		case n > 1e15:
			return time.UnixMicro(n), true
		case n > 1e12:
			return time.UnixMilli(n), true
		default:
			return time.Unix(n, 0), true
		}
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		formats := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
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
	// Messages sourced from SQLite may nest their payload under a "data"
	// column instead of exposing it as top-level fields; flattening is a
	// no-op for flat-file-sourced messages that don't have a "data" field.
	raw = flattenSQLiteRow(raw)

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
