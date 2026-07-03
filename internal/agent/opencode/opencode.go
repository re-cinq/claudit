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
//
// OpenCode's on-disk storage layout has changed shape across releases
// (project-nested flat JSON files, a flat file layout with no project
// nesting, and a SQLite database whose table/column names have also
// changed). Rather than assuming one fixed layout, discovery tries
// progressively looser matches and, for SQLite, introspects the schema
// instead of hardcoding table/column names.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	if session := a.discoverFromFlatFiles(projectPath); session != nil {
		return session, nil
	}

	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath), nil
}

// discoverFromFlatFiles looks for session JSON files on disk. It tries, in
// order: sessions nested under the computed project ID, a flat layout with
// no project nesting at all, and finally scanning every project
// subdirectory for the most recently touched session (in case the
// project-ID scheme itself no longer matches what we compute).
func (a *Agent) discoverFromFlatFiles(projectPath string) *agent.SessionInfo {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return nil
	}
	sessionRoot := filepath.Dir(sessionDir) // dataDir/storage/session

	// 1) Sessions nested under the computed project ID (the "normal" case).
	if session := sessionFromDir(sessionDir, projectPath); session != nil {
		return session
	}

	// 2) Flat layout: session files directly under storage/session/, with
	// no per-project nesting.
	if session := sessionFromDir(sessionRoot, projectPath); session != nil {
		return session
	}

	// 3) Unknown/changed project-ID scheme: scan every project subdirectory,
	// preferring one whose recorded directory/cwd/path matches projectPath,
	// and otherwise falling back to the most recently touched session.
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		return nil
	}

	var best *agent.SessionInfo
	var bestModTime time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(sessionRoot, entry.Name())
		session := sessionFromDir(dir, projectPath)
		if session == nil {
			continue
		}
		if sessionMatchesProject(dir, session.SessionID, projectPath) {
			return session
		}
		if modTime, err := time.Parse(time.RFC3339, session.StartedAt); err == nil {
			if best == nil || modTime.After(bestModTime) {
				best = session
				bestModTime = modTime
			}
		}
	}

	return best
}

// sessionFromDir returns the most recently modified session file in dir
// (within the recency window), with TranscriptPath pointed at the message
// directory rather than the session file itself.
func sessionFromDir(dir, projectPath string) *agent.SessionInfo {
	session, err := agent.ScanDirForRecentSession(dir, ".json", nil, projectPath)
	if err != nil || session == nil {
		return nil
	}
	if msgDir, err := GetMessageDir(session.SessionID); err == nil {
		session.TranscriptPath = msgDir
	}
	return session
}

// sessionMatchesProject reports whether the session file for sessionID in
// dir records a directory/cwd/path matching projectPath. Used as a
// tie-breaker when scanning across all projects, since the project-ID
// scheme is not assumed to be stable across OpenCode versions.
func sessionMatchesProject(dir, sessionID, projectPath string) bool {
	data, err := os.ReadFile(filepath.Join(dir, sessionID+".json"))
	if err != nil {
		return false
	}
	var fields struct {
		Directory   string `json:"directory"`
		Cwd         string `json:"cwd"`
		Path        string `json:"path"`
		ProjectPath string `json:"projectPath"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return false
	}
	for _, candidate := range []string{fields.Directory, fields.Cwd, fields.Path, fields.ProjectPath} {
		if candidate != "" && candidate == projectPath {
			return true
		}
	}
	return false
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session. Table and column names are introspected via
// sqlite_master/PRAGMA table_info rather than hardcoded, since OpenCode has
// renamed both across releases (e.g. "session"/"sessions", "project_id" vs
// a "directory"/"cwd" column).
func discoverFromSQLite(dataDir, projectID, projectPath string) *agent.SessionInfo {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil
	}

	sessionTable := detectSQLiteTable(dbPath, []string{"session", "sessions"})
	messageTable := detectSQLiteTable(dbPath, []string{"message", "messages"})
	if sessionTable == "" || messageTable == "" {
		return nil
	}

	sessionCols := sqliteColumns(dbPath, sessionTable)
	idCol := firstExistingColumn(sessionCols, []string{"id"})
	if idCol == "" {
		return nil
	}
	dirCol := firstExistingColumn(sessionCols, []string{"directory", "cwd", "path", "worktree"})
	projectCol := firstExistingColumn(sessionCols, []string{"project_id", "projectid", "project"})
	timeCol := firstExistingColumn(sessionCols, []string{"time_updated", "updated_at", "updatedat", "timeupdated"})

	orderBy := "rowid"
	if timeCol != "" {
		orderBy = timeCol
	}

	// Find most recent session for this project. Prefer an exact match on
	// the project's working directory when the schema exposes one, since
	// that doesn't depend on our own project-ID hashing scheme matching
	// OpenCode's.
	var sessionID string
	if dirCol != "" {
		sessionID = querySingleValue(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
			idCol, sessionTable, dirCol, escapeSQLiteString(projectPath), orderBy))
	}
	if sessionID == "" && projectCol != "" {
		sessionID = querySingleValue(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
			idCol, sessionTable, projectCol, escapeSQLiteString(projectID), orderBy))
	}
	if sessionID == "" {
		// Unknown/renamed project column: fall back to the most recent
		// session system-wide. The recency check below guards against
		// attaching an unrelated, stale session.
		sessionID = querySingleValue(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s ORDER BY %s DESC LIMIT 1;`, idCol, sessionTable, orderBy))
	}
	if sessionID == "" {
		return nil
	}

	// Check if this session was recent (within timeout)
	if timeCol != "" {
		timeStr := querySingleValue(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s';`, timeCol, sessionTable, idCol, escapeSQLiteString(sessionID)))
		if !isRecentSQLiteTimestamp(timeStr) {
			return nil
		}
	}

	// Get messages for this session as a JSON array
	transcriptData := queryOpenCodeMessages(dbPath, messageTable, sessionID)
	if len(transcriptData) == 0 {
		return nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}
}

// detectSQLiteTable returns the first candidate table name (case
// insensitive) that actually exists in the database, or "" if none do.
func detectSQLiteTable(dbPath string, candidates []string) string {
	names := querySingleColumn(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	existing := make(map[string]bool, len(names))
	for _, name := range names {
		existing[strings.ToLower(name)] = true
	}
	for _, cand := range candidates {
		if existing[cand] {
			return cand
		}
	}
	return ""
}

// sqliteColumns returns the column names of table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) > 1 {
			cols = append(cols, parts[1])
		}
	}
	return cols
}

// firstExistingColumn returns the first candidate present in cols (case
// insensitive), or "" if none are present.
func firstExistingColumn(cols []string, candidates []string) string {
	set := make(map[string]bool, len(cols))
	for _, c := range cols {
		set[strings.ToLower(c)] = true
	}
	for _, cand := range candidates {
		if set[cand] {
			return cand
		}
	}
	return ""
}

// querySingleValue runs query and returns the trimmed output, or "" on any
// error.
func querySingleValue(dbPath, query string) string {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// querySingleColumn runs query and returns each output line as an element.
func querySingleColumn(dbPath, query string) []string {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// escapeSQLiteString escapes single quotes for use in a SQL string literal.
func escapeSQLiteString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// isRecentSQLiteTimestamp reports whether timeStr, in any of the timestamp
// formats OpenCode has used (RFC3339-ish strings or unix epoch
// seconds/milliseconds), falls within RecentSessionTimeout. If the format
// can't be recognized it returns true so discovery still proceeds — better
// to try than to skip.
func isRecentSQLiteTimestamp(timeStr string) bool {
	timeStr = strings.TrimSpace(timeStr)
	if timeStr == "" {
		return true
	}

	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, format := range formats {
		if t, err := time.Parse(format, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		t := time.Unix(n, 0)
		if n > 1e12 { // looks like milliseconds, not seconds
			t = time.UnixMilli(n)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	return true
}

// queryOpenCodeMessages returns the messages for sessionID from
// messageTable as a JSON array, or nil if the schema doesn't match or no
// rows are found.
func queryOpenCodeMessages(dbPath, messageTable, sessionID string) []byte {
	cols := sqliteColumns(dbPath, messageTable)
	idCol := firstExistingColumn(cols, []string{"id"})
	sessionCol := firstExistingColumn(cols, []string{"session_id", "sessionid"})
	dataCol := firstExistingColumn(cols, []string{"data", "content", "parts"})
	if idCol == "" || sessionCol == "" || dataCol == "" {
		return nil
	}
	timeCol := firstExistingColumn(cols, []string{"time_created", "created_at", "timecreated", "createdat"})

	orderBy := "rowid"
	if timeCol != "" {
		orderBy = timeCol
	}

	query := fmt.Sprintf(
		`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s' ORDER BY %s;`,
		dataCol, idCol, messageTable, sessionCol, escapeSQLiteString(sessionID), orderBy,
	)
	out := querySingleValue(dbPath, query)
	if out == "" || out == "[null]" || out == "[]" {
		return nil
	}
	return []byte(out)
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
