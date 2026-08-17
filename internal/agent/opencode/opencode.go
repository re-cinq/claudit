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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session belonging to this project.
//
// OpenCode's SQLite schema (table/column names, and how a session record is
// associated with a project) has changed across releases — this codebase has
// already had to adapt once (flat files -> SQLite around v1.2). Rather than
// hardcode column names that can silently drift out from under us again
// (a mismatch here fails "closed": zero rows, not an error, so a stale
// assumption looks identical to "no session exists"), the schema is
// introspected via PRAGMA table_info and matched defensively:
//   - the project association is checked against BOTH the git-root-commit-hash
//     projectID and the raw absolute projectPath, against every column (exact
//     match or substring), since we don't know which convention (or column
//     name) the installed version uses.
//   - if no column matches either candidate, we fall back to the most
//     recently updated session in the database, still subject to the
//     recency timeout, rather than giving up.
//   - the message table is handled the same way: if no single JSON-blob
//     column is found, one is synthesized from all available columns.
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

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := pickColumn(sessionCols, "time_updated", "updated", "time_created", "created", "time", "mtime")

	sessionID := findRecentSessionID(dbPath, sessionCols, idCol, timeCol, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	transcriptData := loadSessionMessages(dbPath, sessionID)
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

// sqliteTableColumns returns the column names of a SQLite table via
// PRAGMA table_info. table must be a fixed, trusted string (not user input).
func sqliteTableColumns(dbPath, table string) ([]string, error) {
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) >= 2 && fields[1] != "" {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// pickColumn returns the first column (case-insensitively) matching one of
// candidates in priority order, first by exact name then by substring.
func pickColumn(cols []string, candidates ...string) string {
	lower := make(map[string]string, len(cols))
	for _, c := range cols {
		lower[strings.ToLower(c)] = c
	}

	for _, cand := range candidates {
		if c, ok := lower[cand]; ok {
			return c
		}
	}
	for _, cand := range candidates {
		for lc, orig := range lower {
			if strings.Contains(lc, cand) {
				return orig
			}
		}
	}
	return ""
}

// findRecentSessionID locates the most recent session row for this project,
// trying every column as a possible project-association field before
// falling back to the most recent session overall.
func findRecentSessionID(dbPath string, cols []string, idCol, timeCol, projectID, projectPath string) string {
	var candidates []string
	if projectID != "" {
		candidates = append(candidates, projectID)
	}
	if projectPath != "" {
		candidates = append(candidates, projectPath)
	}

	var conds []string
	for _, col := range cols {
		if col == idCol {
			continue
		}
		for _, val := range candidates {
			esc := strings.ReplaceAll(val, "'", "''")
			conds = append(conds, fmt.Sprintf("%s = '%s'", col, esc))
			conds = append(conds, fmt.Sprintf("%s LIKE '%%%s%%'", col, esc))
		}
	}

	orderBy := idCol
	if timeCol != "" {
		orderBy = timeCol
	}

	selectCols := idCol
	if timeCol != "" {
		selectCols = idCol + ", " + timeCol
	}

	var queries []string
	if len(conds) > 0 {
		queries = append(queries, fmt.Sprintf(
			"SELECT %s FROM session WHERE %s ORDER BY %s DESC LIMIT 1;",
			selectCols, strings.Join(conds, " OR "), orderBy,
		))
	}
	// Fallback: best-effort match against the most recent session overall,
	// in case the project-association column/scheme couldn't be identified.
	queries = append(queries, fmt.Sprintf(
		"SELECT %s FROM session ORDER BY %s DESC LIMIT 1;",
		selectCols, orderBy,
	))

	for _, query := range queries {
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		line := strings.TrimSpace(string(out))
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		id := fields[0]
		if id == "" {
			continue
		}
		if len(fields) > 1 && !isRecentTimestamp(fields[1]) {
			continue
		}
		return id
	}
	return ""
}

// isRecentTimestamp reports whether raw (in any of several formats OpenCode
// has used for session timestamps — ISO-8601 variants or epoch
// seconds/milliseconds/microseconds) falls within RecentSessionTimeout. An
// unparseable value is treated as recent — better to attempt a match than
// silently drop a valid session over a formatting difference we don't know
// about.
func isRecentTimestamp(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}

	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		var t time.Time
		switch {
		case n > 1_000_000_000_000_000: // microseconds
			t = time.UnixMicro(n)
		case n > 1_000_000_000_000: // milliseconds
			t = time.UnixMilli(n)
		case n > 0: // seconds
			t = time.Unix(n, 0)
		default:
			return true
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
		if t, err := time.Parse(f, raw); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}
	return true
}

// loadSessionMessages fetches all messages for a session as a JSON array,
// adapting to whichever message-table schema is actually present.
func loadSessionMessages(dbPath, sessionID string) []byte {
	cols, err := sqliteTableColumns(dbPath, "message")
	if err != nil || len(cols) == 0 {
		return nil
	}

	idCol := pickColumn(cols, "id")
	sessionCol := pickColumn(cols, "session_id", "sessionid", "session")
	dataCol := pickColumn(cols, "data", "content", "json", "body")
	timeCol := pickColumn(cols, "time_created", "created", "time_updated", "updated", "time")

	if sessionCol == "" {
		return nil
	}

	orderBy := timeCol
	if orderBy == "" {
		if idCol != "" {
			orderBy = idCol
		} else {
			orderBy = sessionCol
		}
	}

	var dataExpr string
	switch {
	case dataCol != "" && idCol != "" && dataCol != idCol:
		dataExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, idCol)
	case dataCol != "":
		dataExpr = dataCol
	default:
		// No single JSON-blob column; synthesize one from all available columns.
		parts := make([]string, 0, len(cols))
		for _, c := range cols {
			parts = append(parts, fmt.Sprintf("'%s', %s", c, c))
		}
		dataExpr = fmt.Sprintf("json_object(%s)", strings.Join(parts, ", "))
	}

	escID := strings.ReplaceAll(sessionID, "'", "''")
	query := fmt.Sprintf(
		"SELECT json_group_array(%s) FROM message WHERE %s='%s' ORDER BY %s;",
		dataExpr, sessionCol, escID, orderBy,
	)

	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	transcriptData := strings.TrimSpace(string(out))
	// sqlite3 returns "[null]" when no rows match
	if transcriptData == "" || transcriptData == "[null]" || transcriptData == "[]" {
		return nil
	}
	return []byte(transcriptData)
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
