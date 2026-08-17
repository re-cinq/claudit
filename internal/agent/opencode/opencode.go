package opencode

import (
	"bytes"
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

func (a *Agent) Name() agent.Name    { return agent.OpenCode }
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
		"bash":     true,
		"shell":    true,
		"terminal": true,
		"execute":  true,
		"run":      true,
		"command":  true,
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
// OpenCode's on-disk SQLite schema (table names, column names, project
// identification strategy, timestamp encoding) has changed across releases,
// so instead of assuming one fixed shape we introspect the database at
// runtime and adapt to whatever schema is actually present.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findOpenCodeDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, messageTable := findSessionAndMessageTables(dbPath)
	if sessionTable == "" {
		return nil, nil
	}

	sessionCols := tableColumns(dbPath, sessionTable)
	idCol := firstColumn(sessionCols, "id", "session_id", "sessionID", "sessionId")
	if idCol == "" {
		return nil, nil
	}
	projectCol := firstColumn(sessionCols, "project_id", "projectID", "projectId", "project", "directory", "cwd", "path", "worktree")
	updatedCol := firstColumn(sessionCols, "time_updated", "updated_at", "updatedAt", "timeUpdated",
		"time_created", "created_at", "createdAt", "timeCreated")

	sessionID := findRecentSessionID(dbPath, sessionTable, idCol, projectCol, updatedCol, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout). If we can't
	// determine the timestamp at all, proceed anyway — better to try than
	// to silently drop a valid session.
	if updatedCol != "" {
		if t, ok := sessionTimestamp(dbPath, sessionTable, idCol, updatedCol, sessionID); ok {
			if time.Since(t) > agent.RecentSessionTimeout {
				return nil, nil
			}
		}
	}

	transcriptData := messagesAsJSON(dbPath, messageTable, sessionID)
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

// findOpenCodeDB locates OpenCode's SQLite database under the data directory.
// The canonical location is "opencode.db", but we also accept any other
// "*.db" file directly under dataDir in case the filename changes.
func findOpenCodeDB(dataDir string) string {
	primary := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(primary); err == nil {
		return primary
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		return filepath.Join(dataDir, e.Name())
	}
	return ""
}

// findSessionAndMessageTables inspects the SQLite schema and returns the
// actual table names OpenCode is using for sessions and messages, matching
// loosely (singular or plural) rather than assuming one fixed name.
func findSessionAndMessageTables(dbPath string) (sessionTable, messageTable string) {
	output, err := runSQLite(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return "", ""
	}
	for _, name := range strings.Split(strings.TrimSpace(output), "\n") {
		name = strings.TrimSpace(name)
		switch strings.ToLower(name) {
		case "session", "sessions":
			if sessionTable == "" {
				sessionTable = name
			}
		case "message", "messages":
			if messageTable == "" {
				messageTable = name
			}
		}
	}
	return sessionTable, messageTable
}

// tableColumns returns the column names for a SQLite table.
func tableColumns(dbPath, table string) []string {
	output, err := runSQLite(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return nil
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 && fields[1] != "" {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// firstColumn returns the actual column name (preserving its original case)
// for the first candidate present in cols, or "" if none match.
func firstColumn(cols []string, candidates ...string) string {
	byLower := make(map[string]string, len(cols))
	for _, c := range cols {
		byLower[strings.ToLower(c)] = c
	}
	for _, cand := range candidates {
		if actual, ok := byLower[strings.ToLower(cand)]; ok {
			return actual
		}
	}
	return ""
}

// findRecentSessionID returns the most recent session ID for this project.
// It tries matching the project column against both the git-root-commit-hash
// project ID and the raw project path, since different OpenCode versions have
// used different project identification schemes. If no project column was
// found at all, it falls back to the single most recently updated session in
// the database.
func findRecentSessionID(dbPath, sessionTable, idCol, projectCol, updatedCol, projectID, projectPath string) string {
	orderClause := ""
	if updatedCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", updatedCol)
	}

	if projectCol != "" {
		for _, candidate := range []string{projectID, projectPath} {
			if candidate == "" {
				continue
			}
			query := fmt.Sprintf("SELECT %s FROM %s WHERE %s=%s%s LIMIT 1;",
				idCol, sessionTable, projectCol, sqliteQuote(candidate), orderClause)
			if out, err := runSQLite(dbPath, query); err == nil {
				if id := strings.TrimSpace(out); id != "" {
					return id
				}
			}
		}
		return ""
	}

	query := fmt.Sprintf("SELECT %s FROM %s%s LIMIT 1;", idCol, sessionTable, orderClause)
	out, err := runSQLite(dbPath, query)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// sessionTimestamp resolves a session's "updated" column to a time.Time,
// handling both string-encoded timestamps and Unix epoch integers (seconds
// or milliseconds).
func sessionTimestamp(dbPath, sessionTable, idCol, updatedCol, sessionID string) (time.Time, bool) {
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s=%s;", updatedCol, sessionTable, idCol, sqliteQuote(sessionID))
	out, err := runSQLite(dbPath, query)
	if err != nil {
		return time.Time{}, false
	}

	raw := strings.TrimSpace(out)
	if raw == "" {
		return time.Time{}, false
	}

	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n), true
		}
		if n > 1e9 {
			return time.Unix(n, 0), true
		}
	}

	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}

	return time.Time{}, false
}

// messagesAsJSON returns all messages for a session as a JSON array,
// adapting to whichever message schema is present. Newer OpenCode versions
// may not store a single JSON blob per message (historically a "data"
// column); in that case we assemble an equivalent object from whatever
// recognizable columns exist (role, content, parts, text, ...).
func messagesAsJSON(dbPath, messageTable, sessionID string) []byte {
	if messageTable == "" {
		return nil
	}

	cols := tableColumns(dbPath, messageTable)
	idCol := firstColumn(cols, "id", "message_id", "messageID", "messageId")
	sessionCol := firstColumn(cols, "session_id", "sessionID", "sessionId", "session")
	if idCol == "" || sessionCol == "" {
		return nil
	}

	var selectExpr string
	if dataCol := firstColumn(cols, "data"); dataCol != "" {
		selectExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, idCol)
	} else {
		fields := []string{fmt.Sprintf("'id', %s", idCol)}
		for _, cand := range []string{"role", "content", "parts", "text", "type"} {
			if col := firstColumn(cols, cand); col != "" {
				fields = append(fields, fmt.Sprintf("'%s', %s", strings.ToLower(cand), col))
			}
		}
		if timeCol := firstColumn(cols, "time_created", "created_at", "createdAt", "timeCreated"); timeCol != "" {
			fields = append(fields, fmt.Sprintf("'time', json_object('created', %s)", timeCol))
		}
		if len(fields) == 1 {
			// Nothing but an ID — no usable content, not worth returning.
			return nil
		}
		selectExpr = fmt.Sprintf("json_object(%s)", strings.Join(fields, ", "))
	}

	query := fmt.Sprintf("SELECT json_group_array(%s) FROM %s WHERE %s=%s",
		selectExpr, messageTable, sessionCol, sqliteQuote(sessionID))
	if orderCol := firstColumn(cols, "time_created", "created_at", "createdAt", "timeCreated"); orderCol != "" {
		query += fmt.Sprintf(" ORDER BY %s", orderCol)
	}
	query += ";"

	out, err := runSQLite(dbPath, query)
	if err != nil {
		return nil
	}

	data := bytes.TrimSpace([]byte(out))
	if len(data) == 0 || string(data) == "[null]" || string(data) == "[]" {
		return nil
	}
	return data
}

// runSQLite runs a query against dbPath and returns raw stdout.
func runSQLite(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// sqliteQuote escapes and single-quotes a value for inline use in a SQLite
// query (the sqlite3 CLI does not support parameter binding for ad-hoc
// queries run this way).
func sqliteQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
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

	// Try "parts" as an array of typed blocks (newer OpenCode versions store
	// message content this way instead of a flat "content" field/array).
	// Each part may carry its text directly (`{"type":"text","text":"..."}`)
	// or nested under a "data" object (`{"type":"text","data":{"text":"..."}}`).
	if partsRaw, ok := raw["parts"]; ok {
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(partsRaw, &parts); err == nil && len(parts) > 0 {
			var blocks []agent.ContentBlock
			for _, p := range parts {
				block := agent.ContentBlock{Type: "text"}
				if typeRaw, ok := p["type"]; ok {
					var t string
					if err := json.Unmarshal(typeRaw, &t); err == nil && t != "" {
						block.Type = t
					}
				}
				if textRaw, ok := p["text"]; ok {
					var t string
					_ = json.Unmarshal(textRaw, &t)
					block.Text = t
				} else if dataRaw, ok := p["data"]; ok {
					var d struct {
						Text string `json:"text"`
					}
					if err := json.Unmarshal(dataRaw, &d); err == nil {
						block.Text = d.Text
					}
				}
				if block.Text != "" {
					blocks = append(blocks, block)
				}
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
