package opencode

import (
	"context"
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

// sqliteQueryTimeout bounds how long we wait on OpenCode's SQLite database.
// A live OpenCode session may hold the database open, so queries must not
// block the calling process (e.g. a git post-commit hook) indefinitely.
const sqliteQueryTimeout = 5 * time.Second

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
// OpenCode's internal SQLite schema has changed across releases (column names
// such as project_id/time_updated/session_id are not guaranteed to be stable),
// so column names are introspected at runtime via PRAGMA table_info instead of
// being hardcoded. If a project-scoped lookup can't be resolved, we fall back
// to the most recently created session overall rather than reporting nothing.
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

	idCol := firstMatchingColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := firstMatchingColumn(sessionCols, "project_id", "projectid", "project")
	timeCol := firstMatchingColumn(sessionCols, "time_updated", "updated_at", "timeupdated", "updated")

	var sessionID string
	if projectCol != "" {
		query := fmt.Sprintf(`SELECT %s FROM session WHERE %s='%s'`, idCol, projectCol, projectID)
		if timeCol != "" {
			query += fmt.Sprintf(` ORDER BY %s DESC`, timeCol)
		}
		query += ` LIMIT 1;`
		sessionID, _ = sqliteQuery(dbPath, query)
	}
	if sessionID == "" {
		// Either the project column didn't match anything or the schema drifted
		// enough that we couldn't build a project-scoped query. Fall back to
		// the most recently created session rather than giving up entirely.
		sessionID, _ = sqliteQuery(dbPath, fmt.Sprintf(`SELECT %s FROM session ORDER BY rowid DESC LIMIT 1;`, idCol))
	}
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout). If we can't
	// determine the time, proceed anyway — better to try than skip.
	if timeCol != "" {
		timeStr, err := sqliteQuery(dbPath, fmt.Sprintf(`SELECT %s FROM session WHERE %s='%s';`, timeCol, idCol, sessionID))
		if err == nil && timeStr != "" {
			if recent, ok := isWithinRecentTimeout(timeStr); ok && !recent {
				return nil, nil
			}
		}
	}

	messageCols, err := sqliteTableColumns(dbPath, "message")
	if err != nil || len(messageCols) == 0 {
		return nil, nil
	}
	msgIDCol := firstMatchingColumn(messageCols, "id")
	msgSessionCol := firstMatchingColumn(messageCols, "session_id", "sessionid")
	msgDataCol := firstMatchingColumn(messageCols, "data", "content", "body", "payload")
	if msgSessionCol == "" || msgDataCol == "" {
		return nil, nil
	}

	var msgQuery string
	if msgIDCol != "" {
		msgQuery = fmt.Sprintf(`SELECT %s, %s FROM message WHERE %s='%s' ORDER BY rowid;`, msgIDCol, msgDataCol, msgSessionCol, sessionID)
	} else {
		msgQuery = fmt.Sprintf(`SELECT %s FROM message WHERE %s='%s' ORDER BY rowid;`, msgDataCol, msgSessionCol, sessionID)
	}

	rows, err := sqliteQueryLines(dbPath, msgQuery)
	if err != nil || len(rows) == 0 {
		return nil, nil
	}

	var messages []json.RawMessage
	for _, row := range rows {
		var id, data string
		if msgIDCol != "" {
			parts := strings.SplitN(row, "\t", 2)
			if len(parts) != 2 {
				continue
			}
			id, data = parts[0], parts[1]
		} else {
			data = row
		}

		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			continue
		}
		if id != "" {
			if idJSON, err := json.Marshal(id); err == nil {
				obj["id"] = idJSON
			}
		}
		merged, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		messages = append(messages, merged)
	}

	if len(messages) == 0 {
		return nil, nil
	}

	transcriptData, err := json.Marshal(messages)
	if err != nil {
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

// sqliteTableColumns returns the column names of a SQLite table (via
// PRAGMA table_info), or nil if the table doesn't exist or has no columns.
func sqliteTableColumns(dbPath, table string) ([]string, error) {
	lines, err := sqliteQueryLines(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return nil, err
	}

	var cols []string
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		cols = append(cols, fields[1])
	}
	return cols, nil
}

// firstMatchingColumn returns the first candidate name that exists in cols
// (case-insensitive exact match first), then falls back to a substring match
// on the first candidate. Returns "" if nothing matches.
func firstMatchingColumn(cols []string, candidates ...string) string {
	for _, candidate := range candidates {
		for _, col := range cols {
			if strings.EqualFold(col, candidate) {
				return col
			}
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	needle := strings.ToLower(candidates[0])
	for _, col := range cols {
		if strings.Contains(strings.ToLower(col), needle) {
			return col
		}
	}
	return ""
}

// isWithinRecentTimeout reports whether timeStr (in any of the formats
// OpenCode has used for timestamps, including Unix epoch milliseconds) is
// within agent.RecentSessionTimeout. ok is false if timeStr couldn't be
// parsed at all, in which case the caller should proceed rather than discard
// the session.
func isWithinRecentTimeout(timeStr string) (recent, ok bool) {
	timeStr = strings.TrimSpace(timeStr)

	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout, true
		}
	}

	// OpenCode may store timestamps as Unix epoch milliseconds.
	if ms, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		return time.Since(time.UnixMilli(ms)) <= agent.RecentSessionTimeout, true
	}

	return false, false
}

// sqliteQuery runs a single SQLite query and returns the first line of output.
func sqliteQuery(dbPath, query string) (string, error) {
	lines, err := sqliteQueryLines(dbPath, query)
	if err != nil || len(lines) == 0 {
		return "", err
	}
	return lines[0], nil
}

// sqliteQueryLines runs a query against dbPath with a bounded timeout — a live
// OpenCode process may hold the database open, so this must not be allowed to
// hang the calling process (e.g. a git post-commit hook) — and returns the
// non-empty output lines, tab-separated per row.
func sqliteQueryLines(dbPath, query string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sqliteQueryTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sqlite3", "-separator", "\t", dbPath, ".timeout 2000", query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var lines []string
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
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
