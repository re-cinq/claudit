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
// OpenCode's on-disk storage layout has changed across releases (flat JSON
// files namespaced by project, a flat JSON "session/info" directory keyed by
// a "directory" field, and SQLite), so each strategy is attempted in turn and
// the SQLite path introspects the schema rather than assuming fixed table and
// column names.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	if session, err := a.discoverFromProjectDir(projectPath); err != nil {
		return nil, err
	} else if session != nil {
		return session, nil
	}

	if session, err := a.discoverFromSessionInfoDir(projectPath); err != nil {
		return nil, err
	} else if session != nil {
		return session, nil
	}

	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// discoverFromProjectDir tries the flat file session discovery layout where
// sessions are namespaced under a per-project directory:
// <dataDir>/storage/session/<projectID>/<sessionID>.json
func (a *Agent) discoverFromProjectDir(projectPath string) (*agent.SessionInfo, error) {
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

	return sessionInfoForMessages(bestSessionID, bestModTime, projectPath), nil
}

// discoverFromSessionInfoDir tries the alternate layout where session
// metadata lives in a single flat directory (<dataDir>/storage/session/info)
// annotated with a "directory" and/or "projectID" field, rather than being
// namespaced into per-project subdirectories.
func (a *Agent) discoverFromSessionInfoDir(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	infoDir := filepath.Join(dataDir, "storage", "session", "info")
	dirEntries, err := os.ReadDir(infoDir)
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	now := time.Now()

	var bestSessionID, bestAnySessionID string
	var bestModTime, bestAnyModTime time.Time

	for _, entry := range dirEntries {
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

		// Track the most recent session regardless of project match, as a
		// last-resort fallback if nothing matches explicitly.
		if bestAnySessionID == "" || modTime.After(bestAnyModTime) {
			id := strings.TrimSuffix(entry.Name(), ".json")
			bestAnySessionID = id
			bestAnyModTime = modTime
		}

		data, err := os.ReadFile(filepath.Join(infoDir, entry.Name()))
		if err != nil {
			continue
		}

		var meta struct {
			ID        string `json:"id"`
			Directory string `json:"directory"`
			ProjectID string `json:"projectID"`
			ProjectID2 string `json:"project_id"`
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}

		matches := (meta.Directory != "" && agent.PathsEqual(meta.Directory, projectPath)) ||
			(meta.ProjectID != "" && meta.ProjectID == projectID) ||
			(meta.ProjectID2 != "" && meta.ProjectID2 == projectID)
		if !matches {
			continue
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			id := meta.ID
			if id == "" {
				id = strings.TrimSuffix(entry.Name(), ".json")
			}
			bestSessionID = id
			bestModTime = modTime
		}
	}

	sessionID, modTime := bestSessionID, bestModTime
	if sessionID == "" {
		// Nothing explicitly matched this project; better to use the most
		// recent session found than to report no session at all.
		sessionID, modTime = bestAnySessionID, bestAnyModTime
	}
	if sessionID == "" {
		return nil, nil
	}

	return sessionInfoForMessages(sessionID, modTime, projectPath), nil
}

// sessionInfoForMessages resolves a discovered session ID to a SessionInfo,
// locating its messages under either known message directory layout. If
// neither layout is found on disk, it falls back to an empty inline
// transcript so the caller can still record the commit-session link.
func sessionInfoForMessages(sessionID string, startedAt time.Time, projectPath string) *agent.SessionInfo {
	info := &agent.SessionInfo{
		SessionID:   sessionID,
		StartedAt:   startedAt.Format(time.RFC3339),
		ProjectPath: projectPath,
	}

	dataDir, err := GetDataDir()
	if err == nil {
		nested := filepath.Join(dataDir, "storage", "session", "message", sessionID)
		if st, statErr := os.Stat(nested); statErr == nil && st.IsDir() {
			info.TranscriptPath = nested
			return info
		}
	}

	if flat, err := GetMessageDir(sessionID); err == nil {
		if st, statErr := os.Stat(flat); statErr == nil && st.IsDir() {
			info.TranscriptPath = flat
			return info
		}
	}

	// No message directory found for this session — still report the
	// session so a note gets attached, with an empty transcript.
	info.TranscriptData = []byte("[]")
	return info
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session. OpenCode's SQLite schema is not assumed to be fixed: table and
// column names are discovered dynamically via sqlite_master/PRAGMA table_info
// so this keeps working across releases that rename or restructure columns.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findSQLiteDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := findTable(dbPath, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}
	sessionCols := tableColumns(dbPath, sessionTable)

	idCol := firstMatch(sessionCols, "id", "session_id", "sessionID")
	if idCol == "" {
		return nil, nil
	}

	timeCol := firstMatch(sessionCols, "time_updated", "updated_at", "updated", "time_created", "created_at", "mtime", "time")
	orderClause := " ORDER BY rowid DESC"
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}

	var whereClause string
	if projectCol := firstMatch(sessionCols, "project_id", "projectID", "project"); projectCol != "" {
		whereClause = fmt.Sprintf(" WHERE %s='%s'", projectCol, escapeSQLite(projectID))
	} else if dirCol := firstMatch(sessionCols, "directory", "cwd", "path", "project_dir"); dirCol != "" {
		whereClause = fmt.Sprintf(" WHERE %s='%s'", dirCol, escapeSQLite(projectPath))
	}

	sessionID := runSQLiteScalar(dbPath, fmt.Sprintf("SELECT %s FROM %s%s%s LIMIT 1;", idCol, sessionTable, whereClause, orderClause))
	if sessionID == "" && whereClause != "" {
		// The project column's value scheme may not match how we computed
		// projectID/projectPath — fall back to the most recent session
		// overall rather than reporting nothing found.
		sessionID = runSQLiteScalar(dbPath, fmt.Sprintf("SELECT %s FROM %s%s LIMIT 1;", idCol, sessionTable, orderClause))
	}
	if sessionID == "" {
		return nil, nil
	}

	if timeCol != "" {
		timeStr := runSQLiteScalar(dbPath, fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s';", timeCol, sessionTable, idCol, escapeSQLite(sessionID)))
		if t, ok := parseFlexibleTime(timeStr); ok && time.Since(t) > agent.RecentSessionTimeout {
			return nil, nil
		}
		// If we can't parse the time, proceed anyway — better to try than skip.
	}

	transcriptData := fetchSQLiteMessages(dbPath, sessionID)
	if len(transcriptData) == 0 {
		transcriptData = []byte("[]")
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// fetchSQLiteMessages retrieves all messages for a session as a JSON array,
// adapting to whichever message table/column names the OpenCode release
// uses. Returns nil if no usable message table/columns are found.
func fetchSQLiteMessages(dbPath, sessionID string) []byte {
	messageTable := findTable(dbPath, "message", "messages")
	if messageTable == "" {
		return nil
	}
	msgCols := tableColumns(dbPath, messageTable)

	sessionRefCol := firstMatch(msgCols, "session_id", "sessionID", "session")
	dataCol := firstMatch(msgCols, "data", "content", "body", "message")
	if sessionRefCol == "" || dataCol == "" {
		return nil
	}

	expr := dataCol
	if idCol := firstMatch(msgCols, "id"); idCol != "" && idCol != dataCol {
		expr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, idCol)
	}

	query := fmt.Sprintf("SELECT json_group_array(%s) FROM %s WHERE %s='%s'",
		expr, messageTable, sessionRefCol, escapeSQLite(sessionID))
	if orderCol := firstMatch(msgCols, "time_created", "created_at", "created", "time"); orderCol != "" {
		query += fmt.Sprintf(" ORDER BY %s", orderCol)
	} else {
		query += " ORDER BY rowid"
	}
	query += ";"

	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	data := strings.TrimSpace(string(out))
	if data == "" || data == "[null]" || data == "[]" {
		return nil
	}
	return []byte(data)
}

// findSQLiteDB returns the path to OpenCode's SQLite database, trying the
// filenames used across OpenCode releases.
func findSQLiteDB(dataDir string) string {
	for _, name := range []string{"opencode.db", "opencode.sqlite", "opencode.sqlite3", "storage.db", "db.sqlite"} {
		path := filepath.Join(dataDir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// findTable returns the first candidate table name that exists in the
// database, or "" if none of the candidates are present.
func findTable(dbPath string, candidates ...string) string {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	existing := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		existing[strings.TrimSpace(line)] = true
	}
	for _, c := range candidates {
		if existing[c] {
			return c
		}
	}
	return ""
}

// tableColumns returns the column names of a SQLite table via PRAGMA
// table_info, or nil if the table doesn't exist or can't be introspected.
func tableColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", "-separator", "|", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// firstMatch returns the first candidate present in cols, or "" if none match.
func firstMatch(cols []string, candidates ...string) string {
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

// escapeSQLite escapes single quotes for embedding in a SQLite string literal.
func escapeSQLite(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// runSQLiteScalar runs a query expected to return a single scalar value,
// returning "" on any error or empty result.
func runSQLiteScalar(dbPath, query string) string {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// parseFlexibleTime tries several timestamp formats/encodings used across
// OpenCode releases (RFC3339 variants, plain SQL datetime, and Unix epoch
// seconds or milliseconds).
func parseFlexibleTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, true
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		if n > 1_000_000_000_000 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
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
