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

func (a *Agent) Name() agent.Name    { return agent.OpenCode }
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
// OpenCode's on-disk storage layout and location have changed across
// releases (flat JSON files scoped by project, SQLite databases in the
// global data dir, project-local databases, renamed tables/columns), so
// each strategy is tried in turn and, within a strategy, discovery degrades
// gracefully (e.g. falling back to a directory-wide scan) rather than
// failing outright when our assumptions about the exact layout don't hold.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first.
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite storage.
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// sessionEntry describes a candidate session found on disk.
type sessionEntry struct {
	id      string
	path    string
	isDir   bool
	modTime time.Time
}

// latestSessionEntry scans a directory (non-recursive) for the most
// recently modified session entry within the recent-session window.
// Entries may be either a "<sessionID>.json" file (older OpenCode) or a
// "<sessionID>/" directory (newer OpenCode), so both are considered.
func latestSessionEntry(dir string) *sessionEntry {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	now := time.Now()
	var best *sessionEntry

	for _, entry := range dirEntries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
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

		id := name
		if !entry.IsDir() {
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			id = strings.TrimSuffix(name, ".json")
		}

		if best == nil || modTime.After(best.modTime) {
			best = &sessionEntry{
				id:      id,
				path:    filepath.Join(dir, name),
				isDir:   entry.IsDir(),
				modTime: modTime,
			}
		}
	}

	return best
}

// hasContent reports whether dir exists and contains at least one file.
func hasContent(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			return true
		}
	}
	return false
}

// resolveTranscriptPath finds the best available location for a session's
// message data, tolerating storage layouts where messages live in a
// separate top-level directory, nested inside the session's own directory,
// or (as a last resort) only in the session entry itself.
func resolveTranscriptPath(se *sessionEntry) string {
	if msgDir, err := GetMessageDir(se.id); err == nil && hasContent(msgDir) {
		return msgDir
	}

	if se.isDir {
		for _, sub := range []string{"message", "messages"} {
			candidate := filepath.Join(se.path, sub)
			if hasContent(candidate) {
				return candidate
			}
		}
	}

	// Guaranteed to exist since we just found it via ReadDir.
	return se.path
}

// buildSessionInfo converts a discovered session entry into a SessionInfo.
func buildSessionInfo(projectPath string, se *sessionEntry) *agent.SessionInfo {
	return &agent.SessionInfo{
		SessionID:      se.id,
		TranscriptPath: resolveTranscriptPath(se),
		StartedAt:      se.modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// discoverFromFlatFiles tries the flat file session discovery.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return nil, nil
	}

	if se := latestSessionEntry(sessionDir); se != nil {
		return buildSessionInfo(projectPath, se), nil
	}

	// Our project ID (derived from the git root commit) may not match how
	// OpenCode itself scopes sessions to a project. Search across all
	// project directories for the most recently active session as a
	// best-effort fallback rather than reporting no session at all.
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}
	sessionsRoot := filepath.Join(dataDir, "storage", "session")
	projectDirs, err := os.ReadDir(sessionsRoot)
	if err != nil {
		return nil, nil
	}

	var best *sessionEntry
	for _, pd := range projectDirs {
		if !pd.IsDir() {
			continue
		}
		if se := latestSessionEntry(filepath.Join(sessionsRoot, pd.Name())); se != nil {
			if best == nil || se.modTime.After(best.modTime) {
				best = se
			}
		}
	}
	if best == nil {
		return nil, nil
	}

	return buildSessionInfo(projectPath, best), nil
}

// discoverFromSQLite queries an OpenCode SQLite database for the most
// recent session. Both the database location and its schema (table and
// column names) have changed across OpenCode releases, so the schema is
// introspected at runtime rather than assumed.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	candidates := []string{
		filepath.Join(dataDir, "opencode.db"),
		filepath.Join(projectPath, ".opencode", "opencode.db"),
	}

	for _, dbPath := range candidates {
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		if info := trySQLiteSession(dbPath, projectID, projectPath); info != nil {
			return info, nil
		}
	}

	return nil, nil
}

// trySQLiteSession introspects dbPath's schema and returns the most recent
// matching session, or nil if none is found.
func trySQLiteSession(dbPath, projectID, projectPath string) *agent.SessionInfo {
	tables := sqliteQueryJSON(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	sessionTable := findTable(tables, "session")
	messageTable := findTable(tables, "message")
	if sessionTable == "" {
		return nil
	}

	rows := sqliteQueryJSON(dbPath, fmt.Sprintf("SELECT * FROM %q;", sessionTable))
	if len(rows) == 0 {
		return nil
	}

	var best map[string]interface{}
	var bestTime time.Time
	for _, row := range rows {
		if _, ok := findStringField(row, "id"); !ok {
			continue
		}
		if !sessionMatchesProject(row, projectID, projectPath) {
			continue
		}
		t := findTimestamp(row)
		if best == nil || t.After(bestTime) {
			best = row
			bestTime = t
		}
	}
	if best == nil {
		return nil
	}
	if !bestTime.IsZero() && time.Since(bestTime) > agent.RecentSessionTimeout {
		return nil
	}

	sessionID, ok := findStringField(best, "id")
	if !ok {
		return nil
	}

	msgRows := []map[string]interface{}{}
	if messageTable != "" {
		if col := findSessionIDColumn(dbPath, messageTable); col != "" {
			if rows := sqliteQueryJSON(dbPath, fmt.Sprintf("SELECT * FROM %q WHERE %q=%s;", messageTable, col, sqliteQuote(sessionID))); len(rows) > 0 {
				msgRows = rows
			}
		}
		if len(msgRows) == 0 {
			if rows := sqliteQueryJSON(dbPath, fmt.Sprintf("SELECT * FROM %q;", messageTable)); len(rows) > 0 {
				msgRows = rows
			}
		}
	}

	transcriptData, err := json.Marshal(msgRows)
	if err != nil {
		transcriptData = []byte("[]")
	}

	startedAt := time.Now()
	if !bestTime.IsZero() {
		startedAt = bestTime
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      startedAt.Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}
}

// sqliteQueryJSON runs a query against dbPath and returns the rows decoded
// from sqlite3's JSON output mode, or nil on any failure.
func sqliteQueryJSON(dbPath, query string) []map[string]interface{} {
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(output, &rows); err != nil {
		return nil
	}
	return rows
}

// findTable returns the sqlite_master table name best matching substr
// (e.g. "session" matches both "session" and "sessions"), preferring an
// exact singular/plural match over a merely-containing one.
func findTable(tables []map[string]interface{}, substr string) string {
	var candidates []string
	for _, t := range tables {
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		if strings.Contains(strings.ToLower(name), substr) {
			candidates = append(candidates, name)
		}
	}
	for _, c := range candidates {
		lower := strings.ToLower(c)
		if lower == substr || lower == substr+"s" {
			return c
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

// findSessionIDColumn returns the message table's foreign-key column
// referencing the owning session, whatever it happens to be named.
func findSessionIDColumn(dbPath, table string) string {
	cols := sqliteQueryJSON(dbPath, fmt.Sprintf("PRAGMA table_info(%q);", table))
	for _, c := range cols {
		name, _ := c["name"].(string)
		if name != "" && strings.Contains(strings.ToLower(name), "session") {
			return name
		}
	}
	return ""
}

// findStringField returns the first non-empty string value from row whose
// key matches exactly or contains keySubstr (case-insensitive).
func findStringField(row map[string]interface{}, keySubstr string) (string, bool) {
	if v, ok := row[keySubstr]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s, true
		}
	}
	for k, v := range row {
		if strings.Contains(strings.ToLower(k), keySubstr) {
			if s, ok := v.(string); ok && s != "" {
				return s, true
			}
		}
	}
	return "", false
}

// sessionMatchesProject reports whether row appears to belong to
// projectID/projectPath. If the row has no discernible project-scoping
// column at all, it matches by default — the database is likely already
// scoped to a single project (e.g. a project-local opencode.db).
func sessionMatchesProject(row map[string]interface{}, projectID, projectPath string) bool {
	found := false
	for k, v := range row {
		lower := strings.ToLower(k)
		if !strings.Contains(lower, "project") && !strings.Contains(lower, "directory") &&
			!strings.Contains(lower, "cwd") && !strings.Contains(lower, "worktree") {
			continue
		}
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		found = true
		if s == projectID || s == projectPath ||
			strings.Contains(projectPath, s) || strings.Contains(s, projectPath) {
			return true
		}
	}
	return !found
}

// findTimestamp locates the most recent plausible timestamp field on row,
// preferring "updated" over "created" over any other "time"-like field.
func findTimestamp(row map[string]interface{}) time.Time {
	for _, key := range []string{"update", "creat", "time"} {
		var best time.Time
		for k, v := range row {
			if !strings.Contains(strings.ToLower(k), key) {
				continue
			}
			if t := parseFlexibleTime(v); t.After(best) {
				best = t
			}
		}
		if !best.IsZero() {
			return best
		}
	}
	return time.Time{}
}

// parseFlexibleTime parses a timestamp value of unknown shape: a Unix
// timestamp in seconds, milliseconds, or microseconds (as a JSON number),
// or one of several common string formats.
func parseFlexibleTime(v interface{}) time.Time {
	switch val := v.(type) {
	case float64:
		n := int64(val)
		switch {
		case n <= 0:
			return time.Time{}
		case n > 1e14:
			return time.UnixMicro(n)
		case n > 1e11:
			return time.UnixMilli(n)
		default:
			return time.Unix(n, 0)
		}
	case string:
		formats := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
		for _, f := range formats {
			if t, err := time.Parse(f, val); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// sqliteQuote wraps s in single quotes for use as a SQL string literal.
func sqliteQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
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
