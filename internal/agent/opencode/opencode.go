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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
// OpenCode's SQLite schema has changed across versions before (e.g. column renames),
// so this introspects the actual schema at runtime via PRAGMA table_info instead of
// assuming fixed column names, and prefers matching on a stable directory/path column
// before falling back to the computed project ID (root commit hash).
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findOpenCodeDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionCols := sqliteTableColumns(dbPath, "session")
	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := pickColumn(sessionCols, "time_updated", "updated_at", "updatedAt", "time_modified", "modified")

	sessionID, timeStr := findRecentSession(dbPath, "session", idCol, timeCol, buildSessionCandidates(sessionCols, projectID, projectPath))
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout)
	if timeStr != "" && !isRecent(timeStr) {
		return nil, nil
	}

	// We already found the right session — if fetching its message content
	// fails (e.g. the message table schema changed too), still return the
	// session with an empty transcript rather than dropping it entirely.
	transcriptData := fetchSessionMessages(dbPath, sessionID)

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// findOpenCodeDB locates the OpenCode SQLite database, checking a few
// plausible locations since OpenCode has relocated it before.
func findOpenCodeDB(dataDir string) string {
	for _, candidate := range []string{
		filepath.Join(dataDir, "opencode.db"),
		filepath.Join(dataDir, "storage", "opencode.db"),
		filepath.Join(dataDir, "db", "opencode.db"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// sqliteColumnMatch pairs a column name with the value to match against it.
type sqliteColumnMatch struct {
	column string
	value  string
}

// buildSessionCandidates returns an ordered list of (column, value) matches to
// try when looking up the session for a project. Directory/path columns are
// tried first since they're a stable identifier independent of any hashing
// scheme; the computed project ID (root commit hash) is tried as a fallback
// for schemas that still use it.
func buildSessionCandidates(sessionCols []string, projectID, projectPath string) []sqliteColumnMatch {
	var candidates []sqliteColumnMatch
	for _, name := range []string{"directory", "path", "cwd", "worktree"} {
		if col := pickColumn(sessionCols, name); col != "" {
			candidates = append(candidates, sqliteColumnMatch{col, projectPath})
		}
	}
	for _, name := range []string{"project_id", "projectID", "project"} {
		if col := pickColumn(sessionCols, name); col != "" {
			candidates = append(candidates, sqliteColumnMatch{col, projectID})
		}
	}
	return candidates
}

// findRecentSession tries each candidate match in turn, returning the id (and
// optionally the time column value) of the most recently updated matching row.
func findRecentSession(dbPath, table, idCol, timeCol string, candidates []sqliteColumnMatch) (id string, timeStr string) {
	for _, c := range candidates {
		selectCols := idCol
		if timeCol != "" {
			selectCols += ", " + timeCol
		}
		orderBy := ""
		if timeCol != "" {
			orderBy = " ORDER BY " + timeCol + " DESC"
		}
		query := fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s=%s%s LIMIT 1;`,
			selectCols, table, c.column, sqliteQuote(c.value), orderBy,
		)
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		line := strings.TrimSpace(string(out))
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if parts[0] == "" {
			continue
		}
		id = parts[0]
		if len(parts) > 1 {
			timeStr = parts[1]
		}
		return id, timeStr
	}
	return "", ""
}

// isRecent reports whether timeStr, in any of OpenCode's known time formats
// (including Unix epoch milliseconds), is within RecentSessionTimeout. If the
// value can't be parsed at all, it defaults to true — better to try than skip.
func isRecent(timeStr string) bool {
	if t, err := time.Parse(time.RFC3339Nano, timeStr); err == nil {
		return time.Since(t) <= agent.RecentSessionTimeout
	}
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", timeStr); err == nil {
		return time.Since(t) <= agent.RecentSessionTimeout
	}
	if t, err := time.Parse("2006-01-02 15:04:05", timeStr); err == nil {
		return time.Since(t) <= agent.RecentSessionTimeout
	}
	if ms, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		return time.Since(time.UnixMilli(ms)) <= agent.RecentSessionTimeout
	}
	return true
}

// fetchSessionMessages returns the messages for a session as a JSON array,
// adapting to schema changes in the message table via PRAGMA table_info. If
// the message schema can't be resolved or the query fails, it returns an
// empty array rather than dropping the session — the session itself was
// already found, so a note with no transcript detail beats no note at all.
func fetchSessionMessages(dbPath, sessionID string) []byte {
	empty := []byte("[]")

	msgCols := sqliteTableColumns(dbPath, "message")
	idCol := pickColumn(msgCols, "id")
	sessionFKCol := pickColumn(msgCols, "session_id", "sessionID", "session")
	dataCol := pickColumn(msgCols, "data", "content", "body", "payload", "parts")
	timeCol := pickColumn(msgCols, "time_created", "created_at", "createdAt", "time")

	if idCol == "" || sessionFKCol == "" || dataCol == "" {
		return empty
	}

	orderClause := ""
	if timeCol != "" {
		orderClause = " ORDER BY " + timeCol
	}
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM message WHERE %s=%s%s;`,
		dataCol, idCol, sessionFKCol, sqliteQuote(sessionID), orderClause,
	)
	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	out, err := cmd.Output()
	if err != nil {
		return empty
	}

	transcriptData := []byte(strings.TrimSpace(string(out)))
	if len(transcriptData) == 0 || string(transcriptData) == "[null]" {
		return empty
	}

	return transcriptData
}

// sqliteTableColumns returns the column names of a SQLite table (nil if the
// table doesn't exist or sqlite3 fails). Used to adapt queries to schema
// changes across OpenCode versions instead of assuming fixed column names.
func sqliteTableColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", "-json", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil
	}
	cols := make([]string, 0, len(rows))
	for _, r := range rows {
		cols = append(cols, r.Name)
	}
	return cols
}

// pickColumn returns the first candidate present in cols, or "" if none match.
func pickColumn(cols []string, candidates ...string) string {
	set := make(map[string]bool, len(cols))
	for _, c := range cols {
		set[c] = true
	}
	for _, c := range candidates {
		if set[c] {
			return c
		}
	}
	return ""
}

// sqliteQuote escapes a string for embedding as a SQLite string literal.
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
