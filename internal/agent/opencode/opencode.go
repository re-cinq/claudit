```go
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
// Newer OpenCode versions may nest message files under subdirectories (e.g.
// sharded by ID prefix), so this walks the tree recursively rather than
// assuming every message file lives directly inside dir.
func (a *Agent) parseMessageDir(dir string) (*agent.Transcript, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}

	var entries []agent.TranscriptEntry

	_ = filepath.WalkDir(dir, func(path string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}

		name := de.Name()
		switch {
		case strings.HasSuffix(name, ".jsonl"):
			f, openErr := os.Open(path)
			if openErr != nil {
				return nil
			}
			transcript, parseErr := a.ParseTranscript(f)
			_ = f.Close()
			if parseErr == nil {
				entries = append(entries, transcript.Entries...)
			}
		case strings.HasSuffix(name, ".json"):
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}

			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil {
				return nil
			}

			entry := parseOpenCodeEntry(raw, data)
			if entry.Type != "" {
				entries = append(entries, entry)
			}
		}

		return nil
	})

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
// OpenCode has changed how it derives a project's storage directory across
// versions, so this first tries the directory we compute ourselves (fast
// path, matches historically-verified versions), then falls back to scanning
// every project directory under storage/session and matching sessions by
// their recorded working directory instead of by our own project ID guess.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	sessionDir, err := GetSessionDir(projectPath)
	if err == nil {
		if session := mostRecentSessionFile(sessionDir, projectPath); session != nil {
			return session, nil
		}
	}

	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	if _, err := os.Stat(sessionRoot); err != nil {
		return nil, nil
	}

	resolvedProjectPath := projectPath
	if resolved, err := filepath.EvalSymlinks(projectPath); err == nil {
		resolvedProjectPath = resolved
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout
	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(sessionRoot, func(path string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			return nil
		}

		info, err := de.Info()
		if err != nil {
			return nil
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > recentTimeout {
			return nil
		}
		if bestSessionID != "" && !modTime.After(bestModTime) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var info2 sessionInfo
		if err := json.Unmarshal(data, &info2); err != nil || info2.Directory == "" {
			return nil
		}

		sessionDirPath := info2.Directory
		if resolved, err := filepath.EvalSymlinks(sessionDirPath); err == nil {
			sessionDirPath = resolved
		}
		if sessionDirPath != resolvedProjectPath && info2.Directory != projectPath {
			return nil
		}

		bestSessionID = strings.TrimSuffix(de.Name(), ".json")
		bestModTime = modTime
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

// mostRecentSessionFile returns the most recently modified session file
// found under dir (which is expected to be storage/session/<projectID>),
// walking recursively in case OpenCode now shards session files into
// subdirectories rather than storing them flat.
func mostRecentSessionFile(dir, projectPath string) *agent.SessionInfo {
	if _, err := os.Stat(dir); err != nil {
		return nil
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout
	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(dir, func(path string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			return nil
		}

		info, err := de.Info()
		if err != nil {
			return nil
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > recentTimeout {
			return nil
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = strings.TrimSuffix(de.Name(), ".json")
			bestModTime = modTime
		}
		return nil
	})

	if bestSessionID == "" {
		return nil
	}

	msgDir, _ := GetMessageDir(bestSessionID)

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
// Column names have changed between OpenCode releases (e.g. project_id/time_updated
// have been renamed in some versions), so the schema is introspected via
// PRAGMA table_info instead of assuming fixed column names.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionCols := sqliteTableColumns(dbPath, "session")
	if len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := firstMatchingColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := firstMatchingColumn(sessionCols, "project_id", "projectid", "project")
	dirCol := firstMatchingColumn(sessionCols, "directory", "cwd", "worktree", "path")
	timeCol := firstMatchingColumn(sessionCols, "time_updated", "updated_at", "updated", "time_created", "created_at", "created")

	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}

	sessionID := ""
	if projectCol != "" {
		query := fmt.Sprintf(`SELECT %s FROM session WHERE %s='%s'%s LIMIT 1;`,
			idCol, projectCol, escapeSQLiteLiteral(projectID), orderClause)
		out, err := exec.Command("sqlite3", "-separator", "\t", dbPath, query).Output()
		if err == nil {
			sessionID = strings.TrimSpace(string(out))
		}
	}
	if sessionID == "" && dirCol != "" {
		query := fmt.Sprintf(`SELECT %s FROM session WHERE %s='%s'%s LIMIT 1;`,
			idCol, dirCol, escapeSQLiteLiteral(projectPath), orderClause)
		out, err := exec.Command("sqlite3", "-separator", "\t", dbPath, query).Output()
		if err == nil {
			sessionID = strings.TrimSpace(string(out))
		}
	}
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout), when we know which
	// column tracks recency.
	if timeCol != "" {
		query := fmt.Sprintf(`SELECT %s FROM session WHERE %s='%s';`,
			timeCol, idCol, escapeSQLiteLiteral(sessionID))
		out, err := exec.Command("sqlite3", dbPath, query).Output()
		if err == nil {
			timeStr := strings.TrimSpace(string(out))
			if timeStr != "" && !isRecentSQLiteTimestamp(timeStr) {
				return nil, nil
			}
		}
	}

	messageCols := sqliteTableColumns(dbPath, "message")
	msgSessionCol := firstMatchingColumn(messageCols, "session_id", "sessionid", "session")
	msgDataCol := firstMatchingColumn(messageCols, "data", "content", "body")
	msgIDCol := firstMatchingColumn(messageCols, "id")
	msgTimeCol := firstMatchingColumn(messageCols, "time_created", "created_at", "created", "time_updated", "updated")

	var transcriptData []byte
	if msgSessionCol != "" && msgDataCol != "" && msgIDCol != "" {
		msgOrderClause := ""
		if msgTimeCol != "" {
			msgOrderClause = fmt.Sprintf(" ORDER BY %s", msgTimeCol)
		}
		msgQuery := fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM message WHERE %s='%s'%s;`,
			msgDataCol, msgIDCol, msgSessionCol, escapeSQLiteLiteral(sessionID), msgOrderClause,
		)
		out, err := exec.Command("sqlite3", dbPath, msgQuery).Output()
		if err == nil {
			data := strings.TrimSpace(string(out))
			// sqlite3 returns "[null]" when no rows match
			if data != "" && data != "[null]" && data != "[]" {
				transcriptData = []byte(data)
			}
		}
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// sqliteTableColumns returns the column names of a SQLite table via
// PRAGMA table_info, or nil if the table doesn't exist or the query fails.
func sqliteTableColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// firstMatchingColumn returns the actual column name (preserving original
// case) matching the first candidate found, checking exact (case-insensitive)
// matches before falling back to substring matches.
func firstMatchingColumn(cols []string, candidates ...string) string {
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

// isRecentSQLiteTimestamp reports whether a timestamp string (in one of a
// few common formats) falls within RecentSessionTimeout. If the format is
// unrecognized, it reports true — better to try than to skip a real session.
func isRecentSQLiteTimestamp(timeStr string) bool {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}
	return true
}

// escapeSQLiteLiteral escapes single quotes for use in a SQLite string literal.
func escapeSQLiteLiteral(s string) string {
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
```
