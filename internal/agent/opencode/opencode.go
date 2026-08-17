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
// It first tries flat file storage, then falls back to SQLite. OpenCode has
// changed its on-disk storage layout and SQLite schema across releases, so
// both discovery paths adapt to the fields/columns actually present rather
// than assuming one fixed shape.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first.
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite.
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// discoverFromFlatFiles tries flat-file session discovery. OpenCode has used
// more than one flat-file layout over time: sessions nested under a
// per-project directory keyed by a git-root-commit-hash "project ID"
// (storage/session/<projectID>/<sessionID>.json), and flatter layouts where
// all sessions live together and are scoped by a "projectID" or "directory"
// field inside each session file instead of by directory nesting
// (storage/session/info/<sessionID>.json or storage/session/<sessionID>.json).
// Each candidate is tried in turn; the first one that yields a recent,
// matching session wins.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)

	candidates := []struct {
		dir    string
		scoped bool // true if the directory itself already scopes to the project
	}{
		{filepath.Join(dataDir, "storage", "session", projectID), true},
		{filepath.Join(dataDir, "storage", "session", "info"), false},
		{filepath.Join(dataDir, "storage", "session"), false},
	}

	for _, c := range candidates {
		sessionID, modTime := findRecentSessionFile(c.dir, c.scoped, projectID, projectPath)
		if sessionID == "" {
			continue
		}

		msgDir := firstNonEmptyDir(candidateMessageDirs(dataDir, sessionID))
		if msgDir == "" {
			msgDir, _ = GetMessageDir(sessionID)
		}

		return &agent.SessionInfo{
			SessionID:      sessionID,
			TranscriptPath: msgDir,
			StartedAt:      modTime.Format(time.RFC3339),
			ProjectPath:    projectPath,
		}, nil
	}

	return nil, nil
}

// findRecentSessionFile scans dir for the most recently modified session
// JSON file within the recent-session timeout. When scoped is false, only
// files whose "projectID" or "directory" field matches projectID/projectPath
// are considered, since the directory itself doesn't scope to a project in
// that layout.
func findRecentSessionFile(dir string, scoped bool, projectID, projectPath string) (string, time.Time) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}
	}

	now := time.Now()
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
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			continue
		}

		if !scoped {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				continue
			}
			var s sessionInfo
			if err := json.Unmarshal(data, &s); err != nil {
				continue
			}
			if s.ProjectID != projectID && s.Directory != projectPath {
				continue
			}
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = strings.TrimSuffix(entry.Name(), ".json")
			bestModTime = modTime
		}
	}

	return bestSessionID, bestModTime
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session. Column names and the way a session is scoped to a project
// have changed across OpenCode releases (e.g. a git-root-hash "project_id"
// column vs. an absolute-path "directory" column, snake_case vs. camelCase),
// so the schema is introspected at query time rather than assumed.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := sqliteFindTable(dbPath, "session")
	if sessionTable == "" {
		return nil, nil
	}
	sessionCols := sqliteTableColumns(dbPath, sessionTable)

	idCol := findColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := findColumn(sessionCols,
		"time_updated", "timeUpdated", "updated_at", "updated",
		"time_created", "timeCreated", "created_at", "created")

	// Scope candidates: (column, value) pairs to try, in priority order, since
	// the column that scopes a session to a project has changed across
	// OpenCode versions.
	type scopeCandidate struct{ col, val string }
	var scopes []scopeCandidate
	for _, col := range []string{"directory", "project_dir", "projectDir", "cwd", "path"} {
		if found := findColumn(sessionCols, col); found != "" {
			scopes = append(scopes, scopeCandidate{found, projectPath})
		}
	}
	for _, col := range []string{"project_id", "projectID", "projectId"} {
		if found := findColumn(sessionCols, col); found != "" {
			scopes = append(scopes, scopeCandidate{found, projectID})
		}
	}

	var sessionID string
	for _, sc := range scopes {
		query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s'`, idCol, sessionTable, sc.col, sqliteEscape(sc.val))
		if timeCol != "" {
			query += fmt.Sprintf(` ORDER BY %s DESC`, timeCol)
		}
		query += ` LIMIT 1;`

		if out, err := sqliteQuery(dbPath, query); err == nil && out != "" {
			sessionID = out
			break
		}
	}

	// No scoped column matched (or no rows for it): fall back to the most
	// recent session overall. The recency check below still guards against
	// picking up an unrelated stale session.
	if sessionID == "" && timeCol != "" {
		query := fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s DESC LIMIT 1;`, idCol, sessionTable, timeCol)
		if out, err := sqliteQuery(dbPath, query); err == nil && out != "" {
			sessionID = out
		}
	}

	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout).
	if timeCol != "" {
		query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, timeCol, sessionTable, idCol, sqliteEscape(sessionID))
		if timeOutput, err := sqliteQuery(dbPath, query); err == nil && timeOutput != "" {
			if !isRecentTimestamp(timeOutput) {
				return nil, nil
			}
		}
		// If we can't read/parse the time, proceed anyway — better to try than skip.
	}

	transcriptData := fetchSQLiteMessages(dbPath, sessionID)
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

// fetchSQLiteMessages returns the messages for sessionID as a JSON array,
// adapting to schema differences across OpenCode versions: either a single
// JSON blob column per message row, or a normalized layout where message
// metadata (role/type, timestamp) and content ("parts") live in separate
// tables joined by message ID.
func fetchSQLiteMessages(dbPath, sessionID string) []byte {
	messageTable := sqliteFindTable(dbPath, "message")
	if messageTable == "" {
		return nil
	}
	msgCols := sqliteTableColumns(dbPath, messageTable)

	sessionCol := findColumn(msgCols, "session_id", "sessionID", "sessionId", "session")
	idCol := findColumn(msgCols, "id")
	timeCol := findColumn(msgCols, "time_created", "timeCreated", "created_at", "created", "time")
	if sessionCol == "" {
		return nil
	}

	// Preferred layout: a single JSON blob column holding the full message.
	if dataCol := findColumn(msgCols, "data", "content", "body"); dataCol != "" {
		patchExpr := dataCol
		if idCol != "" {
			patchExpr = fmt.Sprintf(`json_patch(%s, json_object('id', %s))`, dataCol, idCol)
		}
		query := fmt.Sprintf(`SELECT json_group_array(%s) FROM %s WHERE %s='%s'`,
			patchExpr, messageTable, sessionCol, sqliteEscape(sessionID))
		if timeCol != "" {
			query += fmt.Sprintf(` ORDER BY %s`, timeCol)
		}
		query += `;`

		if out, err := sqliteQuery(dbPath, query); err == nil {
			if out != "" && out != "[null]" && out != "[]" {
				return []byte(out)
			}
		}
	}

	// Normalized layout: message metadata only, no blob column — content (if
	// any) lives in a separate "part" table.
	roleCol := findColumn(msgCols, "role", "type")
	if roleCol == "" || idCol == "" {
		return nil
	}

	selectCols := []string{idCol + " AS id", roleCol + " AS role"}
	if timeCol != "" {
		selectCols = append(selectCols, timeCol+" AS time_created")
	}
	msgQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s'`,
		strings.Join(selectCols, ", "), messageTable, sessionCol, sqliteEscape(sessionID))
	if timeCol != "" {
		msgQuery += fmt.Sprintf(` ORDER BY %s`, timeCol)
	}
	msgQuery += `;`

	rows := sqliteQueryJSON(dbPath, msgQuery)
	if len(rows) == 0 {
		return nil
	}

	// Best-effort: pull text content in from a separate "part" table, if any.
	textByMessage := map[string]string{}
	if partTable := sqliteFindTable(dbPath, "part"); partTable != "" {
		partCols := sqliteTableColumns(dbPath, partTable)
		partMsgCol := findColumn(partCols, "message_id", "messageID", "messageId")
		partTextCol := findColumn(partCols, "text", "content", "data")
		if partMsgCol != "" && partTextCol != "" {
			partQuery := fmt.Sprintf(
				`SELECT %s AS message_id, %s AS text FROM %s WHERE %s IN (SELECT %s FROM %s WHERE %s='%s');`,
				partMsgCol, partTextCol, partTable, partMsgCol, idCol, messageTable, sessionCol, sqliteEscape(sessionID))
			for _, pr := range sqliteQueryJSON(dbPath, partQuery) {
				mid := toSQLiteString(pr["message_id"])
				text := toSQLiteString(pr["text"])
				if mid == "" || text == "" {
					continue
				}
				if existing, ok := textByMessage[mid]; ok {
					textByMessage[mid] = existing + "\n" + text
				} else {
					textByMessage[mid] = text
				}
			}
		}
	}

	entries := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		id := toSQLiteString(row["id"])
		entry := map[string]interface{}{
			"id":   id,
			"role": toSQLiteString(row["role"]),
		}
		if tc, ok := row["time_created"]; ok && tc != nil {
			entry["time"] = map[string]interface{}{"created": toSQLiteString(tc)}
		}
		if text, ok := textByMessage[id]; ok {
			entry["content"] = text
		}
		entries = append(entries, entry)
	}

	out, err := json.Marshal(entries)
	if err != nil {
		return nil
	}
	return out
}

// sqliteQuery runs query against dbPath in sqlite3's default (pipe-delimited)
// output mode and returns its trimmed output. Suitable for single scalar
// values (e.g. a single id, a single timestamp).
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sqliteQueryJSON runs query with sqlite3's -json flag and returns the
// decoded rows, or nil on any failure. Preferred over sqliteQuery whenever
// values might contain the default "|" column delimiter or embedded
// newlines, since -json output is unambiguous to parse.
func sqliteQueryJSON(dbPath, query string) []map[string]interface{} {
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil
	}
	return rows
}

// toSQLiteString converts a value decoded from sqlite3's -json output (which
// may be a string, number, or nil depending on the column's type affinity)
// into a plain string.
func toSQLiteString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// sqliteEscape escapes single quotes for embedding a value in a SQL string literal.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// sqliteFindTable returns the first table in dbPath whose name matches
// (case-insensitively) or contains one of the given hints, preferring exact
// matches over substring matches.
func sqliteFindTable(dbPath string, hints ...string) string {
	out, err := sqliteQuery(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil || out == "" {
		return ""
	}
	tables := strings.Split(out, "\n")

	for _, hint := range hints {
		for _, t := range tables {
			if strings.EqualFold(t, hint) {
				return t
			}
		}
	}
	for _, hint := range hints {
		for _, t := range tables {
			if strings.Contains(strings.ToLower(t), strings.ToLower(hint)) {
				return t
			}
		}
	}
	return ""
}

// sqliteTableColumns returns the column names of a SQLite table, or nil if
// the table does not exist.
func sqliteTableColumns(dbPath, table string) []string {
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

// findColumn returns the first candidate present in cols (case-insensitive
// match), or "" if none are present.
func findColumn(cols []string, candidates ...string) string {
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.EqualFold(c, cand) {
				return c
			}
		}
	}
	return ""
}

// isRecentTimestamp reports whether timeStr, in any of the formats OpenCode
// has used for session timestamps (RFC3339 variants, space-separated
// datetime, or Unix epoch seconds/milliseconds), is within the
// recent-session timeout. Unparseable input is treated as recent, since it's
// better to try a session than to skip a possibly-valid one.
func isRecentTimestamp(timeStr string) bool {
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}
	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		var t time.Time
		if n > 1e12 { // milliseconds
			t = time.Unix(0, n*int64(time.Millisecond))
		} else {
			t = time.Unix(n, 0)
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
