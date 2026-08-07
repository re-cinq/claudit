```go
package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
// It tries, in order: flat file storage (pre-v1.2), SQLite (v1.2+, with
// schema introspection to tolerate table/column renames across versions),
// and finally a generic recursive scan of the data directory that doesn't
// assume any particular on-disk layout at all. Each layer is a fallback for
// when OpenCode's internal storage format has moved on from what an earlier
// layer assumes.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (pre-v1.2 OpenCode)
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

	// Fall back to SQLite (OpenCode v1.2+)
	if sqliteSession := discoverFromSQLite(dataDir, GetProjectID(projectPath), projectPath); sqliteSession != nil {
		return sqliteSession, nil
	}

	// Last resort: scan the data directory for anything that looks like a
	// session, without assuming a fixed path layout or schema.
	return scanSessionTree(dataDir, projectPath), nil
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

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session. It introspects the schema (table and column names) rather
// than assuming fixed names, since OpenCode has renamed these across
// versions. It tries matching by a project-identifier column (root commit
// hash) first, then by a directory-like column (raw working directory
// path), and finally falls back to the single most recently updated session
// in the database if no project-scoped match is found.
func discoverFromSQLite(dataDir, projectID, projectPath string) *agent.SessionInfo {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil
	}

	tables := sqliteTables(dbPath)
	sessionTable := pickTable(tables, []string{"session", "sessions"})
	if sessionTable == "" {
		return nil
	}

	cols := sqliteColumns(dbPath, sessionTable)
	idCol := pickColumn(cols, []string{"id", "session_id", "sessionid"})
	if idCol == "" {
		return nil
	}
	timeCol := pickColumn(cols, []string{"time_updated", "updated_at", "updatedat", "updated", "time_created", "created_at", "createdat"})

	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", quoteSQLiteIdent(timeCol))
	}

	matchers := []struct {
		names []string
		value string
	}{
		{[]string{"project_id", "projectid", "project"}, projectID},
		{directoryFieldNames, projectPath},
	}

	var sessionID string
	for _, m := range matchers {
		col := pickColumn(cols, m.names)
		if col == "" {
			continue
		}
		query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;`,
			quoteSQLiteIdent(idCol), quoteSQLiteIdent(sessionTable), quoteSQLiteIdent(col),
			escapeSQLiteLiteral(m.value), orderClause)
		if id, err := sqliteQuery(dbPath, query); err == nil && id != "" {
			sessionID = id
			break
		}
	}

	// No project-scoped match: fall back to the single most recently
	// updated session in the database, still gated by the recency check
	// below.
	if sessionID == "" {
		query := fmt.Sprintf(`SELECT %s FROM %s%s LIMIT 1;`, quoteSQLiteIdent(idCol), quoteSQLiteIdent(sessionTable), orderClause)
		id, err := sqliteQuery(dbPath, query)
		if err != nil || id == "" {
			return nil
		}
		sessionID = id
	}

	if timeCol != "" {
		query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`,
			quoteSQLiteIdent(timeCol), quoteSQLiteIdent(sessionTable), quoteSQLiteIdent(idCol), escapeSQLiteLiteral(sessionID))
		if raw, err := sqliteQuery(dbPath, query); err == nil {
			if t, ok := parseFlexibleTime(raw); ok && time.Since(t) > agent.RecentSessionTimeout {
				return nil
			}
		}
		// If we can't parse the time, proceed anyway — better to try than skip.
	}

	transcriptData := fetchSQLiteMessages(dbPath, tables, sessionID)
	if len(transcriptData) == 0 {
		return nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}
}

// fetchSQLiteMessages retrieves all messages for a session as a JSON array,
// introspecting the message table's schema the same way discoverFromSQLite
// does for the session table.
func fetchSQLiteMessages(dbPath string, tables []string, sessionID string) []byte {
	messageTable := pickTable(tables, []string{"message", "messages"})
	if messageTable == "" {
		return nil
	}

	cols := sqliteColumns(dbPath, messageTable)
	sessionCol := pickColumn(cols, []string{"session_id", "sessionid", "session"})
	dataCol := pickColumn(cols, []string{"data", "content", "body", "payload"})
	if sessionCol == "" || dataCol == "" {
		return nil
	}
	idCol := pickColumn(cols, []string{"id", "message_id", "messageid"})
	orderCol := pickColumn(cols, []string{"time_created", "created_at", "createdat", "time_updated", "updated_at"})

	selectExpr := quoteSQLiteIdent(dataCol)
	if idCol != "" && idCol != dataCol {
		selectExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", quoteSQLiteIdent(dataCol), quoteSQLiteIdent(idCol))
	}
	orderClause := ""
	if orderCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s", quoteSQLiteIdent(orderCol))
	}

	query := fmt.Sprintf(`SELECT json_group_array(%s) FROM %s WHERE %s='%s'%s;`,
		selectExpr, quoteSQLiteIdent(messageTable), quoteSQLiteIdent(sessionCol),
		escapeSQLiteLiteral(sessionID), orderClause)

	out, err := sqliteQuery(dbPath, query)
	if err != nil || out == "" || out == "[null]" || out == "[]" {
		return nil
	}
	return []byte(out)
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

	// Try "parts" field (typed part wrappers used by some OpenCode versions)
	if partsRaw, ok := raw["parts"]; ok {
		var parts []struct {
			Type string          `json:"type"`
			Text string          `json:"text"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(partsRaw, &parts); err == nil {
			var blocks []agent.ContentBlock
			for _, p := range parts {
				text := p.Text
				if text == "" && len(p.Data) > 0 {
					var d struct {
						Text string `json:"text"`
					}
					if err := json.Unmarshal(p.Data, &d); err == nil {
						text = d.Text
					}
				}
				if p.Type == "text" && text != "" {
					blocks = append(blocks, agent.ContentBlock{Type: "text", Text: text})
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
```
