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

	if info := scanSessionDir(sessionDir); info != nil {
		info.ProjectPath = projectPath
		return info, nil
	}

	// The project-ID-keyed directory had no (recent) session. OpenCode's
	// project-identification scheme has changed across versions, so a
	// session may still be sitting under a different project directory
	// than the one we compute. Scan every project directory under
	// storage/session and pick the most recently touched session rather
	// than silently reporting "no active session".
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	baseSessionDir := filepath.Join(dataDir, "storage", "session")
	projectDirs, err := os.ReadDir(baseSessionDir)
	if err != nil {
		return nil, nil
	}

	var best *agent.SessionInfo
	var bestModTime time.Time
	for _, pd := range projectDirs {
		if !pd.IsDir() {
			continue
		}
		info := scanSessionDir(filepath.Join(baseSessionDir, pd.Name()))
		if info == nil {
			continue
		}
		modTime, err := time.Parse(time.RFC3339, info.StartedAt)
		if err != nil {
			continue
		}
		if best == nil || modTime.After(bestModTime) {
			best = info
			bestModTime = modTime
		}
	}

	if best == nil {
		return nil, nil
	}

	best.ProjectPath = projectPath
	return best, nil
}

// scanSessionDir returns the most recently modified session in dir (within
// the recent-session timeout), or nil if none is found.
func scanSessionDir(dir string) *agent.SessionInfo {
	dirEntries, err := os.ReadDir(dir)
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
	}
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session. OpenCode's SQLite schema (table names, column names, and the
// project-keying scheme) has changed across versions, so table/column names
// are introspected rather than hardcoded, and if no session matches our
// computed project ID, we fall back to the most recently updated session in
// the database rather than reporting nothing found.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, sessionCols := sqliteFindTable(dbPath, "session")
	if sessionTable == "" {
		return nil, nil
	}
	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "directory", "project", "worktree", "cwd")
	timeCol := pickColumn(sessionCols, "time_updated", "updated_at", "updatedat", "time_created", "created_at", "createdat")

	sessionID, updatedStr := sqliteFindSession(dbPath, sessionTable, idCol, projectCol, timeCol, projectID)
	if sessionID == "" {
		return nil, nil
	}

	if !withinRecentTimeout(updatedStr) {
		return nil, nil
	}

	messageTable, messageCols := sqliteFindTable(dbPath, "message")
	if messageTable == "" {
		return &agent.SessionInfo{
			SessionID:   sessionID,
			StartedAt:   time.Now().Format(time.RFC3339),
			ProjectPath: projectPath,
		}, nil
	}

	sessionIDCol := pickColumn(messageCols, "session_id", "sessionid")
	dataCol := pickColumn(messageCols, "data", "parts", "content")
	msgIDCol := pickColumn(messageCols, "id")
	orderCol := pickColumn(messageCols, "time_created", "createdat", "created_at", "time")

	if sessionIDCol == "" || dataCol == "" {
		return &agent.SessionInfo{
			SessionID:   sessionID,
			StartedAt:   time.Now().Format(time.RFC3339),
			ProjectPath: projectPath,
		}, nil
	}

	// Get messages for this session as a JSON array
	patchExpr := dataCol
	if msgIDCol != "" {
		patchExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, msgIDCol)
	}
	msgQuery := fmt.Sprintf(`SELECT json_group_array(%s) FROM %s WHERE %s='%s'`,
		patchExpr, messageTable, sessionIDCol, sessionID)
	if orderCol != "" {
		msgQuery += fmt.Sprintf(" ORDER BY %s", orderCol)
	}
	msgQuery += ";"

	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
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

// sqliteFindTable finds a table whose name matches hint (e.g. "session"
// matches "session" or "sessions") and returns its name and column names.
func sqliteFindTable(dbPath, hint string) (string, []string) {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	output, err := cmd.Output()
	if err != nil {
		return "", nil
	}

	var candidates []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if lower == hint || lower == hint+"s" || strings.Contains(lower, hint) {
			candidates = append(candidates, name)
		}
	}

	for _, table := range candidates {
		if cols := sqliteColumnNames(dbPath, table); len(cols) > 0 {
			return table, cols
		}
	}
	return "", nil
}

// sqliteColumnNames returns the column names of table via PRAGMA table_info.
func sqliteColumnNames(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) < 2 {
			continue
		}
		cols = append(cols, fields[1])
	}
	return cols
}

// pickColumn returns the first column in cols matching (case-insensitively)
// any of candidates, in candidate priority order, or "" if none match.
func pickColumn(cols []string, candidates ...string) string {
	for _, cand := range candidates {
		for _, col := range cols {
			if strings.EqualFold(col, cand) {
				return col
			}
		}
	}
	return ""
}

// sqliteFindSession looks up the most recently updated session for projectID.
// If projectCol is unknown, or no row matches projectID, it falls back to
// the most recently updated session overall — OpenCode's project-keying
// scheme is not guaranteed to match what we compute.
func sqliteFindSession(dbPath, table, idCol, projectCol, timeCol, projectID string) (id, updated string) {
	selectCols := idCol
	if timeCol != "" {
		selectCols += ", " + timeCol
	}

	if projectCol != "" {
		q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s'`, selectCols, table, projectCol, projectID)
		if timeCol != "" {
			q += " ORDER BY " + timeCol + " DESC"
		}
		q += " LIMIT 1;"
		if foundID, foundUpdated, ok := sqliteQueryRow(dbPath, q, timeCol != ""); ok {
			return foundID, foundUpdated
		}
	}

	q := fmt.Sprintf(`SELECT %s FROM %s`, selectCols, table)
	if timeCol != "" {
		q += " ORDER BY " + timeCol + " DESC"
	}
	q += " LIMIT 1;"
	foundID, foundUpdated, _ := sqliteQueryRow(dbPath, q, timeCol != "")
	return foundID, foundUpdated
}

// sqliteQueryRow runs query (expected to return a single row) and splits the
// tab-separated output into an id and, if hasTime, an updated-at value.
func sqliteQueryRow(dbPath, query string, hasTime bool) (id, updated string, ok bool) {
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return "", "", false
	}

	line := strings.TrimSpace(string(output))
	if line == "" {
		return "", "", false
	}

	parts := strings.SplitN(line, "\t", 2)
	id = strings.TrimSpace(parts[0])
	if id == "" {
		return "", "", false
	}
	if hasTime && len(parts) > 1 {
		updated = strings.TrimSpace(parts[1])
	}
	return id, updated, true
}

// withinRecentTimeout reports whether value, in whatever timestamp format
// OpenCode's SQLite schema currently uses (RFC3339, a plain datetime, or a
// Unix epoch in seconds or milliseconds), is within RecentSessionTimeout.
// An empty or unrecognized value is treated as recent, since it's better to
// try a possibly-valid session than to silently skip it.
func withinRecentTimeout(value string) bool {
	if value == "" {
		return true
	}

	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		t := time.Unix(n, 0)
		if n > 1e12 {
			t = time.UnixMilli(n)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, value); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
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
