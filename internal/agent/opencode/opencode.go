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
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}
	sessionsRoot := filepath.Join(dataDir, "storage", "session")

	projectID := GetProjectID(projectPath)
	best := findMostRecentSessionFile(filepath.Join(sessionsRoot, projectID))
	if best.sessionID == "" {
		// OpenCode's project identifier scheme has changed across versions
		// and may not match ours. Fall back to scanning every project
		// directory for the most recently touched session so discovery
		// still works even when the project ID heuristic is out of date.
		best = findMostRecentSessionFileRecursive(sessionsRoot)
	}

	if best.sessionID == "" {
		return nil, nil
	}

	// The transcript path for OpenCode is the message directory
	msgDir, _ := GetMessageDir(best.sessionID)

	return &agent.SessionInfo{
		SessionID:      best.sessionID,
		TranscriptPath: msgDir,
		StartedAt:      best.modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// sessionFileMatch identifies a candidate session file found on disk.
type sessionFileMatch struct {
	sessionID string
	modTime   time.Time
}

// findMostRecentSessionFile returns the most recently modified *.json
// session file directly inside dir, ignoring anything older than
// agent.RecentSessionTimeout. Returns a zero-value match if none qualify.
func findMostRecentSessionFile(dir string) sessionFileMatch {
	var best sessionFileMatch

	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return best
	}

	now := time.Now()
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

		if best.sessionID == "" || modTime.After(best.modTime) {
			best = sessionFileMatch{
				sessionID: strings.TrimSuffix(entry.Name(), ".json"),
				modTime:   modTime,
			}
		}
	}

	return best
}

// findMostRecentSessionFileRecursive scans every immediate subdirectory of
// root (one per project) for the most recently touched session file.
func findMostRecentSessionFileRecursive(root string) sessionFileMatch {
	var best sessionFileMatch

	projectDirs, err := os.ReadDir(root)
	if err != nil {
		return best
	}

	for _, pd := range projectDirs {
		if !pd.IsDir() {
			continue
		}

		candidate := findMostRecentSessionFile(filepath.Join(root, pd.Name()))
		if candidate.sessionID != "" && (best.sessionID == "" || candidate.modTime.After(best.modTime)) {
			best = candidate
		}
	}

	return best
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
// OpenCode's internal schema (table names, column names, and even whether
// message content lives in dedicated columns or a single JSON blob column)
// has changed across releases, so this introspects sqlite_master/PRAGMA
// table_info at runtime instead of hard-coding one specific layout.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findOpenCodeDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := findTable(dbPath, "session")
	messageTable := findTable(dbPath, "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols := tableColumns(dbPath, sessionTable)
	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projCol := pickColumn(sessionCols, "project_id", "projectid", "project")
	timeCol := pickColumn(sessionCols, "time_updated", "updated_at", "updatedat", "updated", "time_created", "created_at")

	sessionID := querySessionID(dbPath, sessionTable, idCol, projCol, timeCol, projectID)
	if sessionID == "" {
		return nil, nil
	}

	// Best-effort recency check; if we can't determine a timestamp, proceed anyway.
	if timeCol != "" {
		if t, ok := queryRecency(dbPath, sessionTable, idCol, timeCol, sessionID); ok && time.Since(t) > agent.RecentSessionTimeout {
			return nil, nil
		}
	}

	messageCols := tableColumns(dbPath, messageTable)
	sessCol := pickColumn(messageCols, "session_id", "sessionid", "session")
	if sessCol == "" {
		return nil, nil
	}
	orderCol := pickColumn(messageCols, "time_created", "created_at", "createdat", "time_updated", "updated_at")

	transcriptData := queryMessages(dbPath, messageTable, sessCol, orderCol, sessionID)
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

// findOpenCodeDB locates OpenCode's SQLite database under dataDir, trying a
// handful of known file names/locations before falling back to any *.db or
// *.sqlite file found directly inside dataDir.
func findOpenCodeDB(dataDir string) string {
	candidates := []string{
		filepath.Join(dataDir, "opencode.db"),
		filepath.Join(dataDir, "db", "opencode.db"),
		filepath.Join(dataDir, "opencode.sqlite"),
		filepath.Join(dataDir, "db.sqlite"),
		filepath.Join(dataDir, "state.db"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".sqlite") {
			return filepath.Join(dataDir, name)
		}
	}
	return ""
}

// findTable returns the name of the table in dbPath that best matches hint
// (e.g. "session" matches "session" or "sessions").
func findTable(dbPath, hint string) string {
	out, err := runSQLite(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return ""
	}

	var contains string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name := strings.TrimSpace(line)
		lower := strings.ToLower(name)
		if lower == hint || lower == hint+"s" {
			return name
		}
		if contains == "" && strings.Contains(lower, hint) {
			contains = name
		}
	}
	return contains
}

// tableColumns returns the column names of table in dbPath.
func tableColumns(dbPath, table string) []string {
	out, err := runSQLite(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) >= 2 {
			cols = append(cols, parts[1])
		}
	}
	return cols
}

// pickColumn returns the first entry in cols matching one of candidates,
// case-insensitively (exact match preferred, substring match as fallback).
func pickColumn(cols []string, candidates ...string) string {
	lowerToActual := make(map[string]string, len(cols))
	for _, c := range cols {
		lowerToActual[strings.ToLower(c)] = c
	}

	for _, cand := range candidates {
		if actual, ok := lowerToActual[cand]; ok {
			return actual
		}
	}
	for _, cand := range candidates {
		for lower, actual := range lowerToActual {
			if strings.Contains(lower, cand) {
				return actual
			}
		}
	}
	return ""
}

// querySessionID finds the most recently active session, preferring one
// scoped to projectID (if we have a project column and it matches any rows)
// and otherwise falling back to the most recent session across all projects
// — OpenCode's project identifier scheme has changed across versions and
// may not match our own computation of it.
func querySessionID(dbPath, table, idCol, projCol, timeCol, projectID string) string {
	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}

	if projCol != "" {
		query := fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;", idCol, table, projCol, escapeSQLLiteral(projectID), orderClause)
		if out, err := runSQLite(dbPath, query); err == nil {
			if id := strings.TrimSpace(out); id != "" {
				return id
			}
		}
	}

	query := fmt.Sprintf("SELECT %s FROM %s%s LIMIT 1;", idCol, table, orderClause)
	out, err := runSQLite(dbPath, query)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// queryRecency returns the parsed timestamp for sessionID's timeCol value.
func queryRecency(dbPath, table, idCol, timeCol, sessionID string) (time.Time, bool) {
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s';", timeCol, table, idCol, escapeSQLLiteral(sessionID))
	out, err := runSQLite(dbPath, query)
	if err != nil {
		return time.Time{}, false
	}

	timeStr := strings.TrimSpace(out)
	if timeStr == "" {
		return time.Time{}, false
	}

	// Timestamps may be stored as formatted strings or as a Unix epoch
	// (seconds or milliseconds), depending on OpenCode's version.
	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}

	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, timeStr); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// queryMessages fetches all messages for sessionID as a JSON array. It uses
// sqlite3's JSON output mode so it works regardless of the message table's
// actual column names — each row becomes a JSON object keyed by column name.
func queryMessages(dbPath, table, sessCol, orderCol, sessionID string) []byte {
	orderClause := ""
	if orderCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s", orderCol)
	}

	query := fmt.Sprintf("SELECT * FROM %s WHERE %s='%s'%s;", table, sessCol, escapeSQLLiteral(sessionID), orderClause)
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "[]" || trimmed == "[null]" {
		return nil
	}
	return []byte(trimmed)
}

// runSQLite runs a single query against dbPath and returns raw stdout.
func runSQLite(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// escapeSQLLiteral escapes single quotes for safe interpolation into a
// SQLite string literal.
func escapeSQLLiteral(s string) string {
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

// dataColumnCandidates lists column names OpenCode's SQLite storage might
// use to hold a message's JSON payload as a single blob column, as opposed
// to exposing role/content directly as row columns.
var dataColumnCandidates = []string{"data", "content_json", "payload", "json", "body"}

// unwrapRow normalizes a raw OpenCode row/message into the shape
// parseOpenCodeEntry expects. Flat-file storage and some SQLite schemas
// expose "role"/"type" directly on the object; other SQLite schemas keep
// the message JSON nested inside a single blob column instead.
func unwrapRow(raw map[string]json.RawMessage) map[string]json.RawMessage {
	if _, ok := raw["role"]; ok {
		return raw
	}
	if _, ok := raw["type"]; ok {
		return raw
	}

	for _, col := range dataColumnCandidates {
		blob, ok := raw[col]
		if !ok {
			continue
		}

		var inner map[string]json.RawMessage
		var asString string
		if err := json.Unmarshal(blob, &asString); err == nil {
			if json.Unmarshal([]byte(asString), &inner) != nil {
				continue
			}
		} else if json.Unmarshal(blob, &inner) != nil {
			continue
		}

		if _, ok := inner["role"]; !ok {
			if _, ok := inner["type"]; !ok {
				continue
			}
		}

		// Preserve outer id/time if the inner blob doesn't carry them.
		for _, key := range []string{"id", "time"} {
			if _, ok := inner[key]; !ok {
				if v, ok := raw[key]; ok {
					inner[key] = v
				}
			}
		}
		return inner
	}

	return raw
}

// parseOpenCodeEntry parses a single OpenCode message into a TranscriptEntry.
func parseOpenCodeEntry(raw map[string]json.RawMessage, fullData []byte) agent.TranscriptEntry {
	raw = unwrapRow(raw)

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
