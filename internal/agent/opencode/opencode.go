```go
package opencode

import (
	"encoding/json"
	"errors"
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
//
// OpenCode has repeatedly changed its SQLite schema across releases (table
// names like "session" vs "sessions", columns like "project_id" vs
// "projectID", timestamp encodings, and whether message content lives in a
// single blob column or is spread across typed columns). Rather than assume
// one fixed shape, this introspects the actual schema at runtime via
// sqlite_master/PRAGMA metadata and adapts the query accordingly, so a
// rename or restructure doesn't silently break discovery.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := locateOpenCodeDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	tables, err := sqliteTableNames(dbPath)
	if err != nil || len(tables) == 0 {
		return nil, nil
	}

	sessionTable := pickTable(tables, "session")
	messageTable := pickTable(tables, "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionID, rawTime := findRecentSessionID(dbPath, sessionTable, projectID)
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout). If the timestamp
	// can't be parsed, proceed anyway — better to try than skip.
	if rawTime != "" {
		if t, ok := parseFlexibleTime(rawTime); ok && time.Since(t) > agent.RecentSessionTimeout {
			return nil, nil
		}
	}

	// Get messages for this session as a JSON array
	transcriptData, err := sqliteSessionMessages(dbPath, messageTable, sessionID)
	if err != nil {
		return nil, nil
	}

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

// errDBFound aborts a filepath.Walk as soon as a candidate database file is located.
var errDBFound = errors.New("db found")

// locateOpenCodeDB finds OpenCode's SQLite database. It tries the historical
// "opencode.db" filename first, then searches the data directory for any
// *.db/*.sqlite/*.sqlite3 file in case the filename or layout has changed.
func locateOpenCodeDB(dataDir string) string {
	primary := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(primary); err == nil {
		return primary
	}

	var found string
	_ = filepath.Walk(dataDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		name := strings.ToLower(info.Name())
		if strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".sqlite") || strings.HasSuffix(name, ".sqlite3") {
			found = p
			return errDBFound
		}
		return nil
	})
	return found
}

// sqliteTableNames lists the user tables present in the OpenCode SQLite database.
func sqliteTableNames(dbPath string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// pickTable returns the shortest table name containing keyword, skipping
// SQLite's own internal tables and full-text-search shadow tables.
func pickTable(tables []string, keyword string) string {
	best := ""
	for _, t := range tables {
		lt := strings.ToLower(t)
		if !strings.Contains(lt, keyword) {
			continue
		}
		if strings.HasPrefix(lt, "sqlite_") || strings.Contains(lt, "_fts") || strings.Contains(lt, "fts_") {
			continue
		}
		if best == "" || len(t) < len(best) {
			best = t
		}
	}
	return best
}

// sqliteTableColumns lists column names for a table via PRAGMA table_info.
func sqliteTableColumns(dbPath, table string) ([]string, error) {
	cmd := exec.Command("sqlite3", "-separator", "|", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) >= 2 && parts[1] != "" {
			cols = append(cols, parts[1])
		}
	}
	return cols, nil
}

// pickColumn returns the first column matching (case-insensitively) one of
// the candidate names, then falls back to a substring match.
func pickColumn(cols []string, candidates ...string) string {
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.EqualFold(c, cand) {
				return c
			}
		}
	}
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.Contains(strings.ToLower(c), cand) {
				return c
			}
		}
	}
	return ""
}

// escapeSQLiteString escapes single quotes for use in a SQLite string literal.
func escapeSQLiteString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// findRecentSessionID finds the most recently updated session ID for a
// project, returning its ID and a raw timestamp value (column/format
// unknown up front, so the caller parses it leniently). If no
// project-identifying column can be found at all, it falls back to the
// single most recent session overall, which is correct in the common case
// of one active session (e.g. a fresh CI sandbox).
func findRecentSessionID(dbPath, sessionTable, projectID string) (id string, rawTime string) {
	cols, err := sqliteTableColumns(dbPath, sessionTable)
	if err != nil || len(cols) == 0 {
		return "", ""
	}

	idCol := pickColumn(cols, "id")
	if idCol == "" {
		return "", ""
	}
	projectCol := pickColumn(cols, "project_id", "projectid", "project")
	timeCol := pickColumn(cols, "time_updated", "updated_at", "updated", "time_created", "created_at", "created", "mtime", "timestamp")

	runQuery := func(withProjectFilter bool) (string, string) {
		sel := idCol
		if timeCol != "" {
			sel += ", " + timeCol
		}
		query := fmt.Sprintf("SELECT %s FROM %s", sel, sessionTable)
		if withProjectFilter && projectCol != "" {
			query += fmt.Sprintf(" WHERE %s = '%s'", projectCol, escapeSQLiteString(projectID))
		}
		if timeCol != "" {
			query += fmt.Sprintf(" ORDER BY %s DESC", timeCol)
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
		parts := strings.SplitN(line, "\t", 2)
		gotID := strings.TrimSpace(parts[0])
		gotTime := ""
		if len(parts) > 1 {
			gotTime = strings.TrimSpace(parts[1])
		}
		return gotID, gotTime
	}

	if projectCol != "" {
		return runQuery(true)
	}

	return runQuery(false)
}

// sqliteSessionMessages retrieves all messages for a session as a JSON array.
// When the message table has a single JSON blob column (OpenCode's
// historical shape), it patches the id (and role, if separate) into that
// blob. Otherwise it builds a JSON object from every column so the flexible
// transcript parser (parseOpenCodeEntry) can pick out whichever fields
// exist, regardless of exactly how the table is structured.
func sqliteSessionMessages(dbPath, messageTable, sessionID string) ([]byte, error) {
	cols, err := sqliteTableColumns(dbPath, messageTable)
	if err != nil || len(cols) == 0 {
		return nil, fmt.Errorf("could not read %s columns", messageTable)
	}

	sessionCol := pickColumn(cols, "session_id", "sessionid", "session")
	orderCol := pickColumn(cols, "time_created", "created_at", "created", "time", "timestamp")
	blobCol := pickColumn(cols, "data", "content", "body", "payload", "json")
	idCol := pickColumn(cols, "id")

	var jsonExpr string
	if blobCol != "" && idCol != "" && blobCol != idCol {
		extras := []string{fmt.Sprintf("'id', %s", idCol)}
		if roleCol := pickColumn(cols, "role", "type"); roleCol != "" && roleCol != blobCol {
			extras = append(extras, fmt.Sprintf("'%s', %s", roleCol, roleCol))
		}
		jsonExpr = fmt.Sprintf("json_patch(%s, json_object(%s))", blobCol, strings.Join(extras, ", "))
	} else {
		pairs := make([]string, 0, len(cols))
		for _, c := range cols {
			pairs = append(pairs, fmt.Sprintf("'%s', %s", c, c))
		}
		jsonExpr = fmt.Sprintf("json_object(%s)", strings.Join(pairs, ", "))
	}

	query := fmt.Sprintf("SELECT json_group_array(%s) FROM %s", jsonExpr, messageTable)
	if sessionCol != "" {
		query += fmt.Sprintf(" WHERE %s = '%s'", sessionCol, escapeSQLiteString(sessionID))
	}
	if orderCol != "" {
		query += fmt.Sprintf(" ORDER BY %s", orderCol)
	} else {
		query += " ORDER BY rowid"
	}
	query += ";"

	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(string(out))), nil
}

// parseFlexibleTime parses a timestamp value in any of the formats OpenCode
// has used: RFC3339(Nano), a millisecond-precision "Z" format, a bare
// "YYYY-MM-DD HH:MM:SS" form, or a Unix epoch (seconds or milliseconds) as
// used by most JavaScript-based tooling.
func parseFlexibleTime(s string) (time.Time, bool) {
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		switch {
		case n > 1e12:
			return time.UnixMilli(n), true
		case n > 1e9:
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
