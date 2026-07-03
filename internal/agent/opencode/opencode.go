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
// Table and column names are discovered at runtime via SQLite's schema
// introspection (PRAGMA table_info) rather than hardcoded, since OpenCode has
// changed its internal schema across releases (e.g. table renames, or a
// single inline message blob being split into a separate parts table).
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, sessionCols := firstExistingTable(dbPath, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}

	projectCol, projectVal := pickProjectFilter(sessionCols, projectID, projectPath)
	orderCol := pickColumn(sessionCols,
		"time_updated", "updated", "updatedat", "time_created", "created", "createdat", "mtime", "timestamp")

	sessionID := querySessionID(dbPath, sessionTable, idCol, projectCol, projectVal, orderCol)
	if sessionID == "" && projectCol != "" {
		// The project column's value format may not match what we computed;
		// fall back to the most recent session overall rather than nothing.
		sessionID = querySessionID(dbPath, sessionTable, idCol, "", "", orderCol)
	}
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout)
	if orderCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, orderCol, sessionTable, idCol, sqliteEscape(sessionID))
		cmd := exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			// If we can't parse the time, proceed anyway — better to try than skip.
			if !isWithinRecentTimeout(strings.TrimSpace(string(timeOutput))) {
				return nil, nil
			}
		}
	}

	transcriptData, err := loadSQLiteTranscript(dbPath, sessionID)
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

// querySessionID returns the most recent session ID, optionally filtered by
// project. Returns "" if none found or the query fails.
func querySessionID(dbPath, table, idCol, projectCol, projectVal, orderCol string) string {
	query := fmt.Sprintf("SELECT %s FROM %s", idCol, table)
	if projectCol != "" {
		query += fmt.Sprintf(" WHERE %s='%s'", projectCol, sqliteEscape(projectVal))
	}
	if orderCol != "" {
		query += " ORDER BY " + orderCol + " DESC"
	}
	query += " LIMIT 1;"

	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// loadSQLiteTranscript reads a session's messages as a JSON array compatible
// with ParseTranscript. It first tries a single inline JSON blob column on
// the message table (older OpenCode releases), then falls back to
// reconstructing entries from individual columns plus a parts table (newer
// releases that split message content into separate rows).
func loadSQLiteTranscript(dbPath, sessionID string) ([]byte, error) {
	messageTable, messageCols := firstExistingTable(dbPath, "message", "messages")
	if messageTable == "" {
		return nil, nil
	}

	idCol := pickColumn(messageCols, "id")
	sessionRefCol := pickColumn(messageCols, "session_id", "sessionid", "session")
	orderCol := pickColumn(messageCols, "time_created", "created", "createdat", "ctime", "time", "seq")
	dataCol := pickColumn(messageCols, "data", "content", "body")

	if idCol == "" || sessionRefCol == "" {
		return nil, nil
	}

	if dataCol != "" {
		query := fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s'`,
			dataCol, idCol, messageTable, sessionRefCol, sqliteEscape(sessionID),
		)
		if orderCol != "" {
			query += " ORDER BY " + orderCol
		}
		query += ";"

		cmd := exec.Command("sqlite3", dbPath, query)
		if output, err := cmd.Output(); err == nil {
			data := strings.TrimSpace(string(output))
			// sqlite3 returns "[null]" when no rows match
			if data != "" && data != "[null]" && data != "[]" {
				return []byte(data), nil
			}
		}
	}

	return reconstructMessagesFromColumns(dbPath, messageTable, messageCols, idCol, sessionRefCol, orderCol, sessionID)
}

// reconstructMessagesFromColumns builds transcript entries directly from the
// message table's own columns, enriching them with text pulled from a parts
// table when message content isn't stored as a single inline blob.
func reconstructMessagesFromColumns(dbPath, messageTable string, messageCols []string, idCol, sessionRefCol, orderCol, sessionID string) ([]byte, error) {
	roleCol := pickColumn(messageCols, "role", "type")
	timeCol := pickColumn(messageCols, "time_created", "created", "createdat", "ctime", "time")

	selectCols := []string{idCol}
	if roleCol != "" {
		selectCols = append(selectCols, roleCol)
	}
	if timeCol != "" && timeCol != roleCol {
		selectCols = append(selectCols, timeCol)
	}

	query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s'`,
		strings.Join(selectCols, ", "), messageTable, sessionRefCol, sqliteEscape(sessionID))
	if orderCol != "" {
		query += " ORDER BY " + orderCol
	}
	query += ";"

	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	partsByMessage := loadSQLiteParts(dbPath)

	var entries []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		entry := map[string]interface{}{"id": fields[0]}

		idx := 1
		if roleCol != "" && idx < len(fields) {
			entry["role"] = fields[idx]
			idx++
		}
		if timeCol != "" && timeCol != roleCol && idx < len(fields) {
			entry["time"] = map[string]interface{}{"created": fields[idx]}
		}
		if content, ok := partsByMessage[fields[0]]; ok {
			entry["content"] = content
		}
		entries = append(entries, entry)
	}

	if len(entries) == 0 {
		return nil, nil
	}

	return json.Marshal(entries)
}

// loadSQLiteParts returns text content keyed by message ID, read from a
// parts table for OpenCode releases that split message content into separate
// rows instead of a single inline blob.
func loadSQLiteParts(dbPath string) map[string][]agent.ContentBlock {
	result := map[string][]agent.ContentBlock{}

	partTable, partCols := firstExistingTable(dbPath, "part", "parts")
	if partTable == "" {
		return result
	}

	msgRefCol := pickColumn(partCols, "message_id", "messageid", "message")
	textCol := pickColumn(partCols, "text", "content", "data")
	typeCol := pickColumn(partCols, "type")
	if msgRefCol == "" || textCol == "" {
		return result
	}

	selectCols := []string{msgRefCol, textCol}
	if typeCol != "" {
		selectCols = append(selectCols, typeCol)
	}

	query := fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s;`, strings.Join(selectCols, ", "), partTable, msgRefCol)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return result
	}

	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		if typeCol != "" && len(fields) > 2 && fields[2] != "text" {
			continue
		}
		msgID := fields[0]
		result[msgID] = append(result[msgID], agent.ContentBlock{Type: "text", Text: fields[1]})
	}

	return result
}

// firstExistingTable returns the name and columns of the first table (from
// candidates, tried in order) that actually exists in the database.
func firstExistingTable(dbPath string, candidates ...string) (string, []string) {
	for _, t := range candidates {
		if cols, err := sqliteColumns(dbPath, t); err == nil && len(cols) > 0 {
			return t, cols
		}
	}
	return "", nil
}

// sqliteColumns returns a table's column names via PRAGMA table_info.
func sqliteColumns(dbPath, table string) ([]string, error) {
	cmd := exec.Command("sqlite3", "-separator", "|", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil, err
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
	return cols, nil
}

// pickColumn returns the first candidate that matches (case-insensitively)
// one of the actual columns, or "" if none match.
func pickColumn(cols []string, candidates ...string) string {
	actual := make(map[string]bool, len(cols))
	for _, c := range cols {
		actual[strings.ToLower(c)] = true
	}
	for _, cand := range candidates {
		if actual[strings.ToLower(cand)] {
			return cand
		}
	}
	return ""
}

// pickProjectFilter finds the column that identifies a session's project and
// the value to filter by: an ID-based column is compared against our
// computed project ID, while a path-based column is compared against the raw
// project path.
func pickProjectFilter(cols []string, projectID, projectPath string) (col, val string) {
	if c := pickColumn(cols, "project_id", "projectid", "project"); c != "" {
		return c, projectID
	}
	if c := pickColumn(cols, "directory", "worktree", "cwd", "path", "root"); c != "" {
		return c, projectPath
	}
	return "", ""
}

// sqliteEscape escapes single quotes for use in a SQL string literal.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// isWithinRecentTimeout reports whether a timestamp (RFC3339-ish, or a Unix
// epoch in seconds/milliseconds) falls within agent.RecentSessionTimeout. An
// unparseable or empty timestamp is treated as recent — better to try using a
// session than to skip it over a format we don't recognize.
func isWithinRecentTimeout(timeStr string) bool {
	if timeStr == "" {
		return true
	}

	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		t := time.UnixMilli(n)
		if n < 1e12 {
			// Looks like seconds rather than milliseconds.
			t = time.Unix(n, 0)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
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
