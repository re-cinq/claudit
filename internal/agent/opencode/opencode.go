package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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
// It tries, in order: flat file storage (pre-v1.2 OpenCode), SQLite storage
// (v1.2+), and finally a format-agnostic scan of the data directory. The
// final fallback exists because OpenCode's on-disk storage layout and
// SQLite schema have changed across releases (npm publishes new versions
// frequently), so relying on a single fixed layout is not durable.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (pre-v1.2 OpenCode)
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	// Fall back to SQLite (OpenCode v1.2+)
	projectID := GetProjectID(projectPath)
	if session, err := discoverFromSQLite(dataDir, projectID, projectPath); err == nil && session != nil {
		return session, nil
	}

	// Last resort: scan the data directory for the most recently modified
	// session file, regardless of the exact directory/database layout.
	return discoverFromRecentActivity(dataDir, projectPath)
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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session belonging to projectID. Table and column names are discovered
// dynamically (rather than assumed) because OpenCode's SQLite schema has
// changed between releases.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := findSQLiteTable(dbPath, "session")
	if sessionTable == "" {
		return nil, nil
	}
	sessionCols := sqliteTableColumns(dbPath, sessionTable)

	idCol := pickSQLiteColumn(sessionCols, "id")
	projectCol := pickSQLiteColumn(sessionCols, "project_id", "projectid", "project", "workspace_id", "workspaceid")
	updatedCol := pickSQLiteColumn(sessionCols, "time_updated", "updated_at", "updatedat", "mtime", "modified", "updated")

	if idCol == "" {
		return nil, nil
	}

	var conditions []string
	if projectCol != "" {
		conditions = append(conditions, fmt.Sprintf("%s='%s'", projectCol, projectID))
	}
	orderClause := ""
	if updatedCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", updatedCol)
	}
	whereClause := ""
	if len(conditions) > 0 {
		whereClause = " WHERE " + strings.Join(conditions, " AND ")
	}

	sessionQuery := fmt.Sprintf(`SELECT %s FROM %s%s%s LIMIT 1;`, idCol, sessionTable, whereClause, orderClause)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Check if this session was recent (within timeout), if we found a timestamp column
	if updatedCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, updatedCol, sessionTable, idCol, sessionID)
		cmd = exec.Command("sqlite3", dbPath, timeQuery)
		timeOutput, err := cmd.Output()
		if err == nil {
			timeStr := strings.TrimSpace(string(timeOutput))
			if t, err := time.Parse(time.RFC3339Nano, timeStr); err == nil {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			} else if t, err := time.Parse("2006-01-02T15:04:05.000Z", timeStr); err == nil {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			} else if t, err := time.Parse("2006-01-02 15:04:05", timeStr); err == nil {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			} else if ms, err := parseUnixMillis(timeStr); err == nil {
				if time.Since(ms) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
			// If we can't parse the time, proceed anyway — better to try than skip
		}
	}

	messageTable := findSQLiteTable(dbPath, "message")
	if messageTable == "" {
		return nil, nil
	}
	messageCols := sqliteTableColumns(dbPath, messageTable)

	dataCol := pickSQLiteColumn(messageCols, "data", "content", "body", "message")
	sessionRefCol := pickSQLiteColumn(messageCols, "session_id", "sessionid", "session")
	msgIDCol := pickSQLiteColumn(messageCols, "id")
	msgOrderCol := pickSQLiteColumn(messageCols, "time_created", "created_at", "createdat", "ctime", "created")

	if dataCol == "" || sessionRefCol == "" {
		return nil, nil
	}

	selectExpr := dataCol
	if msgIDCol != "" {
		selectExpr = fmt.Sprintf(`json_patch(%s, json_object('id', %s))`, dataCol, msgIDCol)
	}
	msgOrderClause := ""
	if msgOrderCol != "" {
		msgOrderClause = fmt.Sprintf(` ORDER BY %s`, msgOrderCol)
	}

	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM %s WHERE %s='%s'%s;`,
		selectExpr, messageTable, sessionRefCol, sessionID, msgOrderClause,
	)
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

// findSQLiteTable returns the first table in the database whose name contains
// the given substring (case-insensitive), or "" if none match. Used instead
// of assuming a fixed table name since OpenCode's schema has changed across
// releases.
func findSQLiteTable(dbPath, substr string) string {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	lower := strings.ToLower(substr)
	for _, name := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if strings.Contains(strings.ToLower(name), lower) {
			return name
		}
	}
	return ""
}

// sqliteTableColumns returns the column names of the given table.
func sqliteTableColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) >= 2 {
			cols = append(cols, parts[1])
		}
	}
	return cols
}

// pickSQLiteColumn returns the first column (case-insensitive match) from
// cols that matches any of the candidate names, or "" if none match.
func pickSQLiteColumn(cols []string, candidates ...string) string {
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.EqualFold(c, cand) {
				return c
			}
		}
	}
	return ""
}

// parseUnixMillis parses a string as milliseconds (or seconds) since the
// Unix epoch, returning the corresponding time.
func parseUnixMillis(s string) (time.Time, error) {
	var ms int64
	if _, err := fmt.Sscanf(s, "%d", &ms); err != nil {
		return time.Time{}, err
	}
	if ms == 0 {
		return time.Time{}, fmt.Errorf("not a timestamp")
	}
	// Heuristic: treat as milliseconds if large enough, otherwise seconds.
	if ms > 1e12 {
		return time.UnixMilli(ms), nil
	}
	return time.Unix(ms, 0), nil
}

// discoverFromRecentActivity is a format-agnostic fallback that scans the
// OpenCode data directory for the most recently modified session file,
// without assuming any particular directory layout. It exists because
// OpenCode's on-disk storage location and structure have changed across
// releases and neither the flat-file nor SQLite discovery may match the
// installed version.
//
// It prefers files whose content appears to reference projectPath, but
// falls back to the single most-recently-modified session file overall if
// no such reference is found (sessions are commonly stored without an
// explicit back-reference to the project directory).
func discoverFromRecentActivity(dataDir, projectPath string) (*agent.SessionInfo, error) {
	info, statErr := os.Stat(dataDir)
	if statErr != nil || !info.IsDir() {
		return nil, nil
	}

	now := time.Now()
	var bestPath, bestMatchPath string
	var bestModTime, bestMatchModTime time.Time

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		if !strings.Contains(strings.ToLower(filepath.ToSlash(path)), "session") {
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return nil
		}
		modTime := fi.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}

		if bestPath == "" || modTime.After(bestModTime) {
			bestPath = path
			bestModTime = modTime
		}

		if data, err := os.ReadFile(path); err == nil && bytesContainsPath(data, projectPath) {
			if bestMatchPath == "" || modTime.After(bestMatchModTime) {
				bestMatchPath = path
				bestMatchModTime = modTime
			}
		}

		return nil
	})

	chosen := bestMatchPath
	chosenModTime := bestMatchModTime
	if chosen == "" {
		chosen = bestPath
		chosenModTime = bestModTime
	}
	if chosen == "" {
		return nil, nil
	}

	sessionID := strings.TrimSuffix(filepath.Base(chosen), ".json")
	var meta struct {
		ID string `json:"id"`
	}
	if data, err := os.ReadFile(chosen); err == nil {
		if err := json.Unmarshal(data, &meta); err == nil && meta.ID != "" {
			sessionID = meta.ID
		}
	}

	// Prefer the conventional message directory for this session ID; fall
	// back to the directory containing the discovered file.
	transcriptPath := filepath.Dir(chosen)
	if msgDir, err := GetMessageDir(sessionID); err == nil {
		if st, err := os.Stat(msgDir); err == nil && st.IsDir() {
			transcriptPath = msgDir
		}
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
		StartedAt:      chosenModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// bytesContainsPath reports whether data contains projectPath as a substring.
func bytesContainsPath(data []byte, projectPath string) bool {
	if projectPath == "" {
		return false
	}
	return strings.Contains(string(data), projectPath)
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
