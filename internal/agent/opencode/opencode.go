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
//
// OpenCode has changed how sessions are keyed to a project across versions
// (older releases derived a project directory/id from the git root commit
// hash; newer releases may use a different scheme entirely). Rather than
// trusting our own guess at that identifier, both discovery paths verify
// candidate sessions against projectPath using the session's own recorded
// directory, falling back to recency when that isn't available — the same
// pattern used by the Claude and Copilot agents.
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
// It scans every project directory under storage/session (not just the one
// keyed by our best-guess project id) and prefers sessions whose own
// recorded directory field matches projectPath, falling back to the most
// recently modified recent session when no directory match is found.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	projectDirs, err := os.ReadDir(sessionRoot)
	if err != nil {
		return nil, nil
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout
	var bestSessionID string
	var bestModTime time.Time
	var bestDirMatch bool
	haveBest := false

	for _, projectDir := range projectDirs {
		if !projectDir.IsDir() {
			continue
		}

		dir := filepath.Join(sessionRoot, projectDir.Name())
		dirEntries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}

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

			dirMatch := flatSessionFileMatchesProject(filepath.Join(dir, entry.Name()), projectPath)

			if !haveBest || (dirMatch && !bestDirMatch) || (dirMatch == bestDirMatch && modTime.After(bestModTime)) {
				bestSessionID = strings.TrimSuffix(entry.Name(), ".json")
				bestModTime = modTime
				bestDirMatch = dirMatch
				haveBest = true
			}
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

// flatSessionFileMatchesProject reports whether a flat-file session JSON
// document's recorded directory matches projectPath.
func flatSessionFileMatchesProject(path, projectPath string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var s struct {
		Directory string `json:"directory"`
		Path      string `json:"path"`
		Cwd       string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return false
	}
	for _, d := range []string{s.Directory, s.Path, s.Cwd} {
		if d != "" && agent.PathsEqual(d, projectPath) {
			return true
		}
	}
	return false
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// relevant recent session. Column names are discovered at runtime (via
// PRAGMA table_info) instead of hardcoded, since OpenCode has renamed/added
// columns across versions; sessions are additionally verified against
// projectPath using whatever directory-like field is available, rather than
// trusting an exact match on our own guessed project identifier.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionID := findSessionIDBySQLite(dbPath, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	transcriptData := readMessagesFromSQLite(dbPath, sessionID)
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

// findSessionIDBySQLite locates the most relevant recent session for
// projectPath among the session table's most recently inserted rows.
func findSessionIDBySQLite(dbPath, projectID, projectPath string) string {
	// -json gives us structured rows so we can safely inspect whatever
	// columns exist (including a possible embedded JSON "data" blob) without
	// hardcoding the schema.
	cmd := exec.Command("sqlite3", "-json", dbPath,
		"SELECT * FROM session ORDER BY rowid DESC LIMIT 50;")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) == 0 {
		return ""
	}

	now := time.Now()
	var bestID string
	var bestTime time.Time
	var bestDirMatch bool
	haveBest := false

	for _, row := range rows {
		flat := flattenSQLiteRow(row)

		id, _ := flat["id"].(string)
		if id == "" {
			continue
		}

		if t, ok := sqliteRowTime(flat); ok {
			if now.Sub(t) > agent.RecentSessionTimeout {
				continue
			}
			if !haveBest || t.After(bestTime) {
				// Track the newest timestamp seen among matches at the current
				// match tier; tie-breaking below still respects dirMatch.
			}
			dirMatch := sqliteRowMatchesProject(flat, projectID, projectPath)
			if !haveBest || (dirMatch && !bestDirMatch) || (dirMatch == bestDirMatch && t.After(bestTime)) {
				bestID = id
				bestTime = t
				bestDirMatch = dirMatch
				haveBest = true
			}
			continue
		}

		// No parseable timestamp: only use this row if we don't have a
		// better (timestamped or directory-matched) candidate yet.
		dirMatch := sqliteRowMatchesProject(flat, projectID, projectPath)
		if !haveBest || (dirMatch && !bestDirMatch) {
			bestID = id
			bestTime = time.Time{}
			bestDirMatch = dirMatch
			haveBest = true
		}
	}

	return bestID
}

// flattenSQLiteRow merges an embedded JSON "data" column (if present) into
// the row's top-level fields, so callers can look up fields regardless of
// whether OpenCode stores them as indexed columns or inside a JSON blob.
func flattenSQLiteRow(row map[string]interface{}) map[string]interface{} {
	flat := make(map[string]interface{}, len(row))
	for k, v := range row {
		flat[k] = v
	}
	if raw, ok := row["data"].(string); ok && raw != "" {
		var nested map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &nested); err == nil {
			for k, v := range nested {
				if _, exists := flat[k]; !exists {
					flat[k] = v
				}
			}
		}
	}
	return flat
}

// sqliteRowMatchesProject reports whether a (flattened) session row's
// recorded directory matches projectPath, falling back to comparing against
// our own best-guess project identifier for backward compatibility with
// OpenCode versions that key sessions by git root commit hash.
func sqliteRowMatchesProject(flat map[string]interface{}, projectID, projectPath string) bool {
	for _, key := range []string{"directory", "path", "cwd", "worktree"} {
		if v, ok := flat[key].(string); ok && v != "" && agent.PathsEqual(v, projectPath) {
			return true
		}
	}
	for _, key := range []string{"project_id", "projectID", "project"} {
		if v, ok := flat[key].(string); ok && v != "" && v == projectID {
			return true
		}
	}
	return false
}

// sqliteRowTime extracts a best-effort last-updated timestamp from a
// (flattened) session row, checking both a nested {"time": {...}} shape
// (mirroring OpenCode's message records) and a variety of common flat
// column names.
func sqliteRowTime(flat map[string]interface{}) (time.Time, bool) {
	if nested, ok := flat["time"].(map[string]interface{}); ok {
		for _, key := range []string{"updated", "created"} {
			if v, ok := nested[key]; ok {
				if t, ok := parseSQLiteTimeValue(v); ok {
					return t, true
				}
			}
		}
	}

	for _, key := range []string{
		"time_updated", "updatedAt", "updated_at", "updated",
		"time_created", "createdAt", "created_at", "created",
	} {
		if v, ok := flat[key]; ok {
			if t, ok := parseSQLiteTimeValue(v); ok {
				return t, true
			}
		}
	}

	return time.Time{}, false
}

// parseSQLiteTimeValue parses a timestamp that may arrive as an RFC3339-ish
// string or as a numeric (seconds or milliseconds) epoch value.
func parseSQLiteTimeValue(v interface{}) (time.Time, bool) {
	switch val := v.(type) {
	case string:
		return parseSQLiteTimeString(val)
	case float64:
		return epochToTime(int64(val)), true
	}
	return time.Time{}, false
}

func parseSQLiteTimeString(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return epochToTime(n), true
	}

	return time.Time{}, false
}

// epochToTime interprets an integer as a millisecond epoch when it's large
// enough to only make sense that way, otherwise as a second epoch.
func epochToTime(n int64) time.Time {
	if n > 1_000_000_000_000 {
		return time.UnixMilli(n)
	}
	return time.Unix(n, 0)
}

// readMessagesFromSQLite reads all messages for sessionID as a JSON array.
// Column names are discovered at runtime since they've changed across
// OpenCode versions.
func readMessagesFromSQLite(dbPath, sessionID string) []byte {
	msgCols := sqliteColumns(dbPath, "message")
	sessionIDCol := pickColumn(msgCols, "session_id", "sessionID", "sessionId")
	dataCol := pickColumn(msgCols, "data", "content", "message")
	idCol := pickColumn(msgCols, "id")
	timeCol := pickColumn(msgCols, "time_created", "created", "createdAt", "created_at", "time_updated")

	if sessionIDCol == "" || dataCol == "" {
		return nil
	}

	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s", timeCol)
	}

	var query string
	if idCol != "" {
		query = fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM message WHERE %s='%s'%s;`,
			dataCol, idCol, sessionIDCol, sqliteEscape(sessionID), orderClause,
		)
	} else {
		query = fmt.Sprintf(
			`SELECT json_group_array(%s) FROM message WHERE %s='%s'%s;`,
			dataCol, sessionIDCol, sqliteEscape(sessionID), orderClause,
		)
	}

	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	data := strings.TrimSpace(string(out))
	// sqlite3 returns "[null]" when no rows match
	if data == "" || data == "[null]" || data == "[]" {
		return nil
	}

	return []byte(data)
}

// sqliteColumns returns the column names for table, discovered via
// PRAGMA table_info so queries can adapt to schema changes across versions.
func sqliteColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) >= 2 && fields[1] != "" {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// pickColumn returns the first candidate name present in cols
// (case-insensitive), or "" if none are present.
func pickColumn(cols []string, candidates ...string) string {
	set := make(map[string]string, len(cols))
	for _, c := range cols {
		set[strings.ToLower(c)] = c
	}
	for _, cand := range candidates {
		if actual, ok := set[strings.ToLower(cand)]; ok {
			return actual
		}
	}
	return ""
}

// sqliteEscape escapes single quotes for safe inclusion in a SQL string
// literal passed to the sqlite3 CLI.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
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
