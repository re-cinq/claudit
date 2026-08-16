```go
package opencode

import (
	"encoding/json"
	"fmt"
	"io"
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
// Table and column names have drifted across OpenCode releases (e.g. the
// session/message tables have been renamed, and message content has moved
// between a single embedded JSON blob column and a flat "parts" column).
// Rather than hardcoding names that can go stale with any new release, we
// discover the actual schema via sqlite_master/PRAGMA table_info and adapt.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := findTable(dbPath, "session", "sessions")
	messageTable := findTable(dbPath, "message", "messages")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols := tableColumns(dbPath, sessionTable)
	sessionIDCol := findColumn(sessionCols, "id")
	if sessionIDCol == "" {
		return nil, nil
	}

	// Find the most recent session for this project. If the schema no
	// longer has a project-scoping column (or nothing matches for this
	// project), fall back to the most recently created session overall
	// rather than giving up entirely.
	sessionID := ""
	if projectCol := findColumn(sessionCols, "project_id", "projectid", "project", "directory"); projectCol != "" {
		sessionID = queryScalar(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s' ORDER BY rowid DESC LIMIT 1;`,
			sessionIDCol, sessionTable, projectCol, projectID,
		))
	}
	if sessionID == "" {
		sessionID = queryScalar(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s ORDER BY rowid DESC LIMIT 1;`,
			sessionIDCol, sessionTable,
		))
	}
	if sessionID == "" {
		return nil, nil
	}

	// Best-effort staleness check using whatever timestamp column exists.
	// If we can't find or parse a timestamp column, proceed anyway — better
	// to try than to skip a legitimately recent session.
	if timeCol := findColumn(sessionCols, "time_updated", "updated_at", "time_created", "created_at"); timeCol != "" {
		timeStr := queryScalar(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s';`,
			timeCol, sessionTable, sessionIDCol, sessionID,
		))
		if t, ok := parseSQLiteTime(timeStr); ok {
			if time.Since(t) > agent.RecentSessionTimeout {
				return nil, nil
			}
		}
	}

	// Fetch every column for this session's messages. We don't assume a
	// specific content column exists (it may be a single embedded JSON
	// blob, or flat columns with a separate "parts" array) — each row is
	// normalized below regardless of shape.
	msgCols := tableColumns(dbPath, messageTable)
	sessionFKCol := findColumn(msgCols, "session_id", "sessionid")
	if sessionFKCol == "" {
		return nil, nil
	}
	orderCol := "rowid"
	if c := findColumn(msgCols, "time_created", "created_at", "time"); c != "" {
		orderCol = c
	}

	cmd := exec.Command("sqlite3", "-json", dbPath, fmt.Sprintf(
		`SELECT * FROM %s WHERE %s='%s' ORDER BY %s;`,
		messageTable, sessionFKCol, sessionID, orderCol,
	))
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(msgOutput, &rows); err != nil || len(rows) == 0 {
		return nil, nil
	}

	messages := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, normalizeSQLiteMessageRow(row))
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

// normalizeSQLiteMessageRow flattens a raw SQLite message row (as returned by
// `sqlite3 -json`) into a single JSON object suitable for ParseTranscript.
//
// Some OpenCode versions store the entire message (role, content, etc.) as a
// single JSON string embedded in one column (e.g. "data"); others store role
// and other fields as real columns alongside a "parts" column. Either shape
// is handled here: any column whose value is itself a JSON-encoded string is
// decoded, and if that decodes to an object, its fields are merged up to the
// top level so callers don't need to know which column held them.
func normalizeSQLiteMessageRow(row map[string]json.RawMessage) json.RawMessage {
	flat := make(map[string]json.RawMessage, len(row))
	for k, v := range row {
		flat[k] = v
	}

	for _, v := range row {
		var asString string
		if err := json.Unmarshal(v, &asString); err != nil {
			continue
		}
		trimmed := strings.TrimSpace(asString)
		if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
			continue
		}
		var nested json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &nested); err != nil {
			continue
		}

		var nestedObj map[string]json.RawMessage
		if err := json.Unmarshal(nested, &nestedObj); err == nil {
			for nk, nv := range nestedObj {
				if _, exists := row[nk]; !exists {
					flat[nk] = nv
				}
			}
		}
	}

	out, err := json.Marshal(flat)
	if err != nil {
		return json.RawMessage("{}")
	}
	return out
}

// findTable returns the first candidate table name that exists in the
// SQLite database (case-insensitive), or "" if none match.
func findTable(dbPath string, candidates ...string) string {
	out := queryScalar(dbPath, "SELECT group_concat(name, char(10)) FROM sqlite_master WHERE type='table';")
	if out == "" {
		return ""
	}
	names := strings.Split(out, "\n")
	for _, want := range candidates {
		for _, name := range names {
			if strings.EqualFold(strings.TrimSpace(name), want) {
				return strings.TrimSpace(name)
			}
		}
	}
	return ""
}

// tableColumns returns the column names of a SQLite table, or nil on error.
func tableColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// findColumn returns the first candidate column name present in cols
// (case-insensitive), or "" if none match.
func findColumn(cols []string, candidates ...string) string {
	for _, want := range candidates {
		for _, c := range cols {
			if strings.EqualFold(c, want) {
				return c
			}
		}
	}
	return ""
}

// queryScalar runs a SQLite query expected to return a single value and
// returns it trimmed, or "" on error or empty result.
func queryScalar(dbPath, query string) string {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// parseSQLiteTime parses a timestamp column value in any of the formats
// OpenCode has used across versions: RFC3339(Nano), SQL datetime, or Unix
// epoch (seconds, milliseconds, or microseconds).
func parseSQLiteTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}

	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, true
		}
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		switch {
		case n > 1e14: // microseconds since epoch
			return time.UnixMicro(n), true
		case n > 1e11: // milliseconds since epoch
			return time.UnixMilli(n), true
		case n > 0: // seconds since epoch
			return time.Unix(n, 0), true
		}
	}

	return time.Time{}, false
}

// discoverFromSQLiteDB is retained for backwards compatibility with older
// call sites; it simply forwards to discoverFromSQLite.

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

	// Try "parts" as an array of typed content pieces (newer OpenCode
	// message format, e.g. {"type":"text","text":"..."} or
	// {"type":"text","data":{"text":"..."}}).
	if partsRaw, ok := raw["parts"]; ok {
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(partsRaw, &parts); err == nil && len(parts) > 0 {
			var blocks []agent.ContentBlock
			for _, part := range parts {
				text := extractPartText(part)
				if text == "" {
					continue
				}
				blocks = append(blocks, agent.ContentBlock{Type: "text", Text: text})
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

// extractPartText pulls readable text out of a single OpenCode message part,
// checking both a top-level "text" field and a nested "data.text" field.
func extractPartText(part map[string]json.RawMessage) string {
	if tRaw, ok := part["text"]; ok {
		var text string
		if err := json.Unmarshal(tRaw, &text); err == nil && text != "" {
			return text
		}
	}

	if dRaw, ok := part["data"]; ok {
		var data map[string]json.RawMessage
		if err := json.Unmarshal(dRaw, &data); err == nil {
			if tRaw, ok := data["text"]; ok {
				var text string
				if err := json.Unmarshal(tRaw, &text); err == nil && text != "" {
					return text
				}
			}
		}
	}

	return ""
}
```
