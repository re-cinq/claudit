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

// openCodeSQLSchema describes the table/column names used by OpenCode's
// SQLite database. OpenCode ships releases very frequently and has renamed
// these before (e.g. introducing the SQLite backend itself); rather than
// hardcode names that can silently drift out from under us, we introspect
// the database and fall back to the last-known-good names only if
// introspection is inconclusive or finds no matching rows.
type openCodeSQLSchema struct {
	SessionTable string
	SessionID    string
	ProjectCol   string
	UpdatedCol   string

	MessageTable   string
	MessageID      string
	SessionFKCol   string
	MessageTimeCol string
	ContentCol     string
}

// defaultOpenCodeSQLSchema is the last-known-good schema, used when live
// introspection of the database fails, is inconclusive, or turns up nothing.
func defaultOpenCodeSQLSchema() *openCodeSQLSchema {
	return &openCodeSQLSchema{
		SessionTable:   "session",
		SessionID:      "id",
		ProjectCol:     "project_id",
		UpdatedCol:     "time_updated",
		MessageTable:   "message",
		MessageID:      "id",
		SessionFKCol:   "session_id",
		MessageTimeCol: "time_created",
		ContentCol:     "data",
	}
}

// detectOpenCodeSQLSchema introspects the OpenCode SQLite database to find
// the current session/message table and column names. Returns nil if the
// database doesn't look like a recognizable OpenCode schema.
func detectOpenCodeSQLSchema(dbPath string) *openCodeSQLSchema {
	tables := sqliteQueryRows(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	sessionTable := findNameLike(tables, "session")
	messageTable := findNameLike(tables, "message")
	if sessionTable == "" || messageTable == "" {
		return nil
	}

	sessionCols := sqlitePragmaColumns(dbPath, sessionTable)
	messageCols := sqlitePragmaColumns(dbPath, messageTable)

	if !hasColumn(sessionCols, "id") || !hasColumn(messageCols, "id") {
		return nil
	}

	projectCol := findNameLike(sessionCols, "project")
	updatedCol := findNameLike(sessionCols, "updated")
	if updatedCol == "" {
		updatedCol = findNameLike(sessionCols, "created")
	}
	if projectCol == "" || updatedCol == "" {
		return nil
	}

	sessionFKCol := findNameLike(messageCols, "session")
	if sessionFKCol == "" {
		return nil
	}

	messageTimeCol := findNameLike(messageCols, "created")
	if messageTimeCol == "" {
		messageTimeCol = findNameLike(messageCols, "updated")
	}
	if messageTimeCol == "" {
		messageTimeCol = "id"
	}

	contentCol := ""
	for _, candidate := range []string{"data", "parts", "content", "body"} {
		if hasColumn(messageCols, candidate) {
			contentCol = candidate
			break
		}
	}
	if contentCol == "" {
		return nil
	}

	return &openCodeSQLSchema{
		SessionTable:   sessionTable,
		SessionID:      "id",
		ProjectCol:     projectCol,
		UpdatedCol:     updatedCol,
		MessageTable:   messageTable,
		MessageID:      "id",
		SessionFKCol:   sessionFKCol,
		MessageTimeCol: messageTimeCol,
		ContentCol:     contentCol,
	}
}

// sqliteQueryRows runs a read-only sqlite3 query and returns each output line.
// Returns nil on any error (missing binary, malformed DB, etc.) so callers can
// fall back gracefully.
func sqliteQueryRows(dbPath, query string) []string {
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var rows []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			rows = append(rows, line)
		}
	}
	return rows
}

// sqlitePragmaColumns returns the column names of the given table via
// PRAGMA table_info, which sqlite3 emits as pipe-separated rows:
// cid|name|type|notnull|dflt_value|pk
func sqlitePragmaColumns(dbPath, table string) []string {
	rows := sqliteQueryRows(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	var cols []string
	for _, row := range rows {
		fields := strings.Split(row, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// findNameLike returns the first name matching substr case-insensitively,
// preferring an exact match over a substring match.
func findNameLike(names []string, substr string) string {
	for _, n := range names {
		if strings.EqualFold(n, substr) {
			return n
		}
	}
	for _, n := range names {
		if strings.Contains(strings.ToLower(n), substr) {
			return n
		}
	}
	return ""
}

func hasColumn(cols []string, name string) bool {
	for _, c := range cols {
		if strings.EqualFold(c, name) {
			return true
		}
	}
	return false
}

// parseOpenCodeSQLTime attempts to parse a session timestamp using the
// several formats OpenCode has used, including raw Unix epoch integers
// (seconds or milliseconds).
func parseOpenCodeSQLTime(timeStr string) (time.Time, bool) {
	timeStr = strings.TrimSpace(timeStr)
	if timeStr == "" {
		return time.Time{}, false
	}

	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, timeStr); err == nil {
			return t, true
		}
	}

	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		// Disambiguate seconds vs. millisecond vs. microsecond epochs by magnitude.
		switch {
		case n > 1e17:
			return time.UnixMicro(n), true
		case n > 1e14:
			return time.UnixMilli(n), true
		default:
			return time.Unix(n, 0), true
		}
	}

	return time.Time{}, false
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session, trying an introspected schema first (to tolerate table/
// column renames across OpenCode releases) and falling back to the
// last-known-good hardcoded schema if introspection is inconclusive or
// yields no matching session.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	if schema := detectOpenCodeSQLSchema(dbPath); schema != nil {
		info, err := discoverFromSQLiteWithSchema(dbPath, projectID, projectPath, schema)
		if err != nil {
			return nil, err
		}
		if info != nil {
			return info, nil
		}
		// Introspected tables found no matching session; they may be
		// unrelated to session storage. Fall through to the
		// last-known-good schema below.
	}

	return discoverFromSQLiteWithSchema(dbPath, projectID, projectPath, defaultOpenCodeSQLSchema())
}

// discoverFromSQLiteWithSchema finds the most recent session for projectID
// using the given schema, checks recency, and loads its messages.
func discoverFromSQLiteWithSchema(dbPath, projectID, projectPath string, schema *openCodeSQLSchema) (*agent.SessionInfo, error) {
	sessionQuery := fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
		schema.SessionID, schema.SessionTable, schema.ProjectCol, projectID, schema.UpdatedCol,
	)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Check if this session was recent (within timeout)
	timeQuery := fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s='%s';`,
		schema.UpdatedCol, schema.SessionTable, schema.SessionID, sessionID,
	)
	cmd = exec.Command("sqlite3", dbPath, timeQuery)
	timeOutput, err := cmd.Output()
	if err == nil {
		if t, ok := parseOpenCodeSQLTime(string(timeOutput)); ok {
			if time.Since(t) > agent.RecentSessionTimeout {
				return nil, nil
			}
		}
		// If we can't parse the time, proceed anyway — better to try than skip
	}

	// Get messages for this session as a JSON array
	var contentExpr string
	if schema.ContentCol == "data" {
		contentExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", schema.ContentCol, schema.MessageID)
	} else {
		contentExpr = fmt.Sprintf(
			"json_set(json_object('id', %s), '$.%s', json(%s))",
			schema.MessageID, schema.ContentCol, schema.ContentCol,
		)
	}

	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM %s WHERE %s='%s' ORDER BY %s;`,
		contentExpr, schema.MessageTable, schema.SessionFKCol, sessionID, schema.MessageTimeCol,
	)
	cmd = exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
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
