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
// It first tries flat file storage (pre-v1.2), then falls back to SQLite (v1.2+).
//
// OpenCode's on-disk layout (project ID scheme, session/message directory
// nesting, and SQLite schema) has changed across releases. Both discovery
// paths below therefore use several increasingly permissive matching
// strategies rather than a single hard-coded layout, and never discard a
// session they did find just because its messages couldn't be located in
// the expected place — a session with an empty transcript still lets a
// manual commit get a note, which is better than silently producing none.
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

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	rootEntries, err := os.ReadDir(sessionRoot)
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout
	var bestSessionID string
	var bestModTime time.Time

	consider := func(id string, modTime time.Time) {
		if now.Sub(modTime) > recentTimeout {
			return
		}
		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID, bestModTime = id, modTime
		}
	}

	for _, entry := range rootEntries {
		if entry.IsDir() {
			// Legacy layout: storage/session/<projectID>/<sessionID>.json.
			// Newer releases may key this directory differently (or not by
			// project at all), so fall back to inspecting each file's own
			// project fields when the directory name isn't our project ID.
			trustDir := entry.Name() == projectID
			dirPath := filepath.Join(sessionRoot, entry.Name())
			subEntries, err := os.ReadDir(dirPath)
			if err != nil {
				continue
			}
			for _, sub := range subEntries {
				if sub.IsDir() || !strings.HasSuffix(sub.Name(), ".json") {
					continue
				}
				fullPath := filepath.Join(dirPath, sub.Name())
				info, err := sub.Info()
				if err != nil {
					continue
				}
				if !trustDir && !sessionFileMatchesProject(fullPath, projectPath, projectID) {
					continue
				}
				consider(strings.TrimSuffix(sub.Name(), ".json"), info.ModTime())
			}
			continue
		}

		// Flat layout: storage/session/<sessionID>.json with project fields inline.
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		fullPath := filepath.Join(sessionRoot, entry.Name())
		if !sessionFileMatchesProject(fullPath, projectPath, projectID) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		consider(strings.TrimSuffix(entry.Name(), ".json"), info.ModTime())
	}

	if bestSessionID == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: readSessionMessages(dataDir, bestSessionID),
	}, nil
}

// sessionFileMatchesProject reports whether a session JSON file belongs to
// the given project, matching on either the projectID field or a recorded
// working directory (which stays meaningful even if OpenCode's project ID
// algorithm changes).
func sessionFileMatchesProject(path, projectPath, projectID string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	var probe struct {
		ProjectID string `json:"projectID"`
		Directory string `json:"directory"`
		Worktree  string `json:"worktree"`
		Cwd       string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}

	if probe.ProjectID != "" && probe.ProjectID == projectID {
		return true
	}

	for _, dir := range []string{probe.Directory, probe.Worktree, probe.Cwd} {
		if dir == "" {
			continue
		}
		cleanDir := filepath.Clean(dir)
		cleanProject := filepath.Clean(projectPath)
		if cleanDir == cleanProject || strings.HasPrefix(cleanDir, cleanProject+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

// readSessionMessages best-effort loads a session's transcript, trying the
// documented message-directory layout first and falling back to a couple of
// plausible alternates. It always returns a valid JSON array (possibly
// empty) so a session that was found is never discarded just because its
// messages couldn't be located.
func readSessionMessages(dataDir, sessionID string) []byte {
	msgDir := filepath.Join(dataDir, "storage", "message", sessionID)

	if entries, err := os.ReadDir(msgDir); err == nil {
		var messages []json.RawMessage
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			data, err := os.ReadFile(filepath.Join(msgDir, name))
			if err != nil {
				continue
			}
			switch {
			case strings.HasSuffix(name, ".jsonl"):
				for _, line := range strings.Split(string(data), "\n") {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					messages = append(messages, json.RawMessage(line))
				}
			case strings.HasSuffix(name, ".json"):
				messages = append(messages, json.RawMessage(data))
			}
		}
		if len(messages) > 0 {
			if encoded, err := json.Marshal(messages); err == nil {
				return encoded
			}
		}
	}

	// Fall back to a single combined file, in case messages are stored one
	// file per session rather than one file per message.
	for _, suffix := range []string{".json", ".jsonl"} {
		if data, err := os.ReadFile(msgDir + suffix); err == nil && len(data) > 0 {
			return data
		}
	}

	return []byte("[]")
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionID := findRecentSQLiteSessionID(dbPath, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	if !sqliteSessionIsRecent(dbPath, sessionID) {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: fetchSQLiteMessages(dbPath, sessionID),
	}, nil
}

// findRecentSQLiteSessionID locates the most relevant session, trying a
// series of increasingly permissive queries since OpenCode's session table
// schema (project scoping column, timestamp column) has changed across
// releases. The final fallbacks order by rowid, which needs no schema
// knowledge beyond the "session" table itself existing.
func findRecentSQLiteSessionID(dbPath, projectID, projectPath string) string {
	queries := []string{
		fmt.Sprintf(`SELECT id FROM session WHERE project_id=%s ORDER BY time_updated DESC LIMIT 1;`, sqlQuoteLiteral(projectID)),
		fmt.Sprintf(`SELECT id FROM session WHERE directory=%s ORDER BY time_updated DESC LIMIT 1;`, sqlQuoteLiteral(projectPath)),
		`SELECT id FROM session ORDER BY time_updated DESC LIMIT 1;`,
		fmt.Sprintf(`SELECT id FROM session WHERE project_id=%s ORDER BY rowid DESC LIMIT 1;`, sqlQuoteLiteral(projectID)),
		`SELECT id FROM session ORDER BY rowid DESC LIMIT 1;`,
	}
	for _, q := range queries {
		if id := querySQLiteScalar(dbPath, q); id != "" {
			return id
		}
	}
	return ""
}

// sqliteSessionIsRecent checks whether a session was updated within
// RecentSessionTimeout. If the timestamp can't be found or parsed, it
// proceeds anyway — better to try than to silently skip a real session.
func sqliteSessionIsRecent(dbPath, sessionID string) bool {
	timeStr := querySQLiteScalar(dbPath, fmt.Sprintf(`SELECT time_updated FROM session WHERE id=%s;`, sqlQuoteLiteral(sessionID)))
	if timeStr == "" {
		return true
	}

	layouts := []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}
	return true
}

// fetchSQLiteMessages best-effort loads a session's messages as a JSON
// array, trying a couple of plausible schemas before giving up. It always
// returns a valid JSON array so a found session is never discarded just
// because its messages couldn't be queried.
func fetchSQLiteMessages(dbPath, sessionID string) []byte {
	primary := fmt.Sprintf(
		`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE session_id=%s ORDER BY time_created;`,
		sqlQuoteLiteral(sessionID),
	)
	if data := sqliteMessagesFromQuery(dbPath, primary); data != nil {
		return data
	}

	// Older/newer schemas may lack time_created, or expose messages without
	// a separately-tracked id column; fall back to raw row order.
	fallback := fmt.Sprintf(`SELECT json_group_array(data) FROM message WHERE session_id=%s;`, sqlQuoteLiteral(sessionID))
	if data := sqliteMessagesFromQuery(dbPath, fallback); data != nil {
		return data
	}

	return []byte("[]")
}

func sqliteMessagesFromQuery(dbPath, query string) []byte {
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	trimmed := strings.TrimSpace(string(output))
	// sqlite3 returns "[null]" when no rows match
	if trimmed == "" || trimmed == "[null]" || trimmed == "[]" {
		return nil
	}
	return []byte(trimmed)
}

func querySQLiteScalar(dbPath, query string) string {
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// sqlQuoteLiteral escapes a string for embedding as a SQL string literal.
func sqlQuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
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
