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
// OpenCode's on-disk schema has shifted across releases (table/column names,
// whether sessions carry a project_id column vs. living in a per-project
// database file, string vs. integer timestamps, and a "data" blob vs. a
// typed "parts" array for message content). Rather than hardcoding one shape,
// this introspects the actual database at runtime so it keeps working across
// those changes.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	var dbPath string
	for _, candidate := range []string{
		filepath.Join(projectPath, ".opencode", "opencode.db"),
		filepath.Join(dataDir, "project", projectID, "opencode.db"),
		filepath.Join(dataDir, "opencode.db"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			dbPath = candidate
			break
		}
	}
	if dbPath == "" {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := sqliteFirstTable(dbPath, "sessions", "session")
	messageTable := sqliteFirstTable(dbPath, "messages", "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols := sqliteColumns(dbPath, sessionTable)
	sessionIDCol := sqliteFirstColumn(sessionCols, "id")
	if sessionIDCol == "" {
		return nil, nil
	}
	orderCol := sqliteFirstColumn(sessionCols, "time_updated", "updated_at", "time_created", "created_at")
	if orderCol == "" {
		orderCol = sessionIDCol
	}
	projectCol := sqliteFirstColumn(sessionCols, "project_id", "directory", "project")

	// Find most recent session, scoped to this project if the schema supports it
	// (a per-project database file, as used by newer OpenCode releases, needs no
	// such filter).
	var sessionQuery string
	if projectCol != "" {
		sessionQuery = fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
			sessionIDCol, sessionTable, projectCol, projectID, orderCol,
		)
	} else {
		sessionQuery = fmt.Sprintf(
			`SELECT %s FROM %s ORDER BY %s DESC LIMIT 1;`,
			sessionIDCol, sessionTable, orderCol,
		)
	}

	sessionID, err := sqliteQuery(dbPath, sessionQuery)
	if err != nil || sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout), using whatever
	// timestamp column the schema has.
	if tsRaw, err := sqliteQuery(dbPath, fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s='%s';`, orderCol, sessionTable, sessionIDCol, sessionID)); err == nil {
		if t, ok := parseOpenCodeTimestamp(tsRaw); ok {
			if time.Since(t) > agent.RecentSessionTimeout {
				return nil, nil
			}
		}
		// If we can't parse the time, proceed anyway — better to try than skip.
	}

	messageCols := sqliteColumns(dbPath, messageTable)
	msgIDCol := sqliteFirstColumn(messageCols, "id")
	roleCol := sqliteFirstColumn(messageCols, "role")
	sessionFKCol := sqliteFirstColumn(messageCols, "session_id", "sessionId", "sessionID")
	createdCol := sqliteFirstColumn(messageCols, "time_created", "created_at", "time_updated", "updated_at")
	contentCol := sqliteFirstColumn(messageCols, "parts", "data", "content")
	if msgIDCol == "" || sessionFKCol == "" || contentCol == "" {
		return nil, nil
	}
	if createdCol == "" {
		createdCol = msgIDCol
	}
	roleExpr := "'unknown'"
	if roleCol != "" {
		roleExpr = roleCol
	}

	// Get messages for this session as a JSON array. Newer OpenCode releases
	// store message content as a typed "parts" array in its own column;
	// older releases stored the whole message as a JSON blob in "data".
	var msgQuery string
	if contentCol == "parts" {
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(json_object('id', %s, 'role', %s, 'parts', json(%s))) FROM %s WHERE %s='%s' ORDER BY %s;`,
			msgIDCol, roleExpr, contentCol, messageTable, sessionFKCol, sessionID, createdCol,
		)
	} else {
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s' ORDER BY %s;`,
			contentCol, msgIDCol, messageTable, sessionFKCol, sessionID, createdCol,
		)
	}

	msgOutput, err := sqliteQuery(dbPath, msgQuery)
	if err != nil {
		return nil, nil
	}

	// sqlite3 returns "[null]" when no rows match
	if msgOutput == "" || msgOutput == "[null]" || msgOutput == "[]" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: []byte(msgOutput),
	}, nil
}

// sqliteQuery runs a single query against a SQLite database via the sqlite3
// CLI and returns the trimmed output.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sqliteTableExists reports whether a table with the given name exists.
func sqliteTableExists(dbPath, table string) bool {
	out, err := sqliteQuery(dbPath, fmt.Sprintf(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='%s';`, table))
	return err == nil && out == table
}

// sqliteFirstTable returns the first candidate table name that exists in the database.
func sqliteFirstTable(dbPath string, candidates ...string) string {
	for _, c := range candidates {
		if sqliteTableExists(dbPath, c) {
			return c
		}
	}
	return ""
}

// sqliteColumns returns the set of column names for a table.
func sqliteColumns(dbPath, table string) map[string]bool {
	out, err := sqliteQuery(dbPath, fmt.Sprintf(`PRAGMA table_info(%s);`, table))
	cols := make(map[string]bool)
	if err != nil {
		return cols
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			cols[fields[1]] = true
		}
	}
	return cols
}

// sqliteFirstColumn returns the first candidate column name present in cols.
func sqliteFirstColumn(cols map[string]bool, candidates ...string) string {
	for _, c := range candidates {
		if cols[c] {
			return c
		}
	}
	return ""
}

// parseOpenCodeTimestamp parses a session timestamp that may be an integer
// (epoch seconds or milliseconds) or one of several string date formats.
func parseOpenCodeTimestamp(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		if n > 1e12 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
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

	// Newer OpenCode releases store message content as a "parts" array of
	// typed entries (text, tool_call, tool_result, reasoning, finish) rather
	// than a flat "content" field.
	if partsRaw, ok := raw["parts"]; ok {
		if blocks := parseOpenCodeParts(partsRaw); len(blocks) > 0 {
			msg.Content = blocks
			return msg
		}
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

// openCodePart represents a single typed entry in an OpenCode "parts" array.
type openCodePart struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// parseOpenCodeParts converts OpenCode's typed "parts" array into content blocks.
func parseOpenCodeParts(raw json.RawMessage) []agent.ContentBlock {
	var parts []openCodePart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}

	var blocks []agent.ContentBlock
	for _, part := range parts {
		var data struct {
			Text       string          `json:"text"`
			ID         string          `json:"id"`
			Name       string          `json:"name"`
			Input      json.RawMessage `json:"input"`
			ToolCallID string          `json:"tool_call_id"`
			Output     json.RawMessage `json:"output"`
			Result     json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(part.Data, &data)

		switch part.Type {
		case "text":
			if data.Text != "" {
				blocks = append(blocks, agent.ContentBlock{Type: "text", Text: data.Text})
			}
		case "reasoning":
			if data.Text != "" {
				blocks = append(blocks, agent.ContentBlock{Type: "thinking", Thinking: data.Text})
			}
		case "tool_call":
			blocks = append(blocks, agent.ContentBlock{
				Type:  "tool_use",
				ID:    data.ID,
				Name:  data.Name,
				Input: data.Input,
			})
		case "tool_result":
			toolUseID := data.ToolCallID
			if toolUseID == "" {
				toolUseID = data.ID
			}
			content := data.Output
			if len(content) == 0 {
				content = data.Result
			}
			blocks = append(blocks, agent.ContentBlock{
				Type:      "tool_result",
				ToolUseID: toolUseID,
				Content:   content,
			})
		}
	}
	return blocks
}
