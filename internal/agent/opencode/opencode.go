```go
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
// OpenCode's on-disk layout for flat file storage has changed across
// releases: older builds nest sessions under a per-project directory keyed
// by a project ID we reimplement locally (fragile whenever OpenCode's own
// derivation changes), while other builds store sessions flat under
// storage/session and record the scoping info (working directory / project
// ID) inside each session's JSON payload instead. Both layouts are tried.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)

	// Layout 1: storage/session/<projectID>/<sessionID>.json
	perProjectDir := filepath.Join(dataDir, "storage", "session", projectID)
	if id, modTime, ok := latestRecentSession(perProjectDir, nil); ok {
		msgDir, _ := GetMessageDir(id)
		return &agent.SessionInfo{
			SessionID:      id,
			TranscriptPath: msgDir,
			StartedAt:      modTime.Format(time.RFC3339),
			ProjectPath:    projectPath,
		}, nil
	}

	// Layout 2: storage/session/<sessionID>.json, scoped by a "directory" or
	// "projectID" field inside the file rather than by directory structure.
	flatDir := filepath.Join(dataDir, "storage", "session")
	matches := func(raw map[string]json.RawMessage) bool {
		if dirRaw, ok := raw["directory"]; ok {
			var dir string
			if json.Unmarshal(dirRaw, &dir) == nil && dir == projectPath {
				return true
			}
		}
		if pidRaw, ok := raw["projectID"]; ok {
			var pid string
			if json.Unmarshal(pidRaw, &pid) == nil && pid == projectID {
				return true
			}
		}
		return false
	}
	if id, modTime, ok := latestRecentSession(flatDir, matches); ok {
		msgDir, _ := GetMessageDir(id)
		return &agent.SessionInfo{
			SessionID:      id,
			TranscriptPath: msgDir,
			StartedAt:      modTime.Format(time.RFC3339),
			ProjectPath:    projectPath,
		}, nil
	}

	return nil, nil
}

// latestRecentSession returns the id (filename without .json) of the most
// recently modified *.json file in dir that was modified within
// agent.RecentSessionTimeout. If matches is non-nil, only files whose parsed
// JSON content satisfies it are considered.
func latestRecentSession(dir string, matches func(map[string]json.RawMessage) bool) (string, time.Time, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}, false
	}

	now := time.Now()
	var bestID string
	var bestModTime time.Time

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

		if matches != nil {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				continue
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil || !matches(raw) {
				continue
			}
		}

		if bestID == "" || modTime.After(bestModTime) {
			bestID = strings.TrimSuffix(entry.Name(), ".json")
			bestModTime = modTime
		}
	}

	if bestID == "" {
		return "", time.Time{}, false
	}
	return bestID, bestModTime, true
}

// findOpenCodeDB locates OpenCode's SQLite database file. The filename has
// not been stable across releases (e.g. "opencode.db" vs "db.sqlite"), so a
// handful of known names are tried before falling back to any single *.db
// or *.sqlite file found directly under dataDir.
func findOpenCodeDB(dataDir string) string {
	for _, name := range []string{"opencode.db", "db.sqlite", "sqlite.db"} {
		p := filepath.Join(dataDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	for _, pattern := range []string{"*.db", "*.sqlite"} {
		if matches, _ := filepath.Glob(filepath.Join(dataDir, pattern)); len(matches) > 0 {
			return matches[0]
		}
	}

	return ""
}

// sqliteColumns returns the set of column names for a table via PRAGMA
// table_info, so callers can adapt to schema changes instead of assuming
// fixed column names.
func sqliteColumns(dbPath, table string) (map[string]bool, error) {
	cmd := exec.Command("sqlite3", "-json", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(output, &rows); err != nil {
		return nil, err
	}

	cols := make(map[string]bool, len(rows))
	for _, row := range rows {
		cols[row.Name] = true
	}
	return cols, nil
}

// firstColumn returns the first candidate present in cols, or "" if none are.
func firstColumn(cols map[string]bool, candidates ...string) string {
	for _, c := range candidates {
		if cols[c] {
			return c
		}
	}
	return ""
}

// sqliteEscape escapes a string for safe interpolation into a single-quoted
// SQLite string literal.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
// OpenCode's SQLite schema (column names, scoping strategy, DB filename) has
// changed across releases, so the schema is introspected at runtime and the
// lookup degrades gracefully: prefer scoping sessions by their working
// directory, fall back to a reimplemented project ID, and finally fall back
// to the most recently updated session overall (bounded by the recency
// check below) rather than failing outright when an expected column or file
// is missing.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := findOpenCodeDB(dataDir)
	if dbPath == "" {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	cols, err := sqliteColumns(dbPath, "session")
	if err != nil || len(cols) == 0 {
		return nil, nil
	}

	timeCol := firstColumn(cols, "time_updated", "updated_at", "timeUpdated", "time_created", "created_at")
	orderCol := timeCol
	if orderCol == "" {
		orderCol = "rowid"
	}

	var whereClause string
	if dirCol := firstColumn(cols, "directory", "cwd", "path", "worktree"); dirCol != "" {
		whereClause = fmt.Sprintf("WHERE %s='%s'", dirCol, sqliteEscape(projectPath))
	} else if idCol := firstColumn(cols, "project_id", "projectID", "project"); idCol != "" {
		whereClause = fmt.Sprintf("WHERE %s='%s'", idCol, sqliteEscape(projectID))
	}

	// Find most recent session matching the best available scope
	sessionQuery := fmt.Sprintf(
		`SELECT id FROM session %s ORDER BY %s DESC LIMIT 1;`,
		whereClause, orderCol,
	)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Check if this session was recent (within timeout)
	if timeCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM session WHERE id='%s';`, timeCol, sqliteEscape(sessionID))
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
			} else if ms, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
				// Some releases store epoch milliseconds instead of a formatted string.
				t := time.UnixMilli(ms)
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
			// If we can't parse the time, proceed anyway — better to try than skip
		}
	}

	// Get messages for this session as a JSON array
	msgCols, err := sqliteColumns(dbPath, "message")
	sessionFK := "session_id"
	msgOrderCol := "time_created"
	if err == nil && len(msgCols) > 0 {
		if fk := firstColumn(msgCols, "session_id", "sessionID", "session"); fk != "" {
			sessionFK = fk
		}
		if oc := firstColumn(msgCols, "time_created", "created_at", "time", "timeCreated"); oc != "" {
			msgOrderCol = oc
		} else {
			msgOrderCol = "rowid"
		}
	}

	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE %s='%s' ORDER BY %s;`,
		sessionFK, sqliteEscape(sessionID), msgOrderCol,
	)
	cmd = exec.Command("sqlite3", dbPath, msgQuery)
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
