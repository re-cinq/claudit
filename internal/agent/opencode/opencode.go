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
// OpenCode's on-disk storage layout has changed across releases (flat JSON
// files under the global XDG data dir, a global SQLite database, and a
// project-local database have all been observed at different versions), so
// this tries several plausible locations rather than assuming a single fixed
// layout. The SQLite fallback also introspects the actual schema instead of
// hardcoding table/column names, so a version bump that renames a column
// doesn't silently break discovery.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (pre-v1.2 OpenCode, or a project-local layout).
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite (OpenCode v1.2+).
	dataDir, err := GetDataDir()
	if err != nil {
		dataDir = ""
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath), nil
}

// discoverFromFlatFiles tries legacy/alternate flat file session discovery,
// checking both the global OpenCode data directory and a project-local
// ".opencode" directory (the same directory OpenCode already reads its
// plugin config from).
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	for _, c := range flatSessionCandidates(projectPath) {
		if session := scanSessionDir(c.sessionDir, c.storageRoot, projectPath); session != nil {
			return session, nil
		}
	}
	return nil, nil
}

// flatSessionCandidate pairs a session directory to scan with the storage
// root it belongs to, so the corresponding message directory can be derived.
type flatSessionCandidate struct {
	sessionDir  string
	storageRoot string
}

// flatSessionCandidates returns plausible flat-file session directories,
// most specific (project-local) first.
func flatSessionCandidates(projectPath string) []flatSessionCandidate {
	projectID := GetProjectID(projectPath)
	var candidates []flatSessionCandidate

	if dataDir, err := GetDataDir(); err == nil {
		storageRoot := filepath.Join(dataDir, "storage")
		candidates = append(candidates, flatSessionCandidate{
			sessionDir:  filepath.Join(storageRoot, "session", projectID),
			storageRoot: storageRoot,
		})
	}

	localStorageRoot := filepath.Join(projectPath, ".opencode", "storage")
	candidates = append(candidates,
		flatSessionCandidate{
			sessionDir:  filepath.Join(localStorageRoot, "session", projectID),
			storageRoot: localStorageRoot,
		},
		flatSessionCandidate{
			sessionDir:  filepath.Join(localStorageRoot, "session"),
			storageRoot: localStorageRoot,
		},
	)

	return candidates
}

// scanSessionDir scans a directory for the most recently modified session
// file within the recent-session timeout, returning nil if none qualify.
func scanSessionDir(sessionDir, storageRoot, projectPath string) *agent.SessionInfo {
	dirEntries, err := os.ReadDir(sessionDir)
	if err != nil {
		return nil
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

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = strings.TrimSuffix(entry.Name(), ".json")
			bestModTime = modTime
		}
	}

	if bestSessionID == "" {
		return nil
	}

	msgDir := filepath.Join(storageRoot, "message", bestSessionID)

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// discoverFromSQLite queries an OpenCode SQLite database for the most recent
// session, trying several plausible database locations.
func discoverFromSQLite(dataDir, projectID, projectPath string) *agent.SessionInfo {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil
	}

	for _, dbPath := range sqliteDBCandidates(dataDir, projectPath) {
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		if session := discoverFromSQLiteDB(dbPath, projectID, projectPath); session != nil {
			return session
		}
	}
	return nil
}

// sqliteDBCandidates returns plausible OpenCode SQLite database locations,
// most specific (project-local) first.
func sqliteDBCandidates(dataDir, projectPath string) []string {
	candidates := []string{
		filepath.Join(projectPath, ".opencode", "opencode.db"),
		filepath.Join(projectPath, ".opencode", "storage", "opencode.db"),
	}
	if dataDir != "" {
		candidates = append(candidates,
			filepath.Join(dataDir, "opencode.db"),
			filepath.Join(dataDir, "storage", "opencode.db"),
			filepath.Join(dataDir, "db", "opencode.db"),
		)
	}
	return candidates
}

// discoverFromSQLiteDB inspects a single database file for a session
// belonging to the given project, introspecting the schema rather than
// assuming fixed table/column names. Returns nil if the schema doesn't look
// like an OpenCode session store or no matching session is found.
func discoverFromSQLiteDB(dbPath, projectID, projectPath string) *agent.SessionInfo {
	tables, err := sqliteTableNames(dbPath)
	if err != nil {
		return nil
	}
	sessionTable := matchName(tables, "session", "sessions")
	messageTable := matchName(tables, "message", "messages")
	if sessionTable == "" || messageTable == "" {
		return nil
	}

	sessionCols, err := sqliteColumnNames(dbPath, sessionTable)
	if err != nil {
		return nil
	}
	idCol := matchName(sessionCols, "id")
	if idCol == "" {
		return nil
	}
	scopeCol := matchName(sessionCols, "project_id", "projectid", "directory", "cwd", "path", "worktree", "project")
	timeCol := matchName(sessionCols, "time_updated", "updated_at", "updatedat", "time_updated_at", "modified_at", "time_created", "created_at", "createdat")

	sessionID := findSessionID(dbPath, sessionTable, idCol, scopeCol, timeCol, projectID, projectPath)
	if sessionID == "" {
		return nil
	}
	if !isSessionRecent(dbPath, sessionTable, idCol, timeCol, sessionID) {
		return nil
	}

	messageCols, err := sqliteColumnNames(dbPath, messageTable)
	if err != nil {
		return nil
	}
	msgSessionCol := matchName(messageCols, "session_id", "sessionid", "session")
	if msgSessionCol == "" {
		return nil
	}
	msgTimeCol := matchName(messageCols, "time_created", "created_at", "createdat", "time", "time_updated", "updated_at")

	transcriptData := fetchMessagesJSON(dbPath, messageTable, messageCols, msgSessionCol, msgTimeCol, sessionID)
	if transcriptData == nil {
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

// findSessionID looks up the most recent session ID, first scoped to the
// current project (if a scoping column was found), then falling back to the
// most recent session in the database overall. The fallback matters for
// project-local databases, where every row already belongs to this project
// and there may be no explicit scoping column at all.
func findSessionID(dbPath, table, idCol, scopeCol, timeCol, projectID, projectPath string) string {
	if scopeCol != "" {
		scopeValue := projectID
		lowered := strings.ToLower(scopeCol)
		if strings.Contains(lowered, "dir") || strings.Contains(lowered, "path") ||
			strings.Contains(lowered, "cwd") || strings.Contains(lowered, "worktree") {
			scopeValue = projectPath
		}

		query := fmt.Sprintf("SELECT %s FROM %s WHERE %s=%s%s LIMIT 1;",
			quoteIdent(idCol), quoteIdent(table), quoteIdent(scopeCol), sqlQuote(scopeValue), orderByClause(timeCol))
		if out, err := sqliteQuery(dbPath, query); err == nil {
			if id := strings.TrimSpace(out); id != "" {
				return id
			}
		}
	}

	query := fmt.Sprintf("SELECT %s FROM %s%s LIMIT 1;", quoteIdent(idCol), quoteIdent(table), orderByClause(timeCol))
	out, err := sqliteQuery(dbPath, query)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// orderByClause returns an " ORDER BY <col> DESC" clause, or "" if col is empty.
func orderByClause(col string) string {
	if col == "" {
		return ""
	}
	return fmt.Sprintf(" ORDER BY %s DESC", quoteIdent(col))
}

// isSessionRecent checks whether a session's timestamp is within the
// recent-session timeout. Timestamps across OpenCode versions have been
// seen as ISO-8601 strings and as integer epoch values at varying
// resolutions, so several formats are tried. If the timestamp can't be
// parsed at all, the session is treated as recent rather than discarded.
func isSessionRecent(dbPath, table, idCol, timeCol, sessionID string) bool {
	if timeCol == "" {
		return true
	}

	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s=%s LIMIT 1;",
		quoteIdent(timeCol), quoteIdent(table), quoteIdent(idCol), sqlQuote(sessionID))
	out, err := sqliteQuery(dbPath, query)
	if err != nil {
		return true
	}

	raw := strings.TrimSpace(out)
	if raw == "" {
		return true
	}

	if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Since(epochToTime(v)) <= agent.RecentSessionTimeout
	}

	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	return true
}

// epochToTime converts an integer timestamp of unknown resolution (seconds,
// milliseconds, microseconds, or nanoseconds) into a time.Time by inspecting
// its magnitude.
func epochToTime(v int64) time.Time {
	switch {
	case v > 1e17:
		return time.Unix(0, v)
	case v > 1e14:
		return time.Unix(0, v*int64(time.Microsecond))
	case v > 1e11:
		return time.Unix(0, v*int64(time.Millisecond))
	default:
		return time.Unix(v, 0)
	}
}

// fetchMessagesJSON returns all messages for a session as a JSON array,
// built dynamically from whatever columns the message table actually has.
// If one of the columns looks like it holds a nested JSON object for the
// whole message (an older OpenCode schema stored the message under a single
// "data" column), it is flattened together with the other columns so
// downstream parsing sees a normal flat message object.
func fetchMessagesJSON(dbPath, table string, cols []string, sessionCol, timeCol, sessionID string) []byte {
	if len(cols) == 0 {
		return nil
	}

	rowExpr := buildMessageRowExpr(dbPath, table, cols)

	query := fmt.Sprintf("SELECT json_group_array(%s) FROM %s WHERE %s=%s%s;",
		rowExpr, quoteIdent(table), quoteIdent(sessionCol), sqlQuote(sessionID), orderByColumnClause(timeCol))

	out, err := sqliteQuery(dbPath, query)
	if err != nil {
		return nil
	}

	data := strings.TrimSpace(out)
	if data == "" || data == "[null]" || data == "[]" {
		return nil
	}
	return []byte(data)
}

// orderByColumnClause returns " ORDER BY <col>" (ascending), or "" if col is empty.
func orderByColumnClause(col string) string {
	if col == "" {
		return ""
	}
	return fmt.Sprintf(" ORDER BY %s", quoteIdent(col))
}

// buildMessageRowExpr constructs a SQL expression that turns each message
// row into a single JSON object.
func buildMessageRowExpr(dbPath, table string, cols []string) string {
	blobCol := matchName(cols, "data", "parts")
	if blobCol != "" && columnLooksLikeJSONObject(dbPath, table, blobCol) {
		scalarPairs := make([]string, 0, len(cols))
		for _, c := range cols {
			if c == blobCol {
				continue
			}
			scalarPairs = append(scalarPairs, sqlQuote(c)+", "+quoteIdent(c))
		}
		return fmt.Sprintf("json_patch(%s, json_object(%s))", quoteIdent(blobCol), strings.Join(scalarPairs, ", "))
	}

	pairs := make([]string, 0, len(cols))
	for _, c := range cols {
		pairs = append(pairs, sqlQuote(c)+", "+quoteIdent(c))
	}
	return fmt.Sprintf("json_object(%s)", strings.Join(pairs, ", "))
}

// columnLooksLikeJSONObject checks whether a column's values look like a
// JSON object by sampling a single row.
func columnLooksLikeJSONObject(dbPath, table, col string) bool {
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL LIMIT 1;", quoteIdent(col), quoteIdent(table), quoteIdent(col))
	out, err := sqliteQuery(dbPath, query)
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(out), "{")
}

// sqliteQuery runs a query against dbPath and returns raw stdout.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// sqliteTableNames lists table names in the database.
func sqliteTableNames(dbPath string) ([]string, error) {
	out, err := sqliteQuery(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return nil, err
	}
	return splitNonEmptyLines(out), nil
}

// sqliteColumnNames lists column names for a table via PRAGMA table_info.
func sqliteColumnNames(dbPath, table string) ([]string, error) {
	out, err := sqliteQuery(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(table)))
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, line := range splitNonEmptyLines(out) {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

func splitNonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// matchName finds the first entry matching one of the candidate names
// (case-insensitive), preferring earlier candidates.
func matchName(names []string, candidates ...string) string {
	for _, want := range candidates {
		for _, n := range names {
			if strings.EqualFold(n, want) {
				return n
			}
		}
	}
	return ""
}

// quoteIdent quotes a SQL identifier (table/column name) for safe interpolation.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqlQuote quotes a SQL string literal for safe interpolation.
func sqlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
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

	// Try "parts" as an array of typed content wrappers, used by some
	// OpenCode versions: [{"type":"text","data":{"text":"..."}}, ...].
	// The "parts" value may be a JSON array directly, or a JSON-encoded
	// string (when read back verbatim from a SQLite text column).
	if partsRaw, ok := raw["parts"]; ok {
		if blocks := parseOpenCodeParts(partsRaw); len(blocks) > 0 {
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

// parseOpenCodeParts parses a "parts" field into content blocks, handling
// both a direct JSON array and a JSON-encoded string containing one.
func parseOpenCodeParts(partsRaw json.RawMessage) []agent.ContentBlock {
	data := []byte(partsRaw)
	var asString string
	if err := json.Unmarshal(partsRaw, &asString); err == nil && asString != "" {
		data = []byte(asString)
	}

	var parts []struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &parts); err != nil {
		return nil
	}

	var blocks []agent.ContentBlock
	for _, p := range parts {
		if p.Type != "text" && p.Type != "reasoning" {
			continue
		}
		var textData struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(p.Data, &textData); err == nil && textData.Text != "" {
			blocks = append(blocks, agent.ContentBlock{Type: "text", Text: textData.Text})
		}
	}
	return blocks
}
