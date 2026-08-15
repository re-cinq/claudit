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
// It first checks the project's git-root-hash-keyed directory (pre-v1.2
// layout). OpenCode has repeatedly changed how project directories are
// keyed across releases (path-based slugs, different hashes, etc), so if
// that lookup finds nothing we fall back to walking the whole storage tree
// and matching session files by their recorded project directory instead
// of trusting a specific naming scheme.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	if info := discoverFromKeyedSessionDir(projectPath); info != nil {
		return info, nil
	}

	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	return scanStorageTreeForSession(filepath.Join(dataDir, "storage"), projectPath), nil
}

// discoverFromKeyedSessionDir looks for sessions under the project's
// git-root-hash-keyed directory (or "global" for non-git projects). This is
// the fast path for OpenCode releases that key project directories this way.
func discoverFromKeyedSessionDir(projectPath string) *agent.SessionInfo {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return nil
	}

	dirEntries, err := os.ReadDir(sessionDir)
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
		ProjectPath:    projectPath,
	}
}

// sessionDirectoryFieldCandidates lists the JSON field names OpenCode has
// used across releases to record a session's working directory.
var sessionDirectoryFieldCandidates = []string{"directory", "worktree", "projectPath", "project_path", "root"}

// scanStorageTreeForSession walks an OpenCode storage tree looking for the
// most recently modified session file whose recorded project directory
// matches projectPath. Matching is done by file content rather than by
// directory naming convention, since OpenCode has changed the project
// directory keying scheme across versions. The message subtree is skipped
// since it only holds per-message files, never session metadata.
func scanStorageTreeForSession(storageRoot, projectPath string) *agent.SessionInfo {
	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout

	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(storageRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == "message" || d.Name() == "messages" || d.Name() == "part" || d.Name() == "parts" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
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
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil
		}
		if !looksLikeSessionFile(raw) {
			return nil
		}
		dir := extractSessionDirectory(raw)
		if dir == "" || !agent.PathsEqual(dir, projectPath) {
			return nil
		}

		sessionID := extractSessionID(raw)
		if sessionID == "" {
			sessionID = strings.TrimSuffix(d.Name(), ".json")
		}

		bestSessionID = sessionID
		bestModTime = modTime
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

// looksLikeSessionFile excludes files that carry message-shaped fields, so
// individual message JSON files (which may embed a "cwd" for a shell tool
// call) aren't mistaken for session metadata.
func looksLikeSessionFile(raw map[string]json.RawMessage) bool {
	for _, key := range []string{"role", "parts", "content"} {
		if _, ok := raw[key]; ok {
			return false
		}
	}
	return true
}

// extractSessionDirectory reads the first matching directory-like field
// from a candidate session JSON object.
func extractSessionDirectory(raw map[string]json.RawMessage) string {
	for _, key := range sessionDirectoryFieldCandidates {
		v, ok := raw[key]
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

// extractSessionID reads the session identifier from a candidate session
// JSON object, trying the field names OpenCode has used across releases.
func extractSessionID(raw map[string]json.RawMessage) string {
	for _, key := range []string{"id", "sessionID", "sessionId"} {
		v, ok := raw[key]
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
