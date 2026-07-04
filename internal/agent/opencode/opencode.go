package opencode

import (
	"encoding/json"
	"errors"
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
//
// OpenCode's storage backend has shifted across versions (flat JSON files
// under storage/session/<projectID>, a SQLite database, or a hybrid of both
// mid-migration), so this treats "find the session ID" and "load that
// session's messages" as independent problems. The session ID is resolved
// from whichever index is available (flat files first, then SQLite), and
// message data is then resolved from whichever backend actually holds it —
// which is not necessarily the same backend that produced the session ID.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)

	sessionID, startedAt := findRecentSessionID(dataDir, projectID)
	if sessionID == "" {
		return nil, nil
	}

	transcriptData, transcriptPath := resolveTranscript(dataDir, sessionID)
	if len(transcriptData) == 0 && transcriptPath == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
		StartedAt:      startedAt,
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// findRecentSessionID locates the most recently active session for a
// project, trying OpenCode's flat file session index first, then its
// SQLite index. Returns an empty sessionID if neither has a recent match.
func findRecentSessionID(dataDir, projectID string) (sessionID, startedAt string) {
	if id, t, ok := findRecentFlatSessionID(dataDir, projectID); ok {
		return id, t.Format(time.RFC3339)
	}

	if id, t, ok := findRecentSQLiteSessionID(dataDir, projectID); ok {
		return id, t
	}

	return "", ""
}

// findRecentFlatSessionID scans dataDir/storage/session/<projectID> for the
// most recently modified session file within the recent-session window.
func findRecentFlatSessionID(dataDir, projectID string) (string, time.Time, bool) {
	sessionDir := filepath.Join(dataDir, "storage", "session", projectID)

	dirEntries, err := os.ReadDir(sessionDir)
	if err != nil {
		return "", time.Time{}, false
	}

	now := time.Now()
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
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			continue
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = strings.TrimSuffix(entry.Name(), ".json")
			bestModTime = modTime
		}
	}

	if bestSessionID == "" {
		return "", time.Time{}, false
	}
	return bestSessionID, bestModTime, true
}

// findRecentSQLiteSessionID queries OpenCode's SQLite index (used since
// roughly v1.2) for the most recently updated session belonging to
// projectID. The project-scoping column name has shifted across releases
// (project_id / projectID / directory), so each is tried in turn.
func findRecentSQLiteSessionID(dataDir, projectID string) (string, string, bool) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return "", "", false
	}

	if _, err := exec.LookPath("sqlite3"); err != nil {
		return "", "", false
	}

	projectColumns := []string{"project_id", "projectID", "directory"}
	for _, col := range projectColumns {
		sessionQuery := fmt.Sprintf(
			`SELECT id FROM session WHERE %s='%s' ORDER BY time_updated DESC LIMIT 1;`,
			col, projectID,
		)
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
		sessionOutput, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
			continue
		}
		sessionID := strings.TrimSpace(string(sessionOutput))

		startedAt := time.Now().Format(time.RFC3339)

		// Check if this session was recent (within timeout). If the time
		// can't be read or parsed, proceed anyway — better to try than skip.
		timeQuery := fmt.Sprintf(`SELECT time_updated FROM session WHERE id='%s';`, sessionID)
		cmd = exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			timeStr := strings.TrimSpace(string(timeOutput))
			recent := true
			if t, err := time.Parse(time.RFC3339Nano, timeStr); err == nil {
				recent = time.Since(t) <= agent.RecentSessionTimeout
			} else if t, err := time.Parse("2006-01-02T15:04:05.000Z", timeStr); err == nil {
				recent = time.Since(t) <= agent.RecentSessionTimeout
			} else if t, err := time.Parse("2006-01-02 15:04:05", timeStr); err == nil {
				recent = time.Since(t) <= agent.RecentSessionTimeout
			}
			if !recent {
				continue
			}
		}

		return sessionID, startedAt, true
	}

	return "", "", false
}

// resolveTranscript loads the message data for a session, trying OpenCode's
// SQLite message table first (the newer storage backend), then the flat
// message directory convention, then a broad recursive search for a
// directory named after the session ID anywhere under storage. This is
// deliberately independent of how the session ID itself was discovered,
// since the index that names a session and the store that holds its
// messages can be on different backends mid-migration.
func resolveTranscript(dataDir, sessionID string) (transcriptData []byte, transcriptPath string) {
	if data, ok := messagesFromSQLite(dataDir, sessionID); ok {
		return data, ""
	}

	msgDir := filepath.Join(dataDir, "storage", "message", sessionID)
	if hasJSONEntries(msgDir) {
		return nil, msgDir
	}

	if dir := findDirNamed(filepath.Join(dataDir, "storage"), sessionID); dir != "" && hasJSONEntries(dir) {
		return nil, dir
	}

	return nil, ""
}

// messagesFromSQLite fetches all messages for a session from OpenCode's
// SQLite database as a JSON array. The session-linking column name has
// shifted across releases (session_id / sessionID / session), so each is
// tried in turn.
func messagesFromSQLite(dataDir, sessionID string) ([]byte, bool) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, false
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, false
	}

	sessionColumns := []string{"session_id", "sessionID", "session"}
	for _, col := range sessionColumns {
		msgQuery := fmt.Sprintf(
			`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE %s='%s' ORDER BY time_created;`,
			col, sessionID,
		)
		cmd := exec.Command("sqlite3", dbPath, msgQuery)
		msgOutput, err := cmd.Output()
		if err != nil {
			continue
		}

		data := []byte(strings.TrimSpace(string(msgOutput)))
		// sqlite3 returns "[null]" when no rows match
		if len(data) == 0 || string(data) == "[null]" || string(data) == "[]" {
			continue
		}

		return data, true
	}

	return nil, false
}

// hasJSONEntries reports whether dir exists and contains at least one
// .json or .jsonl file.
func hasJSONEntries(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

// findDirNamed recursively searches root for a directory literally named
// target, returning its path if found. This is a last-resort fallback for
// when OpenCode restructures its storage layout in a way the known
// conventions (storage/message/<sessionID>) no longer match.
func findDirNamed(root, target string) string {
	stop := errors.New("stop")
	var found string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() && d.Name() == target {
			found = path
			return stop
		}
		return nil
	})
	_ = err

	return found
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
