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
// OpenCode's on-disk storage layout has changed across versions (flat JSON
// files partitioned by project, flat JSON files with no partitioning at all,
// and SQLite with varying schemas), so each known layout is tried in turn
// instead of assuming one fixed shape.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage partitioned by project directory (older OpenCode).
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	projectID := GetProjectID(projectPath)

	// Fall back to a recursive scan of the data directory: some OpenCode
	// versions don't partition session files by project on disk at all,
	// recording the project only as a field inside each session's JSON.
	session, err = a.discoverFromRecursiveScan(projectPath, projectID)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite, which some OpenCode versions use instead of
	// flat JSON files.
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

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

// discoverFromRecursiveScan searches the entire OpenCode data directory for a
// session JSON file belonging to this project, without assuming any specific
// directory layout. This tolerates on-disk structure changes between
// OpenCode versions, since the project is matched via fields embedded in
// each session's JSON rather than via directory nesting.
func (a *Agent) discoverFromRecursiveScan(projectPath, projectID string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	absProjectPath, err := filepath.Abs(projectPath)
	if err != nil {
		absProjectPath = projectPath
	}

	now := time.Now()
	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > 1<<20 {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var candidate map[string]interface{}
		if err := json.Unmarshal(data, &candidate); err != nil {
			return nil
		}

		id, _ := candidate["id"].(string)
		if id == "" {
			return nil
		}

		matches := false
		for _, key := range []string{"projectID", "project_id", "projectId"} {
			if v, ok := candidate[key].(string); ok && v == projectID {
				matches = true
				break
			}
		}
		if !matches {
			for _, key := range []string{"directory", "cwd", "worktree", "path"} {
				if v, ok := candidate[key].(string); ok && (v == projectPath || v == absProjectPath) {
					matches = true
					break
				}
			}
		}
		if !matches {
			return nil
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = id
			bestModTime = modTime
		}
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: findMessageDir(dataDir, bestSessionID),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// findMessageDir searches dataDir for a directory holding message files for
// the given session ID, tolerating different nesting between OpenCode
// versions.
func findMessageDir(dataDir, sessionID string) string {
	if msgDir, err := GetMessageDir(sessionID); err == nil {
		if info, err := os.Stat(msgDir); err == nil && info.IsDir() {
			return msgDir
		}
	}

	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// discoverFromSQLite queries an OpenCode SQLite database for the most recent
// session. OpenCode's SQLite schema (table names, column names, whether a
// project-scoping column exists, and where the database file lives) has
// varied across versions, so the schema is introspected instead of assumed.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	candidates := []string{
		filepath.Join(projectPath, ".opencode", "opencode.db"),
		filepath.Join(dataDir, "opencode.db"),
		filepath.Join(dataDir, "storage", "opencode.db"),
		filepath.Join(dataDir, "storage.db"),
	}

	var dbPath string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			dbPath = c
			break
		}
	}
	if dbPath == "" {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	tables := sqliteTableNames(dbPath)
	sessionTable := pickName(tables, "session", "sessions")
	messageTable := pickName(tables, "message", "messages")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols := sqliteColumnNames(dbPath, sessionTable)
	projectCol := pickName(sessionCols, "project_id", "projectID", "project")
	sessionUpdatedCol := pickName(sessionCols, "time_updated", "updated_at", "updatedAt", "time_created", "created_at")

	// Find most recent session for this project (or overall, if this
	// OpenCode version scopes the whole database to a single project).
	sessionQuery := "SELECT id FROM " + sessionTable
	if projectCol != "" {
		sessionQuery += fmt.Sprintf(" WHERE %s='%s'", projectCol, projectID)
	}
	if sessionUpdatedCol != "" {
		sessionQuery += fmt.Sprintf(" ORDER BY %s DESC", sessionUpdatedCol)
	}
	sessionQuery += " LIMIT 1;"

	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Check if this session was recent (within timeout)
	if sessionUpdatedCol != "" {
		timeQuery := fmt.Sprintf("SELECT %s FROM %s WHERE id='%s';", sessionUpdatedCol, sessionTable, sessionID)
		cmd = exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			if !isRecentTimestamp(strings.TrimSpace(string(timeOutput))) {
				return nil, nil
			}
		}
		// If we can't run the query, proceed anyway — better to try than skip
	}

	messageCols := sqliteColumnNames(dbPath, messageTable)
	sessionFK := pickName(messageCols, "session_id", "sessionID", "session")
	if sessionFK == "" {
		return nil, nil
	}
	messageCreatedCol := pickName(messageCols, "time_created", "created_at", "createdAt")

	// Build a per-row JSON object for messages regardless of whether the
	// table stores a single JSON blob column ("data") or individual typed
	// columns ("role"/"content"/"parts"/"model").
	var selectExpr string
	if containsName(messageCols, "data") {
		selectExpr = "json_patch(data, json_object('id', id))"
	} else {
		fields := []string{"'id', id"}
		if containsName(messageCols, "role") {
			fields = append(fields, "'role', role")
		}
		if containsName(messageCols, "content") {
			fields = append(fields, "'content', content")
		} else if containsName(messageCols, "parts") {
			fields = append(fields, "'content', parts")
		}
		if containsName(messageCols, "model") {
			fields = append(fields, "'model', model")
		}
		if messageCreatedCol != "" {
			fields = append(fields, fmt.Sprintf("'time', json_object('created', %s)", messageCreatedCol))
		}
		selectExpr = "json_object(" + strings.Join(fields, ", ") + ")"
	}

	// Get messages for this session as a JSON array
	msgQuery := fmt.Sprintf("SELECT json_group_array(%s) FROM %s WHERE %s='%s'", selectExpr, messageTable, sessionFK, sessionID)
	if messageCreatedCol != "" {
		msgQuery += fmt.Sprintf(" ORDER BY %s", messageCreatedCol)
	}
	msgQuery += ";"

	cmd = exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
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

// sqliteTableNames returns the names of all tables in the SQLite database.
func sqliteTableNames(dbPath string) []string {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	return splitNonEmptyLines(string(output))
}

// sqliteColumnNames returns the column names of the given table, via
// PRAGMA table_info (default '|'-separated output: cid|name|type|...).
func sqliteColumnNames(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range splitNonEmptyLines(string(output)) {
		parts := strings.Split(line, "|")
		if len(parts) >= 2 {
			cols = append(cols, parts[1])
		}
	}
	return cols
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

// pickName returns the first entry in names that case-insensitively matches
// one of candidates, tried in candidate priority order. Returns "" if none
// match.
func pickName(names []string, candidates ...string) string {
	for _, c := range candidates {
		for _, n := range names {
			if strings.EqualFold(n, c) {
				return n
			}
		}
	}
	return ""
}

func containsName(names []string, target string) bool {
	return pickName(names, target) != ""
}

// isRecentTimestamp reports whether raw — in any of OpenCode's known
// timestamp formats (RFC3339, SQL datetime, or Unix seconds/milliseconds) —
// is within the recent-session window. An unrecognised format is treated as
// recent so a schema surprise doesn't silently block discovery.
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
		if n > 1e12 { // looks like milliseconds, not seconds
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
