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
//
// OpenCode's on-disk layout has changed across releases (flat JSON files,
// JSON files nested under a per-project directory keyed by a project ID we
// compute independently, and a SQLite database have all been observed), so
// discovery favors resilience over any single hard-coded schema: it looks
// for whichever session data was touched most recently, within the
// recent-session window, instead of assuming a specific project-scoping
// scheme that may no longer match what OpenCode actually does.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	if session, err := discoverFromFiles(dataDir, projectPath); err == nil && session != nil {
		return session, nil
	}

	return discoverFromSQLite(dataDir, projectPath)
}

// discoverFromFiles scans the OpenCode data directory for the most recently
// modified session JSON file within the recent-session window. It walks the
// whole data directory rather than a single computed path, since OpenCode
// has stored sessions both flat (storage/session/<id>.json) and nested under
// a per-project directory (storage/session/<projectID>/<id>.json).
func discoverFromFiles(dataDir, projectPath string) (*agent.SessionInfo, error) {
	sessionID, modTime := findMostRecentSessionFile(dataDir)
	if sessionID == "" {
		return nil, nil
	}
	if time.Since(modTime) > agent.RecentSessionTimeout {
		return nil, nil
	}

	// The transcript path for OpenCode is the message directory
	msgDir, _ := GetMessageDir(sessionID)

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: msgDir,
		StartedAt:      modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// findMostRecentSessionFile walks dataDir looking for the most recently
// modified "<sessionID>.json" file under any path containing "session".
func findMostRecentSessionFile(dataDir string) (sessionID string, modTime time.Time) {
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		if !strings.Contains(strings.ToLower(path), "session") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		if info.ModTime().After(modTime) {
			modTime = info.ModTime()
			sessionID = strings.TrimSuffix(d.Name(), ".json")
		}
		return nil
	})
	return sessionID, modTime
}

// sessionSchema describes one candidate shape of OpenCode's SQLite session table.
type sessionSchema struct {
	table, idCol, timeCol string
}

// sessionSchemas are tried in order; OpenCode's SQLite table/column naming
// has not been consistent across releases.
var sessionSchemas = []sessionSchema{
	{"session", "id", "time_updated"},
	{"session", "id", "updated_at"},
	{"sessions", "id", "updated_at"},
	{"sessions", "id", "time_updated"},
	{"sessions", "id", "updatedAt"},
}

// messageSchema describes one candidate shape of OpenCode's SQLite message table.
type messageSchema struct {
	table, sessionCol, dataCol, idCol string
}

// messageSchemas are tried in order for the same reason as sessionSchemas.
var messageSchemas = []messageSchema{
	{"message", "session_id", "data", "id"},
	{"messages", "session_id", "data", "id"},
	{"messages", "session_id", "parts", "id"},
	{"message", "sessionID", "data", "id"},
}

// discoverFromSQLite queries OpenCode's SQLite database for the most
// recently updated session, trying several candidate schemas since table
// and column names have changed between OpenCode releases.
func discoverFromSQLite(dataDir, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionID, updatedAt, found := findMostRecentSQLiteSession(dbPath)
	if !found {
		return nil, nil
	}

	// Check if this session was recent (within timeout). If we couldn't
	// determine an update time, proceed anyway — better to try than skip.
	if !updatedAt.IsZero() && time.Since(updatedAt) > agent.RecentSessionTimeout {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: fetchSQLiteSessionMessages(dbPath, sessionID),
	}, nil
}

// findMostRecentSQLiteSession tries each candidate session schema in turn
// and returns the most recently updated session ID from the first one that
// succeeds.
func findMostRecentSQLiteSession(dbPath string) (id string, updatedAt time.Time, found bool) {
	for _, s := range sessionSchemas {
		sessionQuery := fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s DESC LIMIT 1;`, s.idCol, s.table, s.timeCol)
		out, err := runSQLite(dbPath, sessionQuery)
		if err != nil || out == "" {
			continue
		}

		timeQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, s.timeCol, s.table, s.idCol, out)
		if timeOut, err := runSQLite(dbPath, timeQuery); err == nil {
			updatedAt = parseSQLiteTime(timeOut)
		}

		return out, updatedAt, true
	}
	return "", time.Time{}, false
}

// fetchSQLiteSessionMessages tries each candidate message schema in turn and
// returns the first one that yields rows for sessionID, as a JSON array.
// Falls back to an empty array so a session that was found, but whose
// messages couldn't be read under any known schema, still results in a
// (sparse) stored note rather than no note at all.
func fetchSQLiteSessionMessages(dbPath, sessionID string) []byte {
	for _, s := range messageSchemas {
		msgQuery := fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s' ORDER BY rowid;`,
			s.dataCol, s.idCol, s.table, s.sessionCol, sessionID,
		)
		out, err := runSQLite(dbPath, msgQuery)
		if err != nil {
			continue
		}

		// sqlite3 returns "[null]" when no rows match
		if out == "" || out == "[null]" || out == "[]" {
			continue
		}
		return []byte(out)
	}
	return []byte("[]")
}

// runSQLite runs a query against dbPath and returns the trimmed output.
func runSQLite(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// parseSQLiteTime parses a timestamp read from OpenCode's database, which
// has used ISO-8601 strings and Unix epoch integers (seconds, milliseconds,
// and microseconds) across releases.
func parseSQLiteTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}

	formats := []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		switch {
		case n > 1_000_000_000_000_000: // microseconds
			return time.UnixMicro(n)
		case n > 1_000_000_000_000: // milliseconds
			return time.UnixMilli(n)
		case n > 0: // seconds
			return time.Unix(n, 0)
		}
	}

	return time.Time{}
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

