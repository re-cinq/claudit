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
// It first tries flat file storage (very old OpenCode versions), then falls
// back to the per-project SQLite database (`<project>/.opencode/opencode.db`)
// used by current OpenCode releases. Unlike the legacy flat-file layout,
// OpenCode's SQLite database lives inside the project directory itself, so
// there is no separate project identifier to look up or filter by.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (legacy OpenCode)
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to the project-local SQLite database used by current OpenCode.
	return discoverFromSQLite(projectPath)
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

// discoverFromSQLite queries OpenCode's per-project SQLite database
// (<project>/.opencode/opencode.db) for the most recently updated session
// and its messages, and builds inline transcript data from the result.
//
// Schema (current OpenCode releases):
//
//	CREATE TABLE sessions (
//	    id TEXT PRIMARY KEY, title TEXT NOT NULL,
//	    message_count INTEGER NOT NULL DEFAULT 0,
//	    updated_at INTEGER NOT NULL, created_at INTEGER NOT NULL
//	);
//	CREATE TABLE messages (
//	    id TEXT PRIMARY KEY, session_id TEXT NOT NULL, role TEXT NOT NULL,
//	    parts TEXT NOT NULL DEFAULT '[]', model TEXT, created_at INTEGER NOT NULL,
//	    FOREIGN KEY (session_id) REFERENCES sessions(id)
//	);
//
// There is no project identifier column: the database itself is already
// scoped to a single project directory.
func discoverFromSQLite(projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(projectPath, ".opencode", "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	// Find the most recently updated session in this project's database.
	cmd := exec.Command("sqlite3", "-json", dbPath,
		"SELECT id, updated_at FROM sessions ORDER BY updated_at DESC LIMIT 1;")
	sessionOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	var sessions []struct {
		ID        string `json:"id"`
		UpdatedAt int64  `json:"updated_at"`
	}
	if err := json.Unmarshal(sessionOutput, &sessions); err != nil || len(sessions) == 0 {
		return nil, nil
	}
	session := sessions[0]

	if time.Since(sqliteEpochToTime(session.UpdatedAt)) > agent.RecentSessionTimeout {
		return nil, nil
	}

	// Fetch messages for this session, ordered by creation time.
	msgQuery := fmt.Sprintf(
		`SELECT id, role, parts, created_at FROM messages WHERE session_id='%s' ORDER BY created_at;`,
		sqliteEscape(session.ID),
	)
	cmd = exec.Command("sqlite3", "-json", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	var rows []struct {
		ID        string          `json:"id"`
		Role      string          `json:"role"`
		Parts     json.RawMessage `json:"parts"`
		CreatedAt int64           `json:"created_at"`
	}
	if err := json.Unmarshal(msgOutput, &rows); err != nil || len(rows) == 0 {
		return nil, nil
	}

	entries := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, map[string]interface{}{
			"id":      row.ID,
			"role":    row.Role,
			"content": partsToContentBlocks(row.Parts),
			"time": map[string]interface{}{
				"created": sqliteEpochToTime(row.CreatedAt).Format(time.RFC3339),
			},
		})
	}

	transcriptData, err := json.Marshal(entries)
	if err != nil {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      session.ID,
		TranscriptPath: "", // transcript comes from inline TranscriptData
		StartedAt:      sqliteEpochToTime(session.UpdatedAt).Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// sqliteEpochToTime converts an OpenCode SQLite timestamp column (Unix
// seconds or milliseconds) into a time.Time.
func sqliteEpochToTime(epoch int64) time.Time {
	if epoch > 1e12 {
		return time.UnixMilli(epoch)
	}
	return time.Unix(epoch, 0)
}

// sqliteEscape escapes single quotes for safe inline use in a SQLite string literal.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// openCodePart represents one typed entry in an OpenCode message's `parts` array
// (e.g. {"type":"text","data":{"text":"..."}}).
type openCodePart struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// partsToContentBlocks converts OpenCode's typed `parts` array (text, tool_call,
// tool_result, reasoning, finish) into the common ContentBlock shape consumed
// by parseOpenCodeMessage.
func partsToContentBlocks(raw json.RawMessage) []map[string]interface{} {
	blocks := make([]map[string]interface{}, 0)

	var parts []openCodePart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return blocks
	}

	for _, part := range parts {
		switch part.Type {
		case "text":
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(part.Data, &d) == nil && d.Text != "" {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": d.Text})
			}
		case "reasoning":
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(part.Data, &d) == nil && d.Text != "" {
				blocks = append(blocks, map[string]interface{}{"type": "thinking", "thinking": d.Text})
			}
		case "tool_call":
			var d struct {
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if json.Unmarshal(part.Data, &d) == nil {
				blocks = append(blocks, map[string]interface{}{
					"type": "tool_use", "id": d.ID, "name": d.Name, "input": d.Input,
				})
			}
		case "tool_result":
			var d struct {
				ID      string          `json:"id"`
				Content json.RawMessage `json:"content"`
				Output  json.RawMessage `json:"output"`
				Result  json.RawMessage `json:"result"`
			}
			if json.Unmarshal(part.Data, &d) == nil {
				content := d.Content
				if len(content) == 0 {
					content = d.Output
				}
				if len(content) == 0 {
					content = d.Result
				}
				blocks = append(blocks, map[string]interface{}{
					"type": "tool_result", "tool_use_id": d.ID, "content": content,
				})
			}
		}
		// "finish" and any unrecognized part types carry no displayable content.
	}

	return blocks
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
