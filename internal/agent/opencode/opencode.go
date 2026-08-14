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
//
// OpenCode has changed how session files are laid out on disk across
// versions. We first try the layout we know about (sessions partitioned by
// our computed project ID); if that yields nothing, we fall back to scanning
// every project subdirectory under storage/session and matching sessions by
// their own embedded directory/cwd field, since newer OpenCode releases may
// key project directories differently than our git-root-hash based ID.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	projectID := GetProjectID(projectPath)

	// Fast path: sessions partitioned by our computed project ID (the
	// layout OpenCode has historically used).
	if id, modTime := pickRecentSessionFile(filepath.Join(sessionRoot, projectID), projectPath, false); id != "" {
		return a.flatFileSessionInfo(dataDir, id, modTime, projectPath), nil
	}

	// Fallback: scan every project subdirectory and match sessions whose
	// own content references this project's directory. This tolerates
	// OpenCode versions that partition sessions differently than the
	// git-root-commit-hash scheme we compute.
	rootEntries, err := os.ReadDir(sessionRoot)
	if err != nil {
		return nil, nil
	}

	var bestID string
	var bestMod time.Time
	for _, entry := range rootEntries {
		if !entry.IsDir() || entry.Name() == projectID {
			continue
		}
		id, modTime := pickRecentSessionFile(filepath.Join(sessionRoot, entry.Name()), projectPath, true)
		if id != "" && (bestID == "" || modTime.After(bestMod)) {
			bestID, bestMod = id, modTime
		}
	}

	if bestID == "" {
		return nil, nil
	}

	return a.flatFileSessionInfo(dataDir, bestID, bestMod, projectPath), nil
}

// flatFileSessionInfo builds a SessionInfo for a discovered flat-file session.
func (a *Agent) flatFileSessionInfo(dataDir, sessionID string, modTime time.Time, projectPath string) *agent.SessionInfo {
	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: discoverMessageDir(dataDir, sessionID),
		StartedAt:      modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// pickRecentSessionFile returns the session ID (filename minus the .json
// extension) and mod time of the most recently modified session file in dir
// that falls within the recent-session window. When verifyContent is true,
// only files whose JSON content references projectPath (via a
// directory/cwd/path/worktree field) qualify — used when scanning
// directories not already known to belong to this project.
func pickRecentSessionFile(dir, projectPath string, verifyContent bool) (string, time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}
	}

	absProjectPath, _ := filepath.Abs(projectPath)
	now := time.Now()

	var bestID string
	var bestMod time.Time
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

		if verifyContent {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil || !sessionMatchesProject(data, projectPath, absProjectPath) {
				continue
			}
		}

		if bestID == "" || modTime.After(bestMod) {
			bestID = strings.TrimSuffix(entry.Name(), ".json")
			bestMod = modTime
		}
	}

	return bestID, bestMod
}

// sessionMatchesProject reports whether a session's stored JSON references
// the given project directory, trying several field names OpenCode has used
// for the session's working directory across versions.
func sessionMatchesProject(data []byte, projectPath, absProjectPath string) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	for _, key := range []string{"directory", "cwd", "path", "worktree", "projectPath"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			continue
		}
		if s == projectPath || (absProjectPath != "" && s == absProjectPath) {
			return true
		}
	}
	return false
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
//
// OpenCode's SQLite schema has changed across versions (column/table names
// and how sessions reference their project). Rather than assuming fixed
// column names, this introspects the actual schema via PRAGMA table_info and
// tries several plausible project-matching and timestamp columns, so it
// keeps working as the schema evolves.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionCols, err := sqliteColumns(dbPath, "session")
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	orderCol := firstExistingColumn(sessionCols, "time_updated", "updated", "time_created", "created")
	orderClause := " ORDER BY rowid DESC"
	if orderCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", orderCol)
	}

	absProjectPath, _ := filepath.Abs(projectPath)

	type matchCandidate struct {
		column string
		value  string
	}
	var candidates []matchCandidate
	for _, col := range []string{"directory", "cwd", "path", "worktree"} {
		if sessionCols[col] {
			candidates = append(candidates, matchCandidate{col, projectPath})
			if absProjectPath != "" && absProjectPath != projectPath {
				candidates = append(candidates, matchCandidate{col, absProjectPath})
			}
		}
	}
	for _, col := range []string{"project_id", "projectID", "project"} {
		if sessionCols[col] {
			candidates = append(candidates, matchCandidate{col, projectID})
		}
	}

	var sessionID string
	for _, cand := range candidates {
		query := fmt.Sprintf(`SELECT id FROM session WHERE %s=%s%s LIMIT 1;`, cand.column, sqliteQuote(cand.value), orderClause)
		out, err := sqliteQuery(dbPath, query)
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(out); id != "" {
			sessionID = id
			break
		}
	}

	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout), using whichever
	// timestamp column is available.
	if orderCol != "" {
		timeOutput, err := sqliteQuery(dbPath, fmt.Sprintf(`SELECT %s FROM session WHERE id=%s;`, orderCol, sqliteQuote(sessionID)))
		if err == nil {
			if recent, ok := isRecentTimestamp(strings.TrimSpace(timeOutput)); ok && !recent {
				return nil, nil
			}
			// If we can't parse the time, proceed anyway — better to try than skip
		}
	}

	messageCols, err := sqliteColumns(dbPath, "message")
	if err != nil || len(messageCols) == 0 {
		return nil, nil
	}

	sessionIDCol := firstExistingColumn(messageCols, "session_id", "sessionID", "session")
	if sessionIDCol == "" {
		return nil, nil
	}

	msgOrderClause := ""
	if msgOrderCol := firstExistingColumn(messageCols, "time_created", "created", "time_updated", "updated"); msgOrderCol != "" {
		msgOrderClause = fmt.Sprintf(" ORDER BY %s", msgOrderCol)
	}

	var msgSelect string
	if messageCols["data"] {
		// Get messages for this session as a JSON array
		msgSelect = fmt.Sprintf(
			`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE %s=%s%s;`,
			sessionIDCol, sqliteQuote(sessionID), msgOrderClause,
		)
	} else {
		// No single "data" blob column — reconstruct a JSON object from
		// whichever recognizable columns exist on this schema.
		var parts []string
		for _, col := range []string{"id", "role", "content", "time", "time_created", "created"} {
			if messageCols[col] {
				parts = append(parts, fmt.Sprintf("%s, %s", sqliteQuote(col), col))
			}
		}
		if len(parts) == 0 {
			return nil, nil
		}
		msgSelect = fmt.Sprintf(
			`SELECT json_group_array(json_object(%s)) FROM message WHERE %s=%s%s;`,
			strings.Join(parts, ", "), sessionIDCol, sqliteQuote(sessionID), msgOrderClause,
		)
	}

	msgOutput, err := sqliteQuery(dbPath, msgSelect)
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(msgOutput))
	// sqlite3 returns "[null]" when no rows match
	if len(transcriptData) == 0 || string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
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

// sqliteQuery runs a single-statement sqlite3 query against dbPath and
// returns its raw stdout.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// sqliteColumns returns the set of column names for a table via
// PRAGMA table_info. Returns an empty (non-nil) map if the table doesn't
// exist or has no columns.
func sqliteColumns(dbPath, table string) (map[string]bool, error) {
	out, err := sqliteQuery(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return nil, err
	}
	cols := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 && fields[1] != "" {
			cols[fields[1]] = true
		}
	}
	return cols, nil
}

// firstExistingColumn returns the first candidate present in cols, or "" if
// none of them are.
func firstExistingColumn(cols map[string]bool, candidates ...string) string {
	for _, c := range candidates {
		if cols[c] {
			return c
		}
	}
	return ""
}

// sqliteQuote safely quotes a string literal for embedding in a SQL query.
func sqliteQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// isRecentTimestamp parses a timestamp string in several formats OpenCode
// might store (RFC3339 variants, plain datetime, or unix epoch seconds/
// milliseconds) and reports whether it falls within the recent-session
// window. ok is false if the timestamp could not be parsed at all, in which
// case the caller should proceed rather than treat it as stale.
func isRecentTimestamp(timeStr string) (recent bool, ok bool) {
	if timeStr == "" {
		return false, false
	}

	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout, true
		}
	}

	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		t := time.Unix(n, 0)
		if n > 1e12 { // milliseconds since epoch
			t = time.UnixMilli(n)
		}
		return time.Since(t) <= agent.RecentSessionTimeout, true
	}

	return false, false
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
