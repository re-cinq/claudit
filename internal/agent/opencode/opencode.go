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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
//
// OpenCode's SQLite schema is not assumed to be stable across versions: column
// names have been observed to vary (e.g. project_id vs projectID), and row data
// may live in dedicated columns or be nested inside a serialized JSON blob
// column (mirroring OpenCode's original flat-file storage). To stay tolerant of
// that, rows are read generically via `sqlite3 -json` and flattened so lookups
// work by field name regardless of which layout is in use.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionCols := sqliteColumns(dbPath, "session")
	projectCol := pickColumn(sessionCols, "project_id", "projectID", "project")

	sessionRows, err := sqliteSelectRows(dbPath, "session", projectCol, projectID)
	if err != nil || len(sessionRows) == 0 {
		return nil, nil
	}

	var bestRow map[string]interface{}
	var bestTime time.Time
	haveTime := false
	for _, raw := range sessionRows {
		flat := flattenRow(raw)
		if !rowMatchesProject(flat, projectID, projectPath) {
			continue
		}
		t, ok := extractTime(flat,
			"time_updated", "timeUpdated", "updated_at", "updatedAt",
			"time_created", "timeCreated", "created_at", "createdAt")
		if bestRow == nil || (ok && (!haveTime || t.After(bestTime))) {
			bestRow = flat
			if ok {
				bestTime = t
				haveTime = true
			}
		}
	}
	if bestRow == nil {
		return nil, nil
	}

	// If we found a recency timestamp, use it to skip stale sessions.
	// If we can't determine one, proceed anyway — better to try than skip.
	if haveTime && time.Since(bestTime) > agent.RecentSessionTimeout {
		return nil, nil
	}

	sessionID := extractString(bestRow, "id", "sessionID", "session_id")
	if sessionID == "" {
		return nil, nil
	}

	msgCols := sqliteColumns(dbPath, "message")
	sessionCol := pickColumn(msgCols, "session_id", "sessionID", "session")

	msgRows, err := sqliteSelectRows(dbPath, "message", sessionCol, sessionID)
	if err != nil {
		return nil, nil
	}

	type flatMsg struct {
		entry map[string]interface{}
		t     time.Time
		idx   int
	}
	flatMsgs := make([]flatMsg, 0, len(msgRows))
	for i, raw := range msgRows {
		flat := flattenRow(raw)

		// If we couldn't filter by session in SQL (no matching column found),
		// filter here using whichever field name the row actually has.
		if sessionCol == "" {
			if sid := extractString(flat, "session_id", "sessionID", "session"); sid != "" && sid != sessionID {
				continue
			}
		}

		t, _ := extractTime(flat, "time_created", "timeCreated", "created_at", "createdAt")
		flatMsgs = append(flatMsgs, flatMsg{entry: flat, t: t, idx: i})
	}

	sort.SliceStable(flatMsgs, func(i, j int) bool {
		if !flatMsgs[i].t.Equal(flatMsgs[j].t) {
			return flatMsgs[i].t.Before(flatMsgs[j].t)
		}
		return flatMsgs[i].idx < flatMsgs[j].idx
	})

	if len(flatMsgs) == 0 {
		return nil, nil
	}

	messages := make([]map[string]interface{}, len(flatMsgs))
	for i, m := range flatMsgs {
		messages[i] = m.entry
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

// sqliteColumns returns the column names of a SQLite table, or nil if the
// table doesn't exist or sqlite3 can't be queried.
func sqliteColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
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

// pickColumn returns the first candidate name present in cols, or "" if none match.
func pickColumn(cols []string, candidates ...string) string {
	set := make(map[string]bool, len(cols))
	for _, c := range cols {
		set[c] = true
	}
	for _, c := range candidates {
		if set[c] {
			return c
		}
	}
	return ""
}

// sqliteSelectRows reads all rows of a table as generic JSON objects,
// optionally filtered to rows where filterCol equals filterVal (when
// filterCol is non-empty and known to exist).
func sqliteSelectRows(dbPath, table, filterCol, filterVal string) ([]map[string]interface{}, error) {
	query := fmt.Sprintf("SELECT * FROM %s", table)
	if filterCol != "" {
		query += fmt.Sprintf(" WHERE %s='%s'", filterCol, sqlEscape(filterVal))
	}
	query += ";"

	cmd := exec.Command("sqlite3", "-json", dbPath, query)
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

func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// flattenRow merges fields nested inside any JSON-object string column (e.g. a
// "data" blob column) into the top level of the row, without overwriting
// fields that already exist there. This lets callers look up fields like
// "role" or "project_id" by name regardless of whether OpenCode stores them
// as dedicated columns or nested inside a serialized blob.
func flattenRow(row map[string]interface{}) map[string]interface{} {
	flat := make(map[string]interface{}, len(row))
	for k, v := range row {
		flat[k] = v
	}
	for _, v := range row {
		s, ok := v.(string)
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(s)
		if !strings.HasPrefix(trimmed, "{") {
			continue
		}
		var nested map[string]interface{}
		if err := json.Unmarshal([]byte(trimmed), &nested); err != nil {
			continue
		}
		for nk, nv := range nested {
			if _, exists := flat[nk]; !exists {
				flat[nk] = nv
			}
		}
	}
	return flat
}

// rowMatchesProject checks a flattened row against the project's root commit
// hash or its absolute directory path, since either may be used as the
// project identifier depending on OpenCode version.
func rowMatchesProject(row map[string]interface{}, projectID, projectPath string) bool {
	for _, key := range []string{"project_id", "projectID", "project"} {
		if v, ok := row[key].(string); ok && v == projectID {
			return true
		}
	}

	cleanPath := filepath.Clean(projectPath)
	for _, key := range []string{"directory", "path", "cwd", "worktree", "root"} {
		if v, ok := row[key].(string); ok && filepath.Clean(v) == cleanPath {
			return true
		}
	}
	return false
}

// extractString returns the first non-empty string value found among candidate keys.
func extractString(row map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := row[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// extractTime returns the first parseable timestamp found among candidate
// keys, trying common string layouts as well as Unix epoch seconds/milliseconds.
func extractTime(row map[string]interface{}, keys ...string) (time.Time, bool) {
	for _, k := range keys {
		v, ok := row[k]
		if !ok {
			continue
		}
		switch val := v.(type) {
		case string:
			for _, layout := range []string{
				time.RFC3339Nano,
				time.RFC3339,
				"2006-01-02T15:04:05.000Z",
				"2006-01-02 15:04:05",
			} {
				if t, err := time.Parse(layout, val); err == nil {
					return t, true
				}
			}
		case float64:
			if val <= 0 {
				continue
			}
			if val > 1e12 {
				return time.UnixMilli(int64(val)), true
			}
			return time.Unix(int64(val), 0), true
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
