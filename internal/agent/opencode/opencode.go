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
// It first looks in the project directory keyed by our own project-ID
// heuristic (git root commit hash). OpenCode's internal project-identity
// scheme has changed across releases and may no longer agree with that
// heuristic, so if nothing is found there, every project directory under
// the session storage root is scanned and matched against the session's
// own recorded "directory" field instead.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")

	if session := findRecentSessionInDir(filepath.Join(sessionRoot, GetProjectID(projectPath)), projectPath, nil); session != nil {
		return session, nil
	}

	absProjectPath, err := filepath.Abs(projectPath)
	if err != nil {
		absProjectPath = projectPath
	}

	projEntries, err := os.ReadDir(sessionRoot)
	if err != nil {
		return nil, nil
	}

	for _, projEntry := range projEntries {
		if !projEntry.IsDir() {
			continue
		}
		dir := filepath.Join(sessionRoot, projEntry.Name())
		if session := findRecentSessionInDir(dir, projectPath, &absProjectPath); session != nil {
			return session, nil
		}
	}

	return nil, nil
}

// findRecentSessionInDir returns the most recently modified session found in
// dir within RecentSessionTimeout, or nil if none qualify. If matchDir is
// non-nil, a session is only considered when its own "directory" field
// resolves to *matchDir; otherwise every recent session file in dir counts.
func findRecentSessionInDir(dir, projectPath string, matchDir *string) *agent.SessionInfo {
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

		if matchDir != nil {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				continue
			}
			var si sessionInfo
			if err := json.Unmarshal(data, &si); err != nil || si.Directory == "" {
				continue
			}
			siAbs, err := filepath.Abs(si.Directory)
			if err != nil {
				siAbs = si.Directory
			}
			if siAbs != *matchDir {
				continue
			}
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

// sqliteColumns returns the set of column names for a table by querying
// PRAGMA table_info, or nil if the table doesn't exist or sqlite3 fails.
// OpenCode's SQLite schema has changed across releases, so callers use this
// instead of assuming fixed column names.
func sqliteColumns(dbPath, table string) map[string]bool {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	cols := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) >= 2 && parts[1] != "" {
			cols[parts[1]] = true
		}
	}
	return cols
}

// pickColumn returns the first candidate present in cols, or "" if none match.
func pickColumn(cols map[string]bool, candidates ...string) string {
	for _, c := range candidates {
		if cols[c] {
			return c
		}
	}
	return ""
}

// escapeSQLiteLiteral escapes a string for safe inclusion in a single-quoted SQLite literal.
func escapeSQLiteLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// sqliteQueryOne runs a query expected to return a single scalar value and
// returns its trimmed output, or "" if the query fails or returns nothing.
func sqliteQueryOne(dbPath, query string) string {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return strings.TrimSpace(line)
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
// Column names are discovered at runtime via sqliteColumns rather than assumed,
// since OpenCode's schema has changed across releases. If the project the
// session belongs to can't be matched via our project-ID heuristic (e.g. it no
// longer agrees with OpenCode's internal scheme), sessions are matched by
// their own recorded working directory instead.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionCols := sqliteColumns(dbPath, "session")
	if len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id", "sessionID", "session_id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := pickColumn(sessionCols, "time_updated", "updated", "time_modified", "modified", "time_created", "created")
	orderBy := "rowid DESC"
	if timeCol != "" {
		orderBy = timeCol + " DESC"
	}
	projectCol := pickColumn(sessionCols, "project_id", "projectID", "projectId", "project")
	dirCol := pickColumn(sessionCols, "directory", "path", "cwd", "worktree", "root")

	var sessionID string
	if projectCol != "" {
		sessionID = sqliteQueryOne(dbPath, fmt.Sprintf(
			`SELECT %s FROM session WHERE %s='%s' ORDER BY %s LIMIT 1;`,
			idCol, projectCol, escapeSQLiteLiteral(projectID), orderBy))
	}
	if sessionID == "" && dirCol != "" {
		// Our project-ID heuristic didn't match anything; fall back to
		// matching on the session's own recorded working directory.
		sessionID = sqliteQueryOne(dbPath, fmt.Sprintf(
			`SELECT %s FROM session WHERE %s='%s' ORDER BY %s LIMIT 1;`,
			idCol, dirCol, escapeSQLiteLiteral(projectPath), orderBy))
	}
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout)
	if timeCol != "" {
		timeStr := sqliteQueryOne(dbPath, fmt.Sprintf(
			`SELECT %s FROM session WHERE %s='%s';`, timeCol, idCol, escapeSQLiteLiteral(sessionID)))
		if timeStr != "" {
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
			} else if ms, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
				// OpenCode may store timestamps as Unix epoch milliseconds.
				if time.Since(time.UnixMilli(ms)) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
			// If we can't parse the time, proceed anyway — better to try than skip
		}
	}

	// Get messages for this session as a JSON array
	var transcriptData []byte
	msgCols := sqliteColumns(dbPath, "message")
	dataCol := pickColumn(msgCols, "data", "content", "message")
	msgSessionCol := pickColumn(msgCols, "session_id", "sessionID", "sessionId")

	if dataCol != "" && msgSessionCol != "" {
		msgIDCol := pickColumn(msgCols, "id", "messageID", "message_id")
		msgTimeCol := pickColumn(msgCols, "time_created", "created", "time_added", "added")

		selectExpr := dataCol
		if msgIDCol != "" {
			selectExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, msgIDCol)
		}
		order := "rowid"
		if msgTimeCol != "" {
			order = msgTimeCol
		}

		msgQuery := fmt.Sprintf(
			`SELECT json_group_array(%s) FROM message WHERE %s='%s' ORDER BY %s;`,
			selectExpr, msgSessionCol, escapeSQLiteLiteral(sessionID), order,
		)
		cmd := exec.Command("sqlite3", dbPath, msgQuery)
		if msgOutput, err := cmd.Output(); err == nil {
			transcriptData = []byte(strings.TrimSpace(string(msgOutput)))
		}
	}

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
