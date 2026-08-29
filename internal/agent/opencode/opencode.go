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
		// Some OpenCode releases nest per-project storage under an extra
		// "project/<projectID>" segment instead of directly under "storage/".
		altDir, altErr := alternateSessionDir(projectPath)
		if altErr != nil {
			return nil, nil
		}
		dirEntries, err = os.ReadDir(altDir)
		if err != nil {
			return nil, nil
		}
		sessionDir = altDir
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
	if info, err := os.Stat(msgDir); err != nil || !info.IsDir() {
		if altMsgDir, altErr := alternateMessageDir(projectPath, bestSessionID); altErr == nil {
			if info, err := os.Stat(altMsgDir); err == nil && info.IsDir() {
				msgDir = altMsgDir
			}
		}
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// alternateSessionDir returns the project-scoped session storage path used by
// some OpenCode releases: <dataDir>/project/<projectID>/storage/session.
func alternateSessionDir(projectPath string) (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}
	projectID := GetProjectID(projectPath)
	return filepath.Join(dataDir, "project", projectID, "storage", "session"), nil
}

// alternateMessageDir returns the project-scoped message storage path used by
// some OpenCode releases: <dataDir>/project/<projectID>/storage/message/<sessionID>.
func alternateMessageDir(projectPath, sessionID string) (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}
	projectID := GetProjectID(projectPath)
	return filepath.Join(dataDir, "project", projectID, "storage", "message", sessionID), nil
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
//
// OpenCode's SQLite schema (table names, column names, and even the database's
// location relative to the data directory) has shifted across releases. Rather
// than assume a fixed shape, this introspects the actual schema via
// sqlite_master/PRAGMA table_info and falls back to the historically-known
// names only if introspection fails, so discovery keeps working across schema
// changes instead of silently finding nothing.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		// Some releases scope the database per-project instead of one shared
		// database for the whole data directory.
		altPath := filepath.Join(dataDir, "project", projectID, "opencode.db")
		if _, err := os.Stat(altPath); err != nil {
			return nil, nil
		}
		dbPath = altPath
	}

	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := findSQLiteTable(dbPath, "sess")
	if sessionTable == "" {
		sessionTable = "session"
	}
	messageTable := findSQLiteTable(dbPath, "mess")
	if messageTable == "" {
		messageTable = "message"
	}

	sessionCols := sqliteTableColumns(dbPath, sessionTable)
	sessionIDCol := pickColumn(sessionCols, "id")
	if sessionIDCol == "" {
		sessionIDCol = "id"
	}
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project")
	updatedCol := pickColumn(sessionCols, "time_updated", "updated_at", "updatedat", "timeupdated")

	orderBy := "rowid DESC"
	if updatedCol != "" {
		orderBy = fmt.Sprintf("%s DESC", updatedCol)
	}

	var sessionQuery string
	if projectCol != "" {
		sessionQuery = fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s LIMIT 1;`,
			sessionIDCol, sessionTable, projectCol, projectID, orderBy,
		)
	} else {
		// Schema has no discoverable project column — fall back to the most
		// recent session overall rather than giving up entirely.
		sessionQuery = fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s LIMIT 1;`, sessionIDCol, sessionTable, orderBy)
	}

	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Check if this session was recent (within timeout)
	if updatedCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, updatedCol, sessionTable, sessionIDCol, sessionID)
		cmd = exec.Command("sqlite3", dbPath, timeQuery)
		timeOutput, err := cmd.Output()
		if err == nil {
			timeStr := strings.TrimSpace(string(timeOutput))
			if !isRecentTimestamp(timeStr) {
				return nil, nil
			}
		}
	}

	messageCols := sqliteTableColumns(dbPath, messageTable)
	sessionLinkCol := pickColumn(messageCols, "session_id", "sessionid")
	if sessionLinkCol == "" {
		sessionLinkCol = "session_id"
	}
	createdCol := pickColumn(messageCols, "time_created", "created_at", "timecreated")
	msgOrderBy := "rowid"
	if createdCol != "" {
		msgOrderBy = createdCol
	}

	// Serialize every column of each message row generically instead of
	// assuming a specific "data"/"parts"/"content" column holds the message
	// body — the transcript parser tries several known shapes when reading
	// the resulting objects back.
	rowExpr := "json_patch(data, json_object('id', id))"
	if len(messageCols) > 0 {
		rowExpr = buildRowJSONExpr(messageCols)
	}

	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM %s WHERE %s='%s' ORDER BY %s;`,
		rowExpr, messageTable, sessionLinkCol, sessionID, msgOrderBy,
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

// findSQLiteTable returns the first table name in the database whose name
// contains the given substring (case-insensitive), or "" if none match or
// the lookup fails.
func findSQLiteTable(dbPath, nameSubstr string) string {
	query := fmt.Sprintf(
		`SELECT name FROM sqlite_master WHERE type='table' AND lower(name) LIKE '%%%s%%' LIMIT 1;`,
		strings.ToLower(nameSubstr),
	)
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// sqliteTableColumns returns the column names of a table, or nil if the
// lookup fails.
func sqliteTableColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 && fields[1] != "" {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// pickColumn returns the first column matching one of the preferred names,
// trying exact (case-insensitive) matches before substring matches. Returns
// "" if nothing matches.
func pickColumn(columns []string, preferred ...string) string {
	for _, p := range preferred {
		for _, c := range columns {
			if strings.EqualFold(c, p) {
				return c
			}
		}
	}
	for _, p := range preferred {
		lp := strings.ToLower(p)
		for _, c := range columns {
			if strings.Contains(strings.ToLower(c), lp) {
				return c
			}
		}
	}
	return ""
}

// buildRowJSONExpr builds a SQL expression that serializes a row's columns
// into a single JSON object, e.g. json_object('id', id, 'role', role, ...).
func buildRowJSONExpr(columns []string) string {
	parts := make([]string, 0, len(columns)*2)
	for _, c := range columns {
		parts = append(parts, "'"+c+"'", c)
	}
	return "json_object(" + strings.Join(parts, ", ") + ")"
}

// isRecentTimestamp reports whether timeStr (RFC3339-ish or a Unix epoch in
// seconds/milliseconds) is within agent.RecentSessionTimeout of now. Unknown
// or unparseable formats are treated as recent — better to try storing a
// conversation than to silently skip one.
func isRecentTimestamp(timeStr string) bool {
	if timeStr == "" {
		return true
	}

	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		t := time.Unix(n, 0)
		if n > 1e12 {
			t = time.UnixMilli(n)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, timeStr); err == nil {
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

	// Try "parts" as an array of typed segments — newer OpenCode releases
	// represent message bodies as [{"type":"text","data":{"text":"..."}}, ...]
	// instead of a flat "content" field.
	if partsRaw, ok := raw["parts"]; ok {
		if blocks := parseOpenCodeParts(partsRaw); len(blocks) > 0 {
			msg.Content = blocks
			return msg
		}
	}

	return msg
}

// parseOpenCodeParts parses OpenCode's typed "parts" message format into
// content blocks.
func parseOpenCodeParts(raw json.RawMessage) []agent.ContentBlock {
	var parts []struct {
		Type string          `json:"type"`
		Text string          `json:"text"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}

	var blocks []agent.ContentBlock
	for _, p := range parts {
		switch p.Type {
		case "text", "reasoning":
			text := p.Text
			if text == "" && len(p.Data) > 0 {
				var d struct {
					Text string `json:"text"`
				}
				if json.Unmarshal(p.Data, &d) == nil {
					text = d.Text
				}
			}
			if text != "" {
				blocks = append(blocks, agent.ContentBlock{Type: "text", Text: text})
			}
		case "tool_call", "tool_use":
			var d struct {
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			_ = json.Unmarshal(p.Data, &d)
			blocks = append(blocks, agent.ContentBlock{Type: "tool_use", ToolUseID: d.ID, Name: d.Name, Input: d.Input})
		}
	}
	return blocks
}
```
