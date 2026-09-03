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

// sqlEscape escapes single quotes for safe interpolation into a sqlite3
// CLI query string (we shell out to sqlite3 rather than using a driver).
func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// sqliteQuery runs a single query against dbPath via the sqlite3 CLI and
// returns the trimmed output.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// sqliteTableExists reports whether the given table exists in dbPath.
func sqliteTableExists(dbPath, table string) bool {
	out, err := sqliteQuery(dbPath, fmt.Sprintf(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='%s';", sqlEscape(table)))
	return err == nil && out != ""
}

// pickTable returns the first candidate table name that exists in dbPath.
// OpenCode has renamed/restructured its storage tables across versions, so
// we probe for known naming variants rather than assuming one.
func pickTable(dbPath string, candidates ...string) string {
	for _, t := range candidates {
		if sqliteTableExists(dbPath, t) {
			return t
		}
	}
	return ""
}

// sqliteColumns returns the column names of a table, or nil if it can't be
// determined.
func sqliteColumns(dbPath, table string) []string {
	out, err := sqliteQuery(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil || out == "" {
		return nil
	}
	var cols []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// firstPresent returns the first candidate that appears in cols, or "".
// Candidates should be ordered from most to least likely so that a schema
// matching shiftlog's original assumptions keeps behaving identically.
func firstPresent(cols []string, candidates ...string) string {
	set := make(map[string]bool, len(cols))
	for _, c := range cols {
		set[c] = true
	}
	for _, cand := range candidates {
		if set[cand] {
			return cand
		}
	}
	return ""
}

func sliceContains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

// epochToTime converts an integer timestamp of unknown unit (seconds,
// milliseconds, microseconds, or nanoseconds since epoch) to a time.Time.
func epochToTime(v int64) time.Time {
	switch {
	case v > 1e17:
		return time.Unix(0, v)
	case v > 1e14:
		return time.Unix(0, v*1e3)
	case v > 1e11:
		return time.Unix(0, v*1e6)
	default:
		return time.Unix(v, 0)
	}
}

// isRecentTimestamp reports whether raw (in any of several known OpenCode
// timestamp formats, including raw epoch integers) falls within
// agent.RecentSessionTimeout. If raw can't be parsed, it returns true —
// better to try using the session than to skip it over a formatting change.
func isRecentTimestamp(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return time.Since(t) <= agent.RecentSessionTimeout
	}
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", raw); err == nil {
		return time.Since(t) <= agent.RecentSessionTimeout
	}
	if t, err := time.Parse("2006-01-02 15:04:05", raw); err == nil {
		return time.Since(t) <= agent.RecentSessionTimeout
	}
	if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Since(epochToTime(v)) <= agent.RecentSessionTimeout
	}
	return true
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
// OpenCode has changed its table/column names across releases (e.g. singular
// vs. plural table names, "time_updated" vs. "updated_at"), so this probes
// the schema at runtime instead of assuming one fixed layout. Candidate
// names are tried in the order shiftlog originally assumed first, so a
// database matching that layout behaves exactly as before.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := pickTable(dbPath, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}
	messageTable := pickTable(dbPath, "message", "messages")
	if messageTable == "" {
		return nil, nil
	}

	sessionCols := sqliteColumns(dbPath, sessionTable)
	projectCol := firstPresent(sessionCols,
		"project_id", "projectID", "projectId", "directory", "path", "cwd", "worktree")
	timeCol := firstPresent(sessionCols,
		"time_updated", "updated_at", "updatedAt", "time_created", "created_at", "createdAt")

	orderBy := "rowid DESC"
	if timeCol != "" {
		orderBy = fmt.Sprintf("%s DESC", timeCol)
	}

	pathLikeCols := map[string]bool{"directory": true, "path": true, "cwd": true, "worktree": true}

	sessionID := ""
	if projectCol != "" {
		filterValue := projectID
		if pathLikeCols[projectCol] {
			filterValue = projectPath
		}
		q := fmt.Sprintf(`SELECT id FROM %s WHERE %s='%s' ORDER BY %s LIMIT 1;`,
			sessionTable, projectCol, sqlEscape(filterValue), orderBy)
		if out, err := sqliteQuery(dbPath, q); err == nil {
			sessionID = strings.TrimSpace(strings.Split(out, "\n")[0])
		}
	}

	// Fall back to the most recent session with no project-based filtering,
	// if the project column can't be found or doesn't match anything. This
	// matches shiftlog's manual-commit use case (a single active session)
	// and is strictly better than reporting no session at all.
	if sessionID == "" {
		q := fmt.Sprintf(`SELECT id FROM %s ORDER BY %s LIMIT 1;`, sessionTable, orderBy)
		out, err := sqliteQuery(dbPath, q)
		if err != nil || out == "" {
			return nil, nil
		}
		sessionID = strings.TrimSpace(strings.Split(out, "\n")[0])
	}
	if sessionID == "" {
		return nil, nil
	}

	// Check recency, if a time column is available.
	if timeCol != "" {
		timeStr, err := sqliteQuery(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s WHERE id='%s';`, timeCol, sessionTable, sqlEscape(sessionID)))
		if err == nil && !isRecentTimestamp(timeStr) {
			return nil, nil
		}
	}

	msgCols := sqliteColumns(dbPath, messageTable)
	sessionRefCol := firstPresent(msgCols, "session_id", "sessionID", "sessionId", "session")
	if sessionRefCol == "" {
		return nil, nil
	}
	msgOrderBy := "rowid"
	if orderCol := firstPresent(msgCols, "time_created", "created_at", "createdAt", "time_updated", "updated_at"); orderCol != "" {
		msgOrderBy = orderCol
	}

	var msgQuery string
	switch {
	case sliceContains(msgCols, "data"):
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM %s WHERE %s='%s' ORDER BY %s;`,
			messageTable, sessionRefCol, sqlEscape(sessionID), msgOrderBy)
	case sliceContains(msgCols, "parts"):
		roleCol := firstPresent(msgCols, "role", "type")
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(json_object('id', id, 'role', %s, 'parts', json(parts))) FROM %s WHERE %s='%s' ORDER BY %s;`,
			roleCol, messageTable, sessionRefCol, sqlEscape(sessionID), msgOrderBy)
	default:
		return nil, nil
	}

	msgOutput, err := sqliteQuery(dbPath, msgQuery)
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(msgOutput)
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

// openCodePart represents a single typed entry in a message's "parts" array,
// used by OpenCode releases that store message content as parts rather than
// a flat "content" string (e.g. {"type":"text","data":{"text":"..."}}).
type openCodePart struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// partsToContentBlocks converts an OpenCode "parts" array into ContentBlocks.
func partsToContentBlocks(partsRaw json.RawMessage) []agent.ContentBlock {
	var parts []openCodePart
	if err := json.Unmarshal(partsRaw, &parts); err != nil {
		return nil
	}

	var blocks []agent.ContentBlock
	for _, p := range parts {
		switch p.Type {
		case "text", "reasoning":
			var d struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(p.Data, &d); err == nil && d.Text != "" {
				blocks = append(blocks, agent.ContentBlock{Type: "text", Text: d.Text})
			}
		case "tool_call", "tool-call", "tool_use":
			var d struct {
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(p.Data, &d); err == nil {
				blocks = append(blocks, agent.ContentBlock{
					Type: "tool_use", ToolUseID: d.ID, Name: d.Name, Input: d.Input,
				})
			}
		case "tool_result", "tool-result":
			var d struct {
				Output json.RawMessage `json:"output"`
			}
			if err := json.Unmarshal(p.Data, &d); err == nil {
				blocks = append(blocks, agent.ContentBlock{Type: "tool_result", Content: d.Output})
			}
		}
	}
	return blocks
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

	// Newer OpenCode releases store content as a typed "parts" array instead
	// of a flat "content" field.
	if partsRaw, ok := raw["parts"]; ok {
		if blocks := partsToContentBlocks(partsRaw); len(blocks) > 0 {
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
