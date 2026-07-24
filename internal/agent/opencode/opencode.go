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
// It first tries flat file storage, then falls back to SQLite. OpenCode's
// on-disk storage layout (directory structure, and SQLite table/column
// names) has changed across releases, so both discovery paths adapt to
// several known/plausible layouts instead of assuming a single fixed one.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// discoverFromFlatFiles tries OpenCode's JSON file-backed session storage.
// OpenCode has used more than one on-disk layout across releases:
//   - <dataDir>/storage/session/<projectID>/<sessionID>.json  (per-project directory)
//   - <dataDir>/storage/session/<sessionID>.json               (flat, project embedded in the file)
//   - <dataDir>/storage/session/info/<sessionID>.json          (namespaced "info" key)
// Each is tried in turn. For the per-project directory, files are accepted
// as-is (the directory itself already scopes them to the project). For the
// flatter layouts, files are matched by an embedded project reference
// (projectID/directory/worktree/cwd) when present.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}
	projectID := GetProjectID(projectPath)

	candidateDirs := []string{
		filepath.Join(dataDir, "storage", "session", projectID),
		filepath.Join(dataDir, "storage", "session"),
		filepath.Join(dataDir, "storage", "session", "info"),
	}

	for _, dir := range candidateDirs {
		if session := findRecentSessionInDir(dir, dataDir, projectID, projectPath); session != nil {
			return session, nil
		}
	}

	return nil, nil
}

// findRecentSessionInDir scans dir (non-recursively) for the most recently
// modified *.json session file within the recency window that belongs to
// the given project.
func findRecentSessionInDir(dir, dataDir, projectID, projectPath string) *agent.SessionInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	now := time.Now()
	var bestID string
	var bestModTime time.Time

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			continue
		}
		if bestID != "" && !modTime.After(bestModTime) {
			continue
		}

		sessionID := strings.TrimSuffix(entry.Name(), ".json")

		if data, err := os.ReadFile(filepath.Join(dir, entry.Name())); err == nil {
			var fields map[string]json.RawMessage
			if json.Unmarshal(data, &fields) == nil {
				if !sessionMatchesProject(fields, projectID, projectPath) {
					continue
				}
				if id := stringFromRaw(fields["id"]); id != "" {
					sessionID = id
				}
			}
		}

		bestID = sessionID
		bestModTime = modTime
	}

	if bestID == "" {
		return nil
	}

	return &agent.SessionInfo{
		SessionID:      bestID,
		TranscriptPath: resolveMessageDir(dataDir, bestID),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// sessionMatchesProject reports whether a session's JSON fields identify it
// as belonging to the given project. If the session file has no recognizable
// project field, it is treated as a match — this lets us accept files from
// directories that are already project-scoped by location (e.g. the legacy
// per-project directory layout).
func sessionMatchesProject(fields map[string]json.RawMessage, projectID, projectPath string) bool {
	for _, key := range []string{"projectID", "project_id", "project"} {
		if raw, ok := fields[key]; ok {
			return stringFromRaw(raw) == projectID
		}
	}
	for _, key := range []string{"directory", "worktree", "cwd", "path"} {
		if raw, ok := fields[key]; ok {
			return agent.PathsEqual(stringFromRaw(raw), projectPath)
		}
	}
	return true
}

// stringFromRaw unmarshals a raw JSON string value, returning "" on failure.
func stringFromRaw(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// resolveMessageDir returns the message storage directory for a session,
// trying the known layouts used across OpenCode releases and falling back
// to the original "storage/message/<sessionID>" layout if none exist yet
// (parseMessageDir tolerates a missing/empty directory).
func resolveMessageDir(dataDir, sessionID string) string {
	candidates := []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
	}
	for _, dir := range candidates {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return candidates[0]
}

// discoverFromSQLite queries an OpenCode SQLite database for the most recent
// session belonging to a project. OpenCode has stored this database in
// different locations and with different table/column names across
// releases (e.g. a global "<dataDir>/opencode.db" scoped by a project_id
// column, or a project-local ".opencode/opencode.db" with no project column
// at all since the database file itself is already project-scoped). Rather
// than hardcoding one release's schema, the actual tables/columns are
// introspected at query time.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	candidates := []string{
		filepath.Join(projectPath, ".opencode", "opencode.db"),
		filepath.Join(dataDir, "opencode.db"),
	}

	for _, dbPath := range candidates {
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		if session := querySQLiteSession(dbPath, projectID, projectPath); session != nil {
			return session, nil
		}
	}

	return nil, nil
}

// querySQLiteSession looks up the most recent session for a project in the
// given SQLite database, adapting to whichever table/column names are
// actually present.
func querySQLiteSession(dbPath, projectID, projectPath string) *agent.SessionInfo {
	sessionTable, ok := findSQLiteTable(dbPath, "session", "sessions")
	if !ok {
		return nil
	}
	cols := sqliteTableColumns(dbPath, sessionTable)
	if !cols["id"] {
		return nil
	}

	orderCol := firstPresentColumn(cols, "time_updated", "updated_at", "time_created", "created_at", "id")

	where := ""
	if projCol := firstPresentColumn(cols, "project_id", "projectID", "project"); projCol != "" {
		where = fmt.Sprintf(" WHERE %s='%s'", projCol, sqliteEscape(projectID))
	} else if dirCol := firstPresentColumn(cols, "directory", "worktree", "cwd", "path"); dirCol != "" {
		where = fmt.Sprintf(" WHERE %s='%s'", dirCol, sqliteEscape(projectPath))
	}
	// If neither a project nor a directory column exists, the database is
	// assumed to already be project-scoped (e.g. a project-local
	// ".opencode/opencode.db"), so no filter is applied.

	query := fmt.Sprintf("SELECT id FROM %s%s ORDER BY %s DESC LIMIT 1;", sessionTable, where, orderCol)
	sessionID, err := runSQLite(dbPath, query)
	if err != nil || sessionID == "" {
		return nil
	}

	if timeOut, err := runSQLite(dbPath, fmt.Sprintf("SELECT %s FROM %s WHERE id='%s';", orderCol, sessionTable, sqliteEscape(sessionID))); err == nil && timeOut != "" {
		if !isRecentTimestamp(timeOut) {
			return nil
		}
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "",
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: fetchSQLiteMessages(dbPath, sessionID),
	}
}

// fetchSQLiteMessages returns a JSON array of messages for a session,
// adapting to either a single "data" blob column (whole message JSON, used
// by earlier OpenCode releases) or a structured schema with role/content
// (or role/parts) columns.
func fetchSQLiteMessages(dbPath, sessionID string) []byte {
	msgTable, ok := findSQLiteTable(dbPath, "message", "messages")
	if !ok {
		return nil
	}
	cols := sqliteTableColumns(dbPath, msgTable)

	sessionCol := firstPresentColumn(cols, "session_id", "sessionID", "session")
	if sessionCol == "" {
		return nil
	}
	orderCol := firstPresentColumn(cols, "time_created", "created_at", "id")

	roleExpr := "''"
	if cols["role"] {
		roleExpr = "role"
	}

	var selectExpr string
	switch {
	case cols["data"]:
		selectExpr = "json_group_array(json_patch(data, json_object('id', id)))"
	case cols["parts"]:
		selectExpr = fmt.Sprintf("json_group_array(json_object('id', id, 'role', %s, 'parts', json(parts)))", roleExpr)
	case cols["content"]:
		selectExpr = fmt.Sprintf("json_group_array(json_object('id', id, 'role', %s, 'content', content))", roleExpr)
	default:
		return nil
	}

	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s' ORDER BY %s;", selectExpr, msgTable, sessionCol, sqliteEscape(sessionID), orderCol)
	out, err := runSQLite(dbPath, query)
	if err != nil || out == "" || out == "[null]" || out == "[]" {
		return nil
	}
	return []byte(out)
}

// runSQLite executes a query against dbPath and returns the trimmed output.
func runSQLite(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// findSQLiteTable returns the first candidate table name that exists in the
// database.
func findSQLiteTable(dbPath string, candidates ...string) (string, bool) {
	out, err := runSQLite(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return "", false
	}
	tables := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			tables[name] = true
		}
	}
	for _, c := range candidates {
		if tables[c] {
			return c, true
		}
	}
	return "", false
}

// sqliteTableColumns returns the set of column names for a table via
// PRAGMA table_info, whose default output is pipe-separated:
// cid|name|type|notnull|dflt_value|pk
func sqliteTableColumns(dbPath, table string) map[string]bool {
	cols := make(map[string]bool)
	out, err := runSQLite(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return cols
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			cols[fields[1]] = true
		}
	}
	return cols
}

// firstPresentColumn returns the first candidate present in cols, or "".
func firstPresentColumn(cols map[string]bool, candidates ...string) string {
	for _, c := range candidates {
		if cols[c] {
			return c
		}
	}
	return ""
}

// sqliteEscape escapes single quotes for use in a single-quoted SQLite string literal.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// isRecentTimestamp reports whether a timestamp string (ISO-8601 in a few
// common variants, or a Unix epoch in seconds/milliseconds/microseconds/
// nanoseconds) falls within the recent-session window. If the value can't
// be parsed, it is treated as recent so an unrecognized formatting change
// doesn't silently drop a valid session.
func isRecentTimestamp(s string) bool {
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		var t time.Time
		switch {
		case n > 1e17:
			t = time.Unix(0, n) // nanoseconds
		case n > 1e14:
			t = time.Unix(0, n*1e3) // microseconds
		case n > 1e11:
			t = time.Unix(0, n*1e6) // milliseconds
		default:
			t = time.Unix(n, 0) // seconds
		}
		return time.Since(t) <= agent.RecentSessionTimeout
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

	// Try "parts" field: newer OpenCode releases represent message content as
	// an array of typed parts, e.g. [{"type":"text","text":"..."}] or
	// [{"type":"text","data":{"text":"..."}}].
	if partsRaw, ok := raw["parts"]; ok {
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(partsRaw, &parts); err == nil {
			var blocks []agent.ContentBlock
			for _, p := range parts {
				if text := extractPartText(p); text != "" {
					blocks = append(blocks, agent.ContentBlock{Type: "text", Text: text})
				}
			}
			if len(blocks) > 0 {
				msg.Content = blocks
				return msg
			}
		}
	}

	return msg
}

// extractPartText pulls a text value out of a single OpenCode message part,
// trying both a flat "text" field and a nested "data.text" field.
func extractPartText(p map[string]json.RawMessage) string {
	if raw, ok := p["text"]; ok {
		if s := stringFromRaw(raw); s != "" {
			return s
		}
	}
	if raw, ok := p["data"]; ok {
		var data map[string]json.RawMessage
		if json.Unmarshal(raw, &data) == nil {
			if textRaw, ok := data["text"]; ok {
				return stringFromRaw(textRaw)
			}
		}
	}
	return ""
}
```
