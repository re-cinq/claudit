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
// OpenCode's on-disk schema has drifted across releases (table/column names,
// where the database file lives, and even whether sessions are scoped by a
// project identifier at all). Rather than hardcoding one schema snapshot, we
// introspect the database at runtime (sqlite_master / PRAGMA table_info) and
// adapt the query to whatever tables/columns are actually present. This keeps
// discovery working as OpenCode's storage layer evolves between versions.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findOpenCodeDB(dataDir, projectPath)
	if dbPath == "" {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	tables := sqliteTableNames(dbPath)
	sessionTable := findTable(tables, "session", "sessions")
	messageTable := findTable(tables, "message", "messages")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols := sqliteColumnNames(dbPath, sessionTable)
	updatedCol := pickColumn(sessionCols, "time_updated", "updated_at", "updatedAt", "time_updated_ms")
	orderBy := updatedCol
	if orderBy == "" {
		orderBy = "rowid"
	}

	// Prefer a directory/cwd column when present — it holds the project's
	// absolute path directly and is more reliable than reconstructing a
	// hash-based project identifier. Fall back to project_id-style columns.
	projectCol := pickColumn(sessionCols, "directory", "cwd", "project_id", "projectID")

	sessionID, updatedRaw := findSessionRow(dbPath, sessionTable, orderBy, projectCol, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	if updatedRaw != "" {
		if t, ok := parseOpenCodeTimestamp(updatedRaw); ok {
			if time.Since(t) > agent.RecentSessionTimeout {
				return nil, nil
			}
		}
		// If we can't parse the time, proceed anyway — better to try than skip
	}

	transcriptData, err := fetchSQLiteMessages(dbPath, messageTable, sessionID)
	if err != nil || len(transcriptData) == 0 {
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

// findSessionRow returns the most recently updated session id (and its raw
// updated-at value, if available) for the project. If a project-scoping
// column is known, it's tried first; if that yields nothing (wrong scoping
// scheme, or a project-local database that doesn't need scoping at all), we
// fall back to simply the most recently updated session in the database.
func findSessionRow(dbPath, table, orderBy, projectCol, projectID, projectPath string) (id, updated string) {
	selectCols := "id"
	if orderBy != "id" && orderBy != "rowid" {
		selectCols = "id, " + orderBy
	}

	var queries []string
	if projectCol != "" {
		val := projectID
		switch projectCol {
		case "directory", "cwd":
			val = projectPath
		}
		queries = append(queries, fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
			selectCols, table, projectCol, sqliteEscape(val), orderBy))
	}
	queries = append(queries, fmt.Sprintf(
		`SELECT %s FROM %s ORDER BY %s DESC LIMIT 1;`,
		selectCols, table, orderBy))

	for _, q := range queries {
		out, err := sqliteQuery(dbPath, q)
		if err != nil || out == "" {
			continue
		}
		parts := strings.SplitN(out, "|", 2)
		candidateID := strings.TrimSpace(parts[0])
		if candidateID == "" {
			continue
		}
		id = candidateID
		if len(parts) > 1 {
			updated = strings.TrimSpace(parts[1])
		}
		return id, updated
	}
	return "", ""
}

// fetchSQLiteMessages builds and runs a query that returns all messages for
// a session as a single JSON array, adapting to whichever message schema is
// present: either a single JSON blob column (older OpenCode versions), or
// separate role/parts/model columns (newer versions).
func fetchSQLiteMessages(dbPath, table, sessionID string) ([]byte, error) {
	cols := sqliteColumnNames(dbPath, table)

	sessionCol := pickColumn(cols, "session_id", "sessionID", "session")
	if sessionCol == "" {
		sessionCol = "session_id"
	}

	orderCol := pickColumn(cols, "time_created", "created_at", "createdAt")
	if orderCol == "" {
		orderCol = "rowid"
	}

	var query string
	if blobCol := pickColumn(cols, "data", "content", "body"); blobCol != "" {
		query = fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', id))) FROM %s WHERE %s='%s' ORDER BY %s;`,
			blobCol, table, sessionCol, sqliteEscape(sessionID), orderCol,
		)
	} else {
		fields := []string{"'id', id"}
		if roleCol := pickColumn(cols, "role"); roleCol != "" {
			fields = append(fields, fmt.Sprintf("'role', %s", roleCol))
		}
		if partsCol := pickColumn(cols, "parts"); partsCol != "" {
			fields = append(fields, fmt.Sprintf("'parts', json(%s)", partsCol))
		}
		if modelCol := pickColumn(cols, "model"); modelCol != "" {
			fields = append(fields, fmt.Sprintf("'model', %s", modelCol))
		}
		fields = append(fields, fmt.Sprintf("'time', %s", orderCol))
		query = fmt.Sprintf(
			`SELECT json_group_array(json_object(%s)) FROM %s WHERE %s='%s' ORDER BY %s;`,
			strings.Join(fields, ", "), table, sessionCol, sqliteEscape(sessionID), orderCol,
		)
	}

	out, err := sqliteQuery(dbPath, query)
	if err != nil {
		return nil, err
	}
	// sqlite3 returns "[null]" when no rows match
	if out == "" || out == "[null]" || out == "[]" {
		return nil, nil
	}
	return []byte(out), nil
}

// sqliteQuery runs a single-shot sqlite3 query and returns trimmed stdout.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sqliteTableNames returns the set of table names present in the database.
func sqliteTableNames(dbPath string) map[string]bool {
	names := map[string]bool{}
	out, err := sqliteQuery(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return names
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names[line] = true
		}
	}
	return names
}

// sqliteColumnNames returns the set of column names for a table.
func sqliteColumnNames(dbPath, table string) map[string]bool {
	cols := map[string]bool{}
	out, err := sqliteQuery(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return cols
	}
	// Each row: cid|name|type|notnull|dflt_value|pk
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 && fields[1] != "" {
			cols[fields[1]] = true
		}
	}
	return cols
}

// findTable returns the first candidate name present in tables, or "".
func findTable(tables map[string]bool, candidates ...string) string {
	for _, c := range candidates {
		if tables[c] {
			return c
		}
	}
	return ""
}

// pickColumn returns the first candidate name present in cols, or "".
func pickColumn(cols map[string]bool, candidates ...string) string {
	for _, c := range candidates {
		if cols[c] {
			return c
		}
	}
	return ""
}

// sqliteEscape escapes single quotes for safe inline use in a sqlite3 string literal.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// parseOpenCodeTimestamp parses a timestamp value that may be one of several
// known string formats, or a Unix epoch integer at second/millisecond/
// microsecond/nanosecond resolution (OpenCode has used both string and
// numeric timestamp columns across versions).
func parseOpenCodeTimestamp(raw string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", raw); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02 15:04:05", raw); err == nil {
		return t, true
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		switch {
		case n > 1e17: // nanoseconds
			return time.Unix(0, n), true
		case n > 1e14: // microseconds
			return time.Unix(0, n*1e3), true
		case n > 1e11: // milliseconds
			return time.Unix(0, n*1e6), true
		default: // seconds
			return time.Unix(n, 0), true
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
		} else {
			// "time" may also be a raw scalar (e.g. epoch integer) rather
			// than an object with a "created" field.
			var scalar json.Number
			if err := json.Unmarshal(timeRaw, &scalar); err == nil {
				entry.Timestamp = scalar.String()
			}
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

	// Try "parts" — newer OpenCode versions represent message content as a
	// typed parts array (text/reasoning/tool_call/tool_result/finish) rather
	// than a flat "content" field.
	if partsRaw, ok := raw["parts"]; ok {
		if blocks := parseOpenCodeParts(partsRaw); len(blocks) > 0 {
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

// parseOpenCodeParts converts a typed OpenCode "parts" array into content blocks.
func parseOpenCodeParts(partsRaw json.RawMessage) []agent.ContentBlock {
	var parts []struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(partsRaw, &parts); err != nil {
		return nil
	}

	var blocks []agent.ContentBlock
	for _, p := range parts {
		switch p.Type {
		case "text":
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(p.Data, &d) == nil && d.Text != "" {
				blocks = append(blocks, agent.ContentBlock{Type: "text", Text: d.Text})
			}
		case "reasoning":
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(p.Data, &d) == nil && d.Text != "" {
				blocks = append(blocks, agent.ContentBlock{Type: "thinking", Thinking: d.Text})
			}
		case "tool_call":
			var d struct {
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if json.Unmarshal(p.Data, &d) == nil {
				blocks = append(blocks, agent.ContentBlock{
					Type:  "tool_use",
					ID:    d.ID,
					Name:  d.Name,
					Input: d.Input,
				})
			}
		case "tool_result":
			var d struct {
				ID     string          `json:"id"`
				Output json.RawMessage `json:"output"`
			}
			if json.Unmarshal(p.Data, &d) == nil {
				blocks = append(blocks, agent.ContentBlock{
					Type:      "tool_result",
					ToolUseID: d.ID,
					Content:   d.Output,
				})
			}
		}
	}
	return blocks
}
```
