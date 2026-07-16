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
// OpenCode's on-disk storage layout has changed across versions, so we try
// each known layout in turn:
//  1. pre-v1.2: flat files nested in a per-project directory named after the
//     project ID (storage/session/<projectID>/<sessionID>.json)
//  2. newer releases: sessions stored together rather than nested per
//     project, identified instead by a field inside the session JSON
//     (storage/session/info/*.json or storage/session/*.json)
//  3. v1.2.x: SQLite (opencode.db)
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	session, err = a.discoverFromUnnestedFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite (OpenCode v1.2.x)
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
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

// discoverFromUnnestedFlatFiles handles newer OpenCode releases that store
// all sessions together under storage/session (optionally storage/session/info)
// rather than nested in a per-project subdirectory. The owning project is
// instead recorded as a field inside the session's JSON payload, so we scan
// for the most recently modified session file whose project field matches
// this project.
func (a *Agent) discoverFromUnnestedFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	resolvedPath := projectPath
	if r, err := filepath.EvalSymlinks(projectPath); err == nil {
		resolvedPath = r
	}

	var candidateFiles []string
	for _, dir := range []string{
		filepath.Join(dataDir, "storage", "session", "info"),
		filepath.Join(dataDir, "storage", "session"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			candidateFiles = append(candidateFiles, filepath.Join(dir, e.Name()))
		}
		if len(candidateFiles) > 0 {
			break
		}
	}

	now := time.Now()
	var bestID string
	var bestModTime time.Time

	for _, path := range candidateFiles {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			continue
		}
		if bestID != "" && !modTime.After(bestModTime) {
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var meta map[string]interface{}
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		if !sessionMatchesProject(meta, projectID, projectPath, resolvedPath) {
			continue
		}

		id, _ := meta["id"].(string)
		if id == "" {
			id = strings.TrimSuffix(filepath.Base(path), ".json")
		}
		bestID = id
		bestModTime = modTime
	}

	if bestID == "" {
		return nil, nil
	}

	sessionInfo := &agent.SessionInfo{
		SessionID:   bestID,
		StartedAt:   bestModTime.Format(time.RFC3339),
		ProjectPath: projectPath,
	}

	// Inline the transcript directly so the caller doesn't have to guess the
	// message directory layout, which has also changed across versions.
	if data := readOpenCodeMessages(dataDir, bestID); len(data) > 0 {
		sessionInfo.TranscriptData = data
	} else {
		msgDir, _ := GetMessageDir(bestID)
		sessionInfo.TranscriptPath = msgDir
	}

	return sessionInfo, nil
}

// sessionMatchesProject checks whether a session's JSON metadata identifies
// it as belonging to the given project, trying the field names OpenCode has
// used across versions to record project identity.
func sessionMatchesProject(meta map[string]interface{}, projectID, projectPath, resolvedPath string) bool {
	for _, key := range []string{"projectID", "project_id", "directory", "worktree", "cwd", "path"} {
		v, ok := meta[key].(string)
		if !ok || v == "" {
			continue
		}
		if v == projectID || v == projectPath || v == resolvedPath {
			return true
		}
	}
	return false
}

// readOpenCodeMessages reads a session's messages from any known message
// storage location and returns them combined as a JSON array, or nil if none
// were found. Checked locations cover both the legacy storage/message/<id>
// layout and the newer storage/session/message/<id> layout.
func readOpenCodeMessages(dataDir, sessionID string) []byte {
	for _, dir := range []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) == 0 {
			continue
		}

		var messages []json.RawMessage
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			messages = append(messages, json.RawMessage(data))
		}
		if len(messages) == 0 {
			continue
		}
		out, err := json.Marshal(messages)
		if err != nil {
			continue
		}
		return out
	}
	return nil
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

// discoverFromUnnestedFlatFiles handles newer OpenCode releases that store
// all sessions together under storage/session (optionally storage/session/info)
// rather than nested in a per-project subdirectory. The owning project is
// instead recorded as a field inside the session's JSON payload, so we scan
// for the most recently modified session file whose project field matches
// this project.
func (a *Agent) discoverFromUnnestedFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	resolvedPath := projectPath
	if r, err := filepath.EvalSymlinks(projectPath); err == nil {
		resolvedPath = r
	}

	var candidateFiles []string
	for _, dir := range []string{
		filepath.Join(dataDir, "storage", "session", "info"),
		filepath.Join(dataDir, "storage", "session"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			candidateFiles = append(candidateFiles, filepath.Join(dir, e.Name()))
		}
		if len(candidateFiles) > 0 {
			break
		}
	}

	now := time.Now()
	var bestID string
	var bestModTime time.Time

	for _, path := range candidateFiles {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			continue
		}
		if bestID != "" && !modTime.After(bestModTime) {
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var meta map[string]interface{}
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		if !sessionMatchesProject(meta, projectID, projectPath, resolvedPath) {
			continue
		}

		id, _ := meta["id"].(string)
		if id == "" {
			id = strings.TrimSuffix(filepath.Base(path), ".json")
		}
		bestID = id
		bestModTime = modTime
	}

	if bestID == "" {
		return nil, nil
	}

	sessionInfo := &agent.SessionInfo{
		SessionID:   bestID,
		StartedAt:   bestModTime.Format(time.RFC3339),
		ProjectPath: projectPath,
	}

	// Inline the transcript directly so the caller doesn't have to guess the
	// message directory layout, which has also changed across versions.
	if data := readOpenCodeMessages(dataDir, bestID); len(data) > 0 {
		sessionInfo.TranscriptData = data
	} else {
		msgDir, _ := GetMessageDir(bestID)
		sessionInfo.TranscriptPath = msgDir
	}

	return sessionInfo, nil
}

// sessionMatchesProject checks whether a session's JSON metadata identifies
// it as belonging to the given project, trying the field names OpenCode has
// used across versions to record project identity.
func sessionMatchesProject(meta map[string]interface{}, projectID, projectPath, resolvedPath string) bool {
	for _, key := range []string{"projectID", "project_id", "directory", "worktree", "cwd", "path"} {
		v, ok := meta[key].(string)
		if !ok || v == "" {
			continue
		}
		if v == projectID || v == projectPath || v == resolvedPath {
			return true
		}
	}
	return false
}

// readOpenCodeMessages reads a session's messages from any known message
// storage location and returns them combined as a JSON array, or nil if none
// were found. Checked locations cover both the legacy storage/message/<id>
// layout and the newer storage/session/message/<id> layout.
func readOpenCodeMessages(dataDir, sessionID string) []byte {
	for _, dir := range []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) == 0 {
			continue
		}

		var messages []json.RawMessage
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			messages = append(messages, json.RawMessage(data))
		}
		if len(messages) == 0 {
			continue
		}
		out, err := json.Marshal(messages)
		if err != nil {
			continue
		}
		return out
	}
	return nil
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

	// Try "parts" array (newer OpenCode message format: typed parts such as
	// {"type": "text", "text": "..."} or {"type": "text", "data": {"text": "..."}}).
	if partsRaw, ok := raw["parts"]; ok {
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(partsRaw, &parts); err == nil && len(parts) > 0 {
			var blocks []agent.ContentBlock
			for _, part := range parts {
				var ptype string
				if t, ok := part["type"]; ok {
					_ = json.Unmarshal(t, &ptype)
				}
				if ptype != "" && ptype != "text" {
					continue
				}
				if textRaw, ok := part["text"]; ok {
					var text string
					if json.Unmarshal(textRaw, &text) == nil && text != "" {
						blocks = append(blocks, agent.ContentBlock{Type: "text", Text: text})
						continue
					}
				}
				if dataRaw, ok := part["data"]; ok {
					var d struct {
						Text string `json:"text"`
					}
					if json.Unmarshal(dataRaw, &d) == nil && d.Text != "" {
						blocks = append(blocks, agent.ContentBlock{Type: "text", Text: d.Text})
					}
				}
			}
			if len(blocks) > 0 {
				msg.Content = blocks
				return msg
			}
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
