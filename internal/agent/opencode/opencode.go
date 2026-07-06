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
// It first tries flat file storage (pre-v1.2 OpenCode), then falls back to
// SQLite (v1.2+). The SQLite schema is introspected at query time rather than
// hardcoded, since OpenCode has renamed/reshaped these internal tables across
// releases (e.g. between 1.14.x and 1.17.x) and a hardcoded column name would
// silently break discovery on any such rename.
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
// Column names are discovered dynamically via PRAGMA table_info rather than
// hardcoded, because OpenCode's internal session/message schema has shifted
// across releases (column renames such as project identifiers, timestamp
// columns, or the JSON payload column). Hardcoding names meant any such rename
// would cause the query to fail and DiscoverSession to silently report "no
// session found" even though a valid session existed on disk.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionCols, err := sqliteTableColumns(dbPath, "session")
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	sessionIDCol := pickColumn(sessionCols, "id")
	sessionProjectCol := pickColumn(sessionCols, "project_id", "projectid", "project", "directory", "worktree", "cwd")
	sessionTimeCol := pickColumn(sessionCols, "time_updated", "updated", "modified", "time_created", "created")
	if sessionIDCol == "" || sessionProjectCol == "" {
		return nil, nil
	}

	// Find most recent session for this project
	sessionQuery := fmt.Sprintf(`SELECT %s FROM session WHERE %s='%s'`, sessionIDCol, sessionProjectCol, projectID)
	if sessionTimeCol != "" {
		sessionQuery += fmt.Sprintf(` ORDER BY %s DESC`, sessionTimeCol)
	}
	sessionQuery += ` LIMIT 1;`

	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Check if this session was recent (within timeout)
	if sessionTimeCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM session WHERE %s='%s';`, sessionTimeCol, sessionIDCol, sessionID)
		cmd = exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			if t, ok := parseFlexibleTime(strings.TrimSpace(string(timeOutput))); ok {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
			// If we can't parse the time, proceed anyway — better to try than skip
		}
	}

	fallback := &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
	}

	msgCols, err := sqliteTableColumns(dbPath, "message")
	if err != nil || len(msgCols) == 0 {
		return fallback, nil
	}

	msgIDCol := pickColumn(msgCols, "id")
	msgSessionCol := pickColumn(msgCols, "session_id", "sessionid", "session")
	msgTimeCol := pickColumn(msgCols, "time_created", "created", "time_updated", "updated")
	msgDataCol := pickColumn(msgCols, "data", "content", "body", "json", "payload", "message")
	if msgSessionCol == "" || msgDataCol == "" {
		return fallback, nil
	}

	selectCols := msgDataCol
	if msgIDCol != "" {
		selectCols = msgIDCol + ", " + msgDataCol
	}
	msgQuery := fmt.Sprintf(`SELECT %s FROM message WHERE %s='%s'`, selectCols, msgSessionCol, sessionID)
	if msgTimeCol != "" {
		msgQuery += fmt.Sprintf(` ORDER BY %s`, msgTimeCol)
	}
	msgQuery += ";"

	cmd = exec.Command("sqlite3", "-json", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return fallback, nil
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal(msgOutput, &rows); err != nil || len(rows) == 0 {
		return fallback, nil
	}

	var entries []json.RawMessage
	for _, row := range rows {
		var obj map[string]json.RawMessage
		switch v := row[msgDataCol].(type) {
		case string:
			if err := json.Unmarshal([]byte(v), &obj); err != nil {
				continue
			}
		case map[string]interface{}:
			b, err := json.Marshal(v)
			if err != nil {
				continue
			}
			if err := json.Unmarshal(b, &obj); err != nil {
				continue
			}
		default:
			continue
		}
		if obj == nil {
			continue
		}
		if msgIDCol != "" {
			if idStr, ok := row[msgIDCol].(string); ok && idStr != "" {
				if idBytes, err := json.Marshal(idStr); err == nil {
					obj["id"] = idBytes
				}
			}
		}
		if merged, err := json.Marshal(obj); err == nil {
			entries = append(entries, merged)
		}
	}

	if len(entries) == 0 {
		return fallback, nil
	}

	transcriptData, err := json.Marshal(entries)
	if err != nil {
		return fallback, nil
	}

	fallback.TranscriptData = transcriptData
	return fallback, nil
}

// sqliteTableColumns returns the column names of a SQLite table via PRAGMA table_info.
func sqliteTableColumns(dbPath, table string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) >= 2 {
			cols = append(cols, parts[1])
		}
	}
	return cols, nil
}

// pickColumn finds the best matching column name from candidates, preferring
// an exact (case-insensitive) match before falling back to substring matches.
func pickColumn(cols []string, candidates ...string) string {
	lowerCols := make(map[string]string, len(cols))
	for _, c := range cols {
		lowerCols[strings.ToLower(c)] = c
	}

	for _, cand := range candidates {
		if c, ok := lowerCols[strings.ToLower(cand)]; ok {
			return c
		}
	}

	for _, cand := range candidates {
		candLower := strings.ToLower(cand)
		for lower, orig := range lowerCols {
			if strings.Contains(lower, candLower) {
				return orig
			}
		}
	}

	return ""
}

// parseFlexibleTime parses a timestamp that may be an RFC3339-family string
// or a Unix epoch integer (seconds, milliseconds, or nanoseconds).
func parseFlexibleTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		switch {
		case n > 1e17: // nanoseconds
			return time.Unix(0, n), true
		case n > 1e14: // milliseconds
			return time.UnixMilli(n), true
		case n > 0: // seconds
			return time.Unix(n, 0), true
		}
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
