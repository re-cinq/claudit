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
// It first tries flat file storage (pre-v1.2 OpenCode), then falls back to
// SQLite, which OpenCode has used since v1.2. Newer OpenCode releases (v1.15+)
// keep a project-local database at <repoRoot>/.opencode/opencode.db instead of
// (or in addition to) the global XDG data directory, so that location is tried
// first before falling back to the historical global path.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (pre-v1.2 OpenCode)
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	projectID := GetProjectID(projectPath)

	// Newer OpenCode versions keep a project-local SQLite database.
	localDBPath := filepath.Join(projectPath, ".opencode", "opencode.db")
	if session, err := discoverFromSQLite(localDBPath, projectID, projectPath); err == nil && session != nil {
		return session, nil
	}

	// Fall back to the global XDG data directory (older OpenCode v1.2-v1.14).
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	globalDBPath := filepath.Join(dataDir, "opencode.db")
	return discoverFromSQLite(globalDBPath, projectID, projectPath)
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

// discoverFromSQLite queries an OpenCode SQLite database for the most recent
// session. The exact table/column names have changed across OpenCode
// releases (e.g. "session"/"message" singular tables with a "data" JSON blob
// in older versions, vs. "sessions"/"messages" plural tables with a "parts"
// JSON array of typed entries in newer ones), so the schema is discovered at
// runtime via sqlite_master/PRAGMA table_info rather than hardcoded.
func discoverFromSQLite(dbPath, projectID, projectPath string) (*agent.SessionInfo, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, sessionCols := sqliteFindTable(dbPath, []string{"session", "sessions"})
	if sessionTable == "" {
		return nil, nil
	}
	idCol := pickColumn(sessionCols, []string{"id"})
	if idCol == "" {
		return nil, nil
	}
	projectCol := pickColumn(sessionCols, []string{"project_id", "projectID", "project", "directory", "cwd"})
	timeCol := pickColumn(sessionCols, []string{"time_updated", "updated_at", "updatedAt", "time_created", "created_at", "createdAt"})

	orderBy := "rowid DESC"
	if timeCol != "" {
		orderBy = fmt.Sprintf("%s DESC", timeCol)
	}

	sessionID := ""
	if projectCol != "" {
		sessionQuery := fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s LIMIT 1;`,
			idCol, sessionTable, projectCol, projectID, orderBy,
		)
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
		if out, err := cmd.Output(); err == nil {
			sessionID = strings.TrimSpace(string(out))
		}
	}

	// If there's no project-scoping column, or the scoped lookup found
	// nothing (e.g. the ID scheme changed), fall back to the most recent
	// session in the database. This is safe for a project-local database
	// (which is inherently scoped to one project) and is still a reasonable
	// best-effort for a global database.
	if sessionID == "" {
		sessionQuery := fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s LIMIT 1;`, idCol, sessionTable, orderBy)
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return nil, nil
		}
		sessionID = strings.TrimSpace(string(out))
	}

	// Check if this session was recent (within timeout), using whichever
	// timestamp column exists. If we can't determine recency, proceed anyway
	// rather than skip a potentially valid session.
	if timeCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, timeCol, sessionTable, idCol, sessionID)
		cmd := exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			if !isRecentOpenCodeTimestamp(strings.TrimSpace(string(timeOutput))) {
				return nil, nil
			}
		}
	}

	messageTable, messageCols := sqliteFindTable(dbPath, []string{"message", "messages"})
	if messageTable == "" {
		return nil, nil
	}
	sessionFKCol := pickColumn(messageCols, []string{"session_id", "sessionID", "sessionId"})
	if sessionFKCol == "" {
		return nil, nil
	}
	msgIDCol := pickColumn(messageCols, []string{"id"})
	msgTimeCol := pickColumn(messageCols, []string{"time_created", "created_at", "createdAt", "time_updated", "updated_at"})
	dataCol := pickColumn(messageCols, []string{"data"})
	partsCol := pickColumn(messageCols, []string{"parts"})
	roleCol := pickColumn(messageCols, []string{"role"})

	msgOrderBy := "rowid"
	if msgTimeCol != "" {
		msgOrderBy = msgTimeCol
	}

	var msgQuery string
	switch {
	case dataCol != "" && msgIDCol != "":
		// Older schema: a single "data" column holds the full message JSON.
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s' ORDER BY %s;`,
			dataCol, msgIDCol, messageTable, sessionFKCol, sessionID, msgOrderBy,
		)
	case partsCol != "":
		// Newer schema: messages are split into id/role/parts/time columns.
		// Reconstruct a {id, role, parts, time} object per message so
		// parseOpenCodeMessage can turn the typed parts into content blocks.
		idExpr := "NULL"
		if msgIDCol != "" {
			idExpr = msgIDCol
		}
		roleExpr := "''"
		if roleCol != "" {
			roleExpr = roleCol
		}
		timeExpr := "NULL"
		if msgTimeCol != "" {
			timeExpr = msgTimeCol
		}
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(json_object('id', %s, 'role', %s, 'parts', json(%s), 'time', json_object('created', %s))) FROM %s WHERE %s='%s' ORDER BY %s;`,
			idExpr, roleExpr, partsCol, timeExpr, messageTable, sessionFKCol, sessionID, msgOrderBy,
		)
	default:
		return nil, nil
	}

	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if len(transcriptData) == 0 || string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
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

// sqliteFindTable returns the name and column names of the first candidate
// table that exists in the database, or ("", nil) if none of them do.
func sqliteFindTable(dbPath string, candidates []string) (string, []string) {
	for _, name := range candidates {
		cols, err := sqliteTableColumns(dbPath, name)
		if err == nil && len(cols) > 0 {
			return name, cols
		}
	}
	return "", nil
}

// sqliteTableColumns returns the column names of a table via PRAGMA
// table_info. Returns an empty slice (no error) if the table doesn't exist.
func sqliteTableColumns(dbPath, table string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// PRAGMA table_info output columns: cid|name|type|notnull|dflt_value|pk
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// pickColumn returns the first candidate that's present in cols, or "".
func pickColumn(cols []string, candidates []string) string {
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

// isRecentOpenCodeTimestamp reports whether a timestamp string (ISO-8601 text
// or a Unix epoch integer in seconds or milliseconds) is within
// agent.RecentSessionTimeout. If the format can't be recognized, it returns
// true so discovery isn't blocked by an unparseable timestamp.
func isRecentOpenCodeTimestamp(timeStr string) bool {
	if timeStr == "" {
		return true
	}

	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		var t time.Time
		switch {
		case n > 1e15: // microseconds
			t = time.UnixMicro(n)
		case n > 1e12: // milliseconds
			t = time.UnixMilli(n)
		default: // seconds
			t = time.Unix(n, 0)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	return true
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

	// Try "parts" array (newer OpenCode message format: typed entries like
	// {"type": "text", "data": {"text": "..."}}).
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

// parseOpenCodeParts converts OpenCode's typed "parts" array into
// ContentBlocks. Each part has a "type" (text, reasoning, tool_call,
// tool_result, finish, ...) and a "data" object holding the type-specific
// fields.
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
		if p.Type == "finish" {
			continue
		}

		var data struct {
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if len(p.Data) > 0 {
			_ = json.Unmarshal(p.Data, &data)
		}

		block := agent.ContentBlock{}
		switch p.Type {
		case "text":
			block.Type = "text"
			block.Text = data.Text
		case "reasoning":
			block.Type = "thinking"
			block.Thinking = data.Text
		case "tool_call":
			block.Type = "tool_use"
			block.ID = data.ID
			block.Name = data.Name
			block.Input = openCodeToolInput(data.Input)
		case "tool_result":
			block.Type = "tool_result"
			toolUseID := data.ToolUseID
			if toolUseID == "" {
				toolUseID = data.ID
			}
			block.ToolUseID = toolUseID
			block.Content = data.Content
		default:
			if data.Text == "" {
				continue
			}
			block.Type = "text"
			block.Text = data.Text
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// openCodeToolInput normalizes a tool_call part's "input" field, which may
// be a JSON object or a JSON-encoded string containing an object.
func openCodeToolInput(input json.RawMessage) json.RawMessage {
	if len(input) == 0 {
		return nil
	}

	trimmed := strings.TrimSpace(string(input))
	if strings.HasPrefix(trimmed, "\"") {
		var inner string
		if err := json.Unmarshal(input, &inner); err == nil {
			return json.RawMessage(inner)
		}
	}
	return input
}
