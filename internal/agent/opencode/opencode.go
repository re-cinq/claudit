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
// OpenCode's on-disk storage layout and SQLite schema have both drifted
// across releases (flat files nested directly under storage/session/<id>,
// flat files nested under project/<id>/storage/session, and a SQLite
// database with column names that have varied between versions). Rather
// than assuming a single fixed layout/schema, discovery tries each known
// flat-file layout first, then falls back to SQLite with the schema
// introspected at query time so a column/table rename doesn't silently
// break discovery.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// flatFileLayout describes one known on-disk arrangement of OpenCode's
// flat-file session/message storage.
type flatFileLayout struct {
	sessionDir string
	messageDir func(sessionID string) string
}

// discoverFromFlatFiles tries the known flat-file session storage layouts,
// newest first, and returns the most recently modified session found.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}
	projectID := GetProjectID(projectPath)

	layouts := []flatFileLayout{
		{
			// storage/session/<projectID>/<sessionID>.json
			sessionDir: filepath.Join(dataDir, "storage", "session", projectID),
			messageDir: func(sessionID string) string {
				return filepath.Join(dataDir, "storage", "message", sessionID)
			},
		},
		{
			// project/<projectID>/storage/session/<sessionID>.json
			sessionDir: filepath.Join(dataDir, "project", projectID, "storage", "session"),
			messageDir: func(sessionID string) string {
				return filepath.Join(dataDir, "project", projectID, "storage", "message", sessionID)
			},
		},
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout

	for _, layout := range layouts {
		dirEntries, err := os.ReadDir(layout.sessionDir)
		if err != nil {
			continue
		}

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
			continue
		}

		msgDir := layout.messageDir(bestSessionID)
		transcriptPath := msgDir
		var transcriptData []byte
		if _, err := os.Stat(msgDir); err != nil {
			// Message directory for this layout is missing (e.g. sessions and
			// messages are nested differently); still report the session so a
			// note with an empty transcript is stored instead of nothing.
			transcriptPath = ""
			transcriptData = []byte("[]")
		}

		return &agent.SessionInfo{
			SessionID:      bestSessionID,
			TranscriptPath: transcriptPath,
			TranscriptData: transcriptData,
			StartedAt:      bestModTime.Format(time.RFC3339),
			ProjectPath:    projectPath,
		}, nil
	}

	return nil, nil
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session belonging to projectPath. Table and column names are introspected
// via PRAGMA table_info rather than assumed, since OpenCode has renamed both
// across releases. A session is only discarded here if none can be found at
// all — if the session is found but its messages can't be read, an empty
// transcript is returned so a note still gets stored.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, sessionCols := sqliteFindTable(dbPath, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}

	idCol := sqlitePickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}

	filterCol, filterVal := "", ""
	switch {
	case sqliteHasColumn(sessionCols, "project_id"):
		filterCol, filterVal = "project_id", projectID
	case sqliteHasColumn(sessionCols, "projectID"):
		filterCol, filterVal = "projectID", projectID
	case sqliteHasColumn(sessionCols, "directory"):
		filterCol, filterVal = "directory", projectPath
	case sqliteHasColumn(sessionCols, "cwd"):
		filterCol, filterVal = "cwd", projectPath
	case sqliteHasColumn(sessionCols, "worktree"):
		filterCol, filterVal = "worktree", projectPath
	}

	orderCol := sqlitePickColumn(sessionCols,
		"time_updated", "updated_at", "timeUpdated", "updatedAt",
		"time_created", "created_at", "timeCreated", "createdAt")

	sessionID, orderVal := sqliteFindSession(dbPath, sessionTable, idCol, filterCol, filterVal, orderCol)
	if sessionID == "" {
		return nil, nil
	}

	if orderVal != "" {
		if t, ok := parseSQLiteTime(orderVal); ok && time.Since(t) > agent.RecentSessionTimeout {
			return nil, nil
		}
	}

	transcriptData := sqliteFetchMessages(dbPath, sessionID)

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// sqliteFindTable returns the name and column list of the first candidate
// table that exists in the database, or ("", nil) if none do.
func sqliteFindTable(dbPath string, candidates ...string) (string, []string) {
	for _, name := range candidates {
		cols := sqliteTableColumns(dbPath, name)
		if len(cols) > 0 {
			return name, cols
		}
	}
	return "", nil
}

// sqliteTableColumns returns the column names of a table via PRAGMA table_info,
// or nil if the table doesn't exist or sqlite3 fails.
func sqliteTableColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) >= 2 {
			cols = append(cols, parts[1])
		}
	}
	return cols
}

func sqliteHasColumn(cols []string, name string) bool {
	for _, c := range cols {
		if c == name {
			return true
		}
	}
	return false
}

// sqlitePickColumn returns the first candidate present in cols, or "".
func sqlitePickColumn(cols []string, candidates ...string) string {
	for _, cand := range candidates {
		if sqliteHasColumn(cols, cand) {
			return cand
		}
	}
	return ""
}

// sqliteEscape escapes single quotes for use in a single-quoted SQL literal.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// sqliteFindSession returns the id (and, if available, the raw order-column
// value) of the most recent session, optionally filtered by filterCol=filterVal.
func sqliteFindSession(dbPath, table, idCol, filterCol, filterVal, orderCol string) (sessionID, orderVal string) {
	query := fmt.Sprintf("SELECT %s", idCol)
	if orderCol != "" {
		query += fmt.Sprintf(", %s", orderCol)
	}
	query += fmt.Sprintf(" FROM %s", table)
	if filterCol != "" {
		query += fmt.Sprintf(" WHERE %s='%s'", filterCol, sqliteEscape(filterVal))
	}
	if orderCol != "" {
		query += fmt.Sprintf(" ORDER BY %s DESC", orderCol)
	} else {
		query += " ORDER BY rowid DESC"
	}
	query += " LIMIT 1;"

	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", ""
	}

	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", ""
	}

	fields := strings.Split(line, "\t")
	sessionID = fields[0]
	if len(fields) > 1 {
		orderVal = fields[1]
	}
	return sessionID, orderVal
}

// sqliteFetchMessages returns the messages for sessionID as a JSON array.
// It adapts to either a single JSON blob column ("data") or a set of
// discrete typed columns. On any failure it returns an empty array rather
// than nil, so a valid (if empty) transcript is always available and the
// caller can still store a note instead of discarding the session entirely.
func sqliteFetchMessages(dbPath, sessionID string) []byte {
	empty := []byte("[]")

	messageTable, messageCols := sqliteFindTable(dbPath, "message", "messages")
	if messageTable == "" {
		return empty
	}

	sessionCol := sqlitePickColumn(messageCols, "session_id", "sessionID", "session")
	if sessionCol == "" {
		return empty
	}

	orderCol := sqlitePickColumn(messageCols,
		"time_created", "created_at", "timeCreated", "createdAt",
		"time_updated", "updated_at")

	var itemExpr string
	if idCol := sqlitePickColumn(messageCols, "id"); idCol != "" && sqliteHasColumn(messageCols, "data") {
		itemExpr = fmt.Sprintf("json_patch(data, json_object('id', %s))", idCol)
	} else if sqliteHasColumn(messageCols, "data") {
		itemExpr = "data"
	} else {
		var fields []string
		for _, candidate := range []string{"id", "role", "type", "content", "parts", "model", "time", "created_at", "time_created"} {
			if sqliteHasColumn(messageCols, candidate) {
				fields = append(fields, fmt.Sprintf("'%s', %s", candidate, candidate))
			}
		}
		if len(fields) == 0 {
			return empty
		}
		itemExpr = fmt.Sprintf("json_object(%s)", strings.Join(fields, ", "))
	}

	inner := fmt.Sprintf("SELECT %s AS item FROM %s WHERE %s='%s'",
		itemExpr, messageTable, sessionCol, sqliteEscape(sessionID))
	if orderCol != "" {
		inner += fmt.Sprintf(" ORDER BY %s", orderCol)
	}

	query := fmt.Sprintf("SELECT json_group_array(json(item)) FROM (%s);", inner)

	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return empty
	}

	data := bytes.TrimSpace(out)
	if len(data) == 0 {
		return empty
	}
	switch string(data) {
	case "[null]", "null", "":
		return empty
	}
	return data
}

// parseSQLiteTime attempts to parse a session timestamp in any of the
// formats OpenCode has used (ISO 8601 variants or epoch seconds/milliseconds).
func parseSQLiteTime(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, true
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		switch {
		case n > 1e14: // microseconds
			return time.UnixMicro(n), true
		case n > 1e11: // milliseconds
			return time.UnixMilli(n), true
		case n > 0: // seconds
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

	// Try "parts" as an array of typed segments (newer OpenCode message format,
	// e.g. [{"type":"text","text":"..."},{"type":"tool", ...}]).
	if partsRaw, ok := raw["parts"]; ok {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(partsRaw, &parts); err == nil && len(parts) > 0 {
			var blocks []agent.ContentBlock
			for _, p := range parts {
				if p.Text == "" {
					continue
				}
				blocks = append(blocks, agent.ContentBlock{Type: "text", Text: p.Text})
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
