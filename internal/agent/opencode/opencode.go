package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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
// It first looks for the project-scoped directory layout
// (storage/session/<projectID>/<sessionID>.json). If that doesn't exist or
// yields nothing, it falls back to a flat, non-project-nested layout
// (storage/session/**/<sessionID>.json) where each session file embeds its
// own "projectID"/"directory" field, since newer OpenCode releases have
// changed how sessions are laid out on disk.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	if session := discoverFromProjectScopedDir(projectPath); session != nil {
		return session, nil
	}
	return discoverFromFlatSessionIndex(projectPath)
}

// discoverFromProjectScopedDir looks for sessions under
// storage/session/<projectID>/*.json.
func discoverFromProjectScopedDir(projectPath string) *agent.SessionInfo {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return nil
	}

	dirEntries, err := os.ReadDir(sessionDir)
	if err != nil {
		return nil
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
		return nil
	}

	// The transcript path for OpenCode is the message directory
	msgDir, _ := GetMessageDir(bestSessionID)

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// discoverFromFlatSessionIndex handles a flat (non project-nested) session
// storage layout by walking storage/session/ and matching files whose
// embedded "projectID" or "directory" field identifies this project.
func discoverFromFlatSessionIndex(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	sessionRoot := filepath.Join(dataDir, "storage", "session")
	if _, err := os.Stat(sessionRoot); err != nil {
		return nil, nil
	}

	now := time.Now()
	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(sessionRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, ierr := d.Info()
		if ierr != nil || now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}

		var session struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectID"`
			Directory string `json:"directory"`
		}
		if jerr := json.Unmarshal(data, &session); jerr != nil {
			return nil
		}

		if session.ProjectID != projectID && session.Directory != projectPath {
			return nil
		}

		if bestSessionID == "" || info.ModTime().After(bestModTime) {
			id := session.ID
			if id == "" {
				id = strings.TrimSuffix(d.Name(), ".json")
			}
			bestSessionID = id
			bestModTime = info.ModTime()
		}
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	msgDir, _ := GetMessageDir(bestSessionID)

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
// It first tries the historically known schema (fast path), then falls back
// to introspecting the database's actual tables/columns at runtime. This
// keeps session discovery working across OpenCode releases that rename
// tables or columns (e.g. "session" -> "sessions", "time_updated" ->
// "updated_at") without us having to hardcode every version's schema.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := locateSQLiteDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	if info := trySQLiteFixedSchema(dbPath, projectID, projectPath); info != nil {
		return info, nil
	}

	return trySQLiteAdaptiveSchema(dbPath, projectID, projectPath)
}

// locateSQLiteDB finds OpenCode's SQLite database under the data directory.
// The database file name/location has moved across OpenCode releases, so we
// check the historically known name first and fall back to searching for
// any *.db file under the data directory.
func locateSQLiteDB(dataDir string) string {
	for _, name := range []string{"opencode.db", "state.db", "db.sqlite"} {
		candidate := filepath.Join(dataDir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" || d == nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".db") {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// trySQLiteFixedSchema queries the database using the historically known
// OpenCode schema: a "session" table (id, project_id, time_updated) and a
// "message" table (id, session_id, time_created, data).
func trySQLiteFixedSchema(dbPath, projectID, projectPath string) *agent.SessionInfo {
	sessionQuery := fmt.Sprintf(
		`SELECT id FROM session WHERE project_id='%s' ORDER BY time_updated DESC LIMIT 1;`,
		projectID,
	)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	timeQuery := fmt.Sprintf(`SELECT time_updated FROM session WHERE id='%s';`, sessionID)
	cmd = exec.Command("sqlite3", dbPath, timeQuery)
	if timeOutput, terr := cmd.Output(); terr == nil {
		if !isRecentTimestamp(strings.TrimSpace(string(timeOutput))) {
			return nil
		}
	}

	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE session_id='%s' ORDER BY time_created;`,
		sessionID,
	)
	cmd = exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	if len(transcriptData) == 0 || string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
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

// trySQLiteAdaptiveSchema introspects the database's actual table and column
// names and builds queries dynamically, as a fallback for when the fixed
// schema above no longer matches.
func trySQLiteAdaptiveSchema(dbPath, projectID, projectPath string) (*agent.SessionInfo, error) {
	sessionTable := pickTable(dbPath, "session")
	messageTable := pickTable(dbPath, "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols := sqliteColumns(dbPath, sessionTable)
	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := pickColumn(sessionCols, "time_updated", "updated_at", "modified_at", "time_modified", "updatedat", "updated", "time_created", "created_at", "time")
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project")
	dirCol := pickColumn(sessionCols, "directory", "worktree", "project_dir", "cwd", "path")

	order := ""
	if timeCol != "" {
		order = " ORDER BY " + timeCol + " DESC"
	}

	sessionID := ""
	if projectCol != "" {
		sessionID = queryScalar(dbPath, fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;", idCol, sessionTable, projectCol, projectID, order))
	}
	if sessionID == "" && dirCol != "" {
		sessionID = queryScalar(dbPath, fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;", idCol, sessionTable, dirCol, projectPath, order))
	}
	if sessionID == "" && projectCol == "" && dirCol == "" {
		// No column identifies the owning project/directory; fall back to the
		// most recently updated session in the database.
		sessionID = queryScalar(dbPath, fmt.Sprintf("SELECT %s FROM %s%s LIMIT 1;", idCol, sessionTable, order))
	}
	if sessionID == "" {
		return nil, nil
	}

	if timeCol != "" {
		ts := queryScalar(dbPath, fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s';", timeCol, sessionTable, idCol, sessionID))
		if !isRecentTimestamp(ts) {
			return nil, nil
		}
	}

	messageCols := sqliteColumns(dbPath, messageTable)
	if len(messageCols) == 0 {
		return nil, nil
	}
	msgIDCol := pickColumn(messageCols, "id")
	sessionLinkCol := pickColumn(messageCols, "session_id", "sessionid", "session")
	msgTimeCol := pickColumn(messageCols, "time_created", "created_at", "createdat", "time", "created")
	if sessionLinkCol == "" {
		return nil, nil
	}

	dataCol := findJSONColumn(dbPath, messageTable, messageCols, msgIDCol, sessionLinkCol)

	var msgQuery string
	if dataCol != "" && msgIDCol != "" {
		msgQuery = fmt.Sprintf(
			"SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s'",
			dataCol, msgIDCol, messageTable, sessionLinkCol, sessionID,
		)
	} else {
		assignments := make([]string, 0, len(messageCols))
		for _, c := range messageCols {
			assignments = append(assignments, fmt.Sprintf("'%s', %s", c, c))
		}
		msgQuery = fmt.Sprintf(
			"SELECT json_group_array(json_object(%s)) FROM %s WHERE %s='%s'",
			strings.Join(assignments, ", "), messageTable, sessionLinkCol, sessionID,
		)
	}
	if msgTimeCol != "" {
		msgQuery += " ORDER BY " + msgTimeCol
	}
	msgQuery += ";"

	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	if len(transcriptData) == 0 || string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "",
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// pickTable finds a table whose name matches (or contains) the given noun,
// preferring an exact singular/plural match over a loose substring match.
func pickTable(dbPath, noun string) string {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	var loose string
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if lower == noun || lower == noun+"s" {
			return name
		}
		if loose == "" && strings.Contains(lower, noun) {
			loose = name
		}
	}
	return loose
}

// sqliteColumns returns the column names of a table.
func sqliteColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 && fields[1] != "" {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// pickColumn returns the first column matching one of the candidate names
// (case-insensitive exact match first, then substring match).
func pickColumn(cols []string, candidates ...string) string {
	lower := make(map[string]string, len(cols))
	for _, c := range cols {
		lower[strings.ToLower(c)] = c
	}

	for _, cand := range candidates {
		if actual, ok := lower[cand]; ok {
			return actual
		}
	}
	for _, cand := range candidates {
		for lc, actual := range lower {
			if strings.Contains(lc, cand) {
				return actual
			}
		}
	}
	return ""
}

// findJSONColumn returns the name of a column (other than the excluded ones)
// that holds a full JSON document, used to reconstruct message content when
// the schema doesn't match the known "data" column name.
func findJSONColumn(dbPath, table string, cols []string, exclude ...string) string {
	excluded := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		if e != "" {
			excluded[e] = true
		}
	}

	preferred := []string{"data", "content", "body", "payload", "parts", "json"}
	var ordered []string
	for _, cand := range preferred {
		for _, c := range cols {
			if excluded[c] || strings.ToLower(c) != cand {
				continue
			}
			ordered = append(ordered, c)
		}
	}
	for _, c := range cols {
		if excluded[c] {
			continue
		}
		alreadyAdded := false
		for _, o := range ordered {
			if o == c {
				alreadyAdded = true
				break
			}
		}
		if !alreadyAdded {
			ordered = append(ordered, c)
		}
	}

	for _, c := range ordered {
		out := queryScalar(dbPath, fmt.Sprintf("SELECT json_valid(%s) FROM %s WHERE %s IS NOT NULL LIMIT 1;", c, table, c))
		if out == "1" {
			return c
		}
	}
	return ""
}

// queryScalar runs a SQL statement expected to return a single scalar value.
func queryScalar(dbPath, query string) string {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// isRecentTimestamp reports whether a timestamp string (RFC3339-ish, plain
// SQL datetime, or unix epoch seconds/milliseconds) is within the recent
// session window. Unrecognized/empty values are treated as recent so that
// discovery doesn't fail just because a timestamp format couldn't be parsed.
func isRecentTimestamp(raw string) bool {
	if raw == "" {
		return true
	}

	formats := []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
	for _, f := range formats {
		if t, err := time.Parse(f, raw); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		t := time.Unix(n, 0)
		if n > 1_000_000_000_000 { // milliseconds since epoch
			t = time.UnixMilli(n)
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

	return msg
}
