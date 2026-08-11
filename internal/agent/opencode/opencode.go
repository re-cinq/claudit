package opencode

import (
	"encoding/json"
	"fmt"
	"io"
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
// OpenCode's on-disk storage layout and project-ID scheme have changed
// across releases, so we probe multiple known layouts (and introspect the
// SQLite schema at runtime) rather than assuming a single fixed format.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)

	// Try flat file storage first (pre-v1.2 OpenCode, and any layout variants)
	session, err := discoverFromFlatFiles(dataDir, projectID, projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite (OpenCode v1.2+)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// discoverFromFlatFiles tries flat JSON file session discovery, probing
// every known OpenCode storage layout. If nothing is found under the
// project-scoped directories (e.g. because OpenCode's project ID scheme no
// longer matches ours), it falls back to scanning all projects under the
// data directory and matching by the session's recorded working directory.
func discoverFromFlatFiles(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout

	bestSessionID, bestModTime := newestRecentJSON(candidateSessionDirs(dataDir, projectID), now, recentTimeout)

	if bestSessionID == "" {
		bestSessionID, bestModTime = newestMatchingSession(dataDir, projectPath, now, recentTimeout)
	}

	if bestSessionID == "" {
		return nil, nil
	}

	// The transcript path for OpenCode is the message directory
	msgDir := ""
	for _, candidate := range candidateMessageDirs(dataDir, projectID, bestSessionID) {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			msgDir = candidate
			break
		}
	}
	if msgDir == "" {
		msgDir, _ = GetMessageDir(bestSessionID)
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session. OpenCode's SQLite schema (table/column names and casing)
// has changed across releases, so it is introspected at runtime via PRAGMA
// rather than assumed.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	tables := sqliteTables(dbPath)
	sessionTable := findTable(tables, "session")
	messageTable := findTable(tables, "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols := sqliteColumns(dbPath, sessionTable)
	idCol := findColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := findColumn(sessionCols, "project_id", "projectid", "project")
	dirCol := findColumn(sessionCols, "directory", "worktree", "cwd", "path")
	timeCol := findColumn(sessionCols, "time_updated", "updated", "time_created", "created", "time")

	sessionID := querySessionID(dbPath, sessionTable, idCol, timeCol, projectCol, projectID, dirCol, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout), when a usable
	// timestamp column exists. If we can't find or parse it, proceed
	// anyway — better to try than to skip a genuinely active session.
	if timeCol != "" {
		query := fmt.Sprintf(`SELECT "%s" FROM "%s" WHERE "%s"='%s';`, timeCol, sessionTable, idCol, escapeSQLite(sessionID))
		cmd := exec.Command("sqlite3", dbPath, query)
		if out, err := cmd.Output(); err == nil {
			if t, ok := parseSQLiteTime(strings.TrimSpace(string(out))); ok {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
		}
	}

	msgCols := sqliteColumns(dbPath, messageTable)
	msgIDCol := findColumn(msgCols, "id")
	msgSessionCol := findColumn(msgCols, "session_id", "sessionid", "session")
	msgDataCol := findColumn(msgCols, "data", "content", "message")
	msgTimeCol := findColumn(msgCols, "time_created", "created", "time")

	if msgSessionCol == "" || msgDataCol == "" {
		return nil, nil
	}

	// Get messages for this session as a JSON array
	var msgQuery string
	if msgIDCol != "" {
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array(json_patch("%s", json_object('id', "%s"))) FROM "%s" WHERE "%s"='%s'`,
			msgDataCol, msgIDCol, messageTable, msgSessionCol, escapeSQLite(sessionID))
	} else {
		msgQuery = fmt.Sprintf(
			`SELECT json_group_array("%s") FROM "%s" WHERE "%s"='%s'`,
			msgDataCol, messageTable, msgSessionCol, escapeSQLite(sessionID))
	}
	if msgTimeCol != "" {
		msgQuery += fmt.Sprintf(` ORDER BY "%s"`, msgTimeCol)
	}
	msgQuery += ";"

	cmd := exec.Command("sqlite3", dbPath, msgQuery)
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

// querySessionID finds the most recent session ID, trying a project-ID
// column match first, then a directory/path column match (in case
// OpenCode's project ID scheme differs from ours), and finally the most
// recent session overall if neither column exists on this schema.
func querySessionID(dbPath, table, idCol, timeCol, projectCol, projectID, dirCol, projectPath string) string {
	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(` ORDER BY "%s" DESC`, timeCol)
	}

	tryQuery := func(where string) string {
		query := fmt.Sprintf(`SELECT "%s" FROM "%s"`, idCol, table)
		if where != "" {
			query += " WHERE " + where
		}
		query += orderClause + " LIMIT 1;"

		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	if projectCol != "" {
		if id := tryQuery(fmt.Sprintf(`"%s"='%s'`, projectCol, escapeSQLite(projectID))); id != "" {
			return id
		}
	}
	if dirCol != "" {
		if id := tryQuery(fmt.Sprintf(`"%s"='%s'`, dirCol, escapeSQLite(projectPath))); id != "" {
			return id
		}
	}
	if projectCol == "" && dirCol == "" {
		return tryQuery("")
	}
	return ""
}

// newestRecentJSON returns the session ID (filename without extension) and
// mod time of the most recently modified .json file across the given
// directories, considering only files modified within recentTimeout.
func newestRecentJSON(dirs []string, now time.Time, recentTimeout time.Duration) (string, time.Time) {
	var bestID string
	var bestModTime time.Time

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
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
			if bestID == "" || modTime.After(bestModTime) {
				bestID = strings.TrimSuffix(entry.Name(), ".json")
				bestModTime = modTime
			}
		}
	}

	return bestID, bestModTime
}

// newestMatchingSession scans every project's session directory under the
// data directory (across known layouts) for the most recently modified
// session file whose recorded directory field matches projectPath. This is
// used when the project ID we compute no longer matches OpenCode's scheme.
func newestMatchingSession(dataDir, projectPath string, now time.Time, recentTimeout time.Duration) (string, time.Time) {
	patterns := []string{
		filepath.Join(dataDir, "storage", "session", "*", "*.json"),
		filepath.Join(dataDir, "project", "*", "storage", "session", "*.json"),
		filepath.Join(dataDir, "project", "*", "storage", "session", "info", "*.json"),
	}

	var bestID string
	var bestModTime time.Time

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, path := range matches {
			info, err := os.Stat(path)
			if err != nil || info.IsDir() {
				continue
			}
			modTime := info.ModTime()
			if now.Sub(modTime) > recentTimeout {
				continue
			}

			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var fields map[string]interface{}
			if err := json.Unmarshal(data, &fields); err != nil {
				continue
			}
			if !sessionMatchesProject(fields, projectPath) {
				continue
			}

			if bestID == "" || modTime.After(bestModTime) {
				bestID = strings.TrimSuffix(filepath.Base(path), ".json")
				bestModTime = modTime
			}
		}
	}

	return bestID, bestModTime
}

// sessionMatchesProject checks whether a session JSON record's directory
// field matches the given project path. If the record has none of the
// known directory-like fields, it is accepted (it's the only candidate
// found within the recency window across a single project's data).
func sessionMatchesProject(fields map[string]interface{}, projectPath string) bool {
	for _, key := range []string{"directory", "worktree", "cwd", "path"} {
		if v, ok := fields[key].(string); ok && v != "" {
			return v == projectPath
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
