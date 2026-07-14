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
// It first tries flat file storage (pre-v1.2), then falls back to SQLite (v1.2+),
// then falls back to a broad, version-tolerant scan for newer releases that have
// moved the on-disk layout again.
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
	if err == nil {
		if sqliteSession, sqliteErr := discoverFromSQLite(dataDir, GetProjectID(projectPath), projectPath); sqliteErr == nil && sqliteSession != nil {
			return sqliteSession, nil
		}
	}

	// Newer OpenCode releases have kept shifting the on-disk layout (dropping
	// the per-project subdirectory we key sessions by, renaming SQLite
	// columns, etc.), so neither guess above may match. As a last resort,
	// scan broadly for the most recently active session and match it against
	// this project by whatever directory/project fields it actually carries,
	// instead of only trusting the exact paths/columns assumed above.
	return a.discoverBroad(projectPath)
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

	// Find most recent session for this project
	sessionQuery := fmt.Sprintf(
		`SELECT id FROM session WHERE project_id='%s' ORDER BY time_updated DESC LIMIT 1;`,
		projectID,
	)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Check if this session was recent (within timeout)
	timeQuery := fmt.Sprintf(
		`SELECT time_updated FROM session WHERE id='%s';`,
		sessionID,
	)
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
		}
		// If we can't parse the time, proceed anyway — better to try than skip
	}

	// Get messages for this session as a JSON array
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE session_id='%s' ORDER BY time_created;`,
		sessionID,
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

// discoverBroad is a best-effort fallback used when neither the legacy
// flat-file layout nor the previously-validated SQLite schema turn up a
// session. OpenCode's storage layout has changed across versions before
// (flat files -> SQLite, and possibly further since), so instead of trusting
// one exact path/column layout, this scans everything available and matches
// sessions by their own recorded directory/project fields, falling back to
// simply the most recently active session when no such field is found
// (correct in the common case of one OpenCode session per checkout, which is
// how the post-commit hook that calls this always runs).
func (a *Agent) discoverBroad(projectPath string) (*agent.SessionInfo, error) {
	absProjectPath, err := filepath.Abs(projectPath)
	if err != nil {
		absProjectPath = projectPath
	}
	projectID := GetProjectID(projectPath)

	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	if best := latestSessionFile(filepath.Join(dataDir, "storage", "session"), absProjectPath, projectID); best != "" {
		if info, err := flatFileSessionInfo(best, projectPath); err == nil && info != nil {
			return info, nil
		}
	}

	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionID := findSessionIDInSQLite(dbPath, projectID, absProjectPath)
	if sessionID == "" {
		return nil, nil
	}

	transcriptData := findMessagesInSQLite(dbPath, sessionID)
	if transcriptData == nil {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "",
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// latestSessionFile scans dir (and one level of subdirectories) for the most
// recently modified *.json session file within the recent-session timeout.
// When matchDirectory or matchProjectID is non-empty, only files whose own
// recorded directory/project fields match are considered.
func latestSessionFile(dir, matchDirectory, matchProjectID string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	now := time.Now()
	var best string
	var bestMod time.Time

	consider := func(path, name string, modTime time.Time) {
		if !strings.HasSuffix(name, ".json") {
			return
		}
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return
		}
		_, directory, projectID := readSessionMeta(path)
		dirMatches := directory != "" && filepath.Clean(directory) == matchDirectory
		idMatches := projectID != "" && projectID == matchProjectID
		if !dirMatches && !idMatches {
			return
		}
		if best == "" || modTime.After(bestMod) {
			best = path
			bestMod = modTime
		}
	}

	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		if e.IsDir() {
			subEntries, err := os.ReadDir(full)
			if err != nil {
				continue
			}
			for _, se := range subEntries {
				if se.IsDir() {
					continue
				}
				info, err := se.Info()
				if err != nil {
					continue
				}
				consider(filepath.Join(full, se.Name()), se.Name(), info.ModTime())
			}
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}
		consider(full, e.Name(), info.ModTime())
	}

	return best
}

// readSessionMeta reads a session JSON file's id, directory and project ID,
// trying several field names since these have varied across OpenCode
// releases (e.g. "directory" vs "cwd" vs "path", "projectID" vs "project_id").
func readSessionMeta(path string) (id, directory, projectID string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", ""
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", "", ""
	}
	id = firstStringField(raw, "id")
	directory = firstStringField(raw, "directory", "cwd", "path", "worktree")
	projectID = firstStringField(raw, "projectID", "project_id")
	return id, directory, projectID
}

// firstStringField returns the first non-empty string value found under any
// of the given keys.
func firstStringField(raw map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil && s != "" {
			return s
		}
	}
	return ""
}

// flatFileSessionInfo builds a SessionInfo from a matched session JSON file.
func flatFileSessionInfo(sessionFilePath, projectPath string) (*agent.SessionInfo, error) {
	info, err := os.Stat(sessionFilePath)
	if err != nil {
		return nil, nil
	}

	id, _, _ := readSessionMeta(sessionFilePath)
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(sessionFilePath), ".json")
	}

	msgDir, _ := GetMessageDir(id)

	return &agent.SessionInfo{
		SessionID:      id,
		TranscriptPath: msgDir,
		StartedAt:      info.ModTime().Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// findSessionIDInSQLite locates the most recent session for a project. The
// SQLite schema has changed across OpenCode releases, so several plausible
// ways of scoping "session belongs to this project" are tried before
// falling back to simply the most recently updated session in the database
// (correct in the common single-project-per-checkout case).
func findSessionIDInSQLite(dbPath, projectID, absProjectPath string) string {
	candidates := []string{
		fmt.Sprintf(`SELECT id FROM session WHERE project_id='%s' ORDER BY time_updated DESC LIMIT 1;`, sqlEscape(projectID)),
		fmt.Sprintf(`SELECT id FROM session WHERE directory='%s' ORDER BY time_updated DESC LIMIT 1;`, sqlEscape(absProjectPath)),
		fmt.Sprintf(`SELECT id FROM session WHERE json_extract(data, '$.projectID')='%s' ORDER BY time_updated DESC LIMIT 1;`, sqlEscape(projectID)),
		fmt.Sprintf(`SELECT id FROM session WHERE json_extract(data, '$.directory')='%s' ORDER BY time_updated DESC LIMIT 1;`, sqlEscape(absProjectPath)),
		`SELECT id FROM session ORDER BY time_updated DESC LIMIT 1;`,
		`SELECT id FROM session ORDER BY rowid DESC LIMIT 1;`,
	}

	for _, q := range candidates {
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, q)
		output, err := cmd.Output()
		if err != nil {
			continue
		}
		id := strings.TrimSpace(string(output))
		if id != "" {
			return id
		}
	}

	return ""
}

// findMessagesInSQLite reads all messages for a session as a JSON array.
// The message table's column names have varied across OpenCode releases,
// so a few plausible shapes are tried before giving up.
func findMessagesInSQLite(dbPath, sessionID string) []byte {
	queries := []string{
		fmt.Sprintf(`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE session_id='%s' ORDER BY time_created;`, sqlEscape(sessionID)),
		fmt.Sprintf(`SELECT json_group_array(json_patch(parts, json_object('id', id))) FROM message WHERE session_id='%s' ORDER BY time_created;`, sqlEscape(sessionID)),
		fmt.Sprintf(`SELECT json_group_array(json_object('id', id, 'role', role, 'content', data)) FROM message WHERE session_id='%s';`, sqlEscape(sessionID)),
	}

	for _, q := range queries {
		cmd := exec.Command("sqlite3", dbPath, q)
		output, err := cmd.Output()
		if err != nil {
			continue
		}
		transcriptData := []byte(strings.TrimSpace(string(output)))
		if len(transcriptData) == 0 || string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
			continue
		}
		return transcriptData
	}

	return nil
}

// sqlEscape escapes single quotes for safe inlining into a sqlite3 CLI
// statement (values here are git commit hashes and filesystem paths, not
// user-controlled query strings).
func sqlEscape(s string) string {
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
