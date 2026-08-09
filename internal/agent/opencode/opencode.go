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

// DiscoverSession finds the most recent OpenCode session for the project.
//
// OpenCode (github.com/opencode-ai/opencode, the Go-based CLI — not to be
// confused with the unrelated sst/opencode TUI) keeps no flat session/message
// files anywhere, and has no home-directory data dir. All session and message
// data lives in a single per-project SQLite database at
// <projectPath>/.opencode/opencode.db, written by the CLI itself.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	return discoverFromSQLite(projectPath)
}

// discoverFromSQLite queries OpenCode's per-project SQLite database
// (<projectPath>/.opencode/opencode.db) for the most recently updated
// session and its messages.
//
// Schema (see internal/db/migrations/20250424200609_initial.sql upstream):
//
//	CREATE TABLE sessions (
//	    id TEXT PRIMARY KEY,
//	    title TEXT NOT NULL,
//	    message_count INTEGER NOT NULL DEFAULT 0,
//	    updated_at INTEGER NOT NULL,
//	    created_at INTEGER NOT NULL
//	);
//	CREATE TABLE messages (
//	    id TEXT PRIMARY KEY,
//	    session_id TEXT NOT NULL,
//	    role TEXT NOT NULL,
//	    parts TEXT NOT NULL DEFAULT '[]',
//	    model TEXT,
//	    created_at INTEGER NOT NULL,
//	    FOREIGN KEY (session_id) REFERENCES sessions(id)
//	);
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
	sessionQuery := `SELECT id, updated_at FROM sessions ORDER BY updated_at DESC LIMIT 1;`
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}

	fields := strings.SplitN(strings.TrimSpace(string(sessionOutput)), "\t", 2)
	sessionID := fields[0]
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recently updated (within timeout).
	if len(fields) == 2 {
		if updatedAt, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64); err == nil {
			if time.Since(unixToTime(updatedAt)) > agent.RecentSessionTimeout {
				return nil, nil
			}
		}
		// If we can't parse the timestamp, proceed anyway — better to try than skip.
	}

	// Fetch messages for this session as a JSON array, preserving each
	// message's role, id, parts (already JSON) and created_at.
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_object('role', role, 'id', id, 'parts', json(parts), 'created_at', created_at)) FROM messages WHERE session_id='%s' ORDER BY created_at;`,
		escapeSQLiteLiteral(sessionID),
	)
	cmd = exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if string(transcriptData) == "[null]" || string(transcriptData) == "[]" || len(transcriptData) == 0 {
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

// escapeSQLiteLiteral escapes single quotes for safe inclusion in a
// single-quoted SQLite string literal.
func escapeSQLiteLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// unixToTime converts a unix timestamp that may be expressed in seconds or
// milliseconds (OpenCode's INTEGER columns aren't documented either way) into
// a time.Time, guessing the unit from its magnitude.
func unixToTime(v int64) time.Time {
	// A seconds-based timestamp for "now" is ~1.8e9; a milliseconds-based one
	// is ~1.8e12. Anything above 1e12 is treated as milliseconds.
	if v > 1_000_000_000_000 {
		return time.UnixMilli(v)
	}
	return time.Unix(v, 0)
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

	// Parse timestamp: either a nested {"time":{"created":...}} object (used
	// by the plugin/JSONL path) or a flat "created_at" unix timestamp (used
	// by the SQLite discovery path).
	if timeRaw, ok := raw["time"]; ok {
		var timeObj struct {
			Created string `json:"created"`
		}
		if err := json.Unmarshal(timeRaw, &timeObj); err == nil {
			entry.Timestamp = timeObj.Created
		}
	}
	if entry.Timestamp == "" {
		if createdRaw, ok := raw["created_at"]; ok {
			var createdAt int64
			if err := json.Unmarshal(createdRaw, &createdAt); err == nil {
				entry.Timestamp = unixToTime(createdAt).UTC().Format(time.RFC3339)
			}
		}
	}

	// Parse content
	entry.Message = parseOpenCodeMessage(raw, entry.Type)

	return entry
}

// openCodePart is a single entry in an OpenCode message's "parts" array.
// OpenCode (the Go CLI backing this integration) splits message content into
// typed parts rather than a single "content" field — see
// internal/message/message.go upstream.
type openCodePart struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// partsToContentBlocks converts an OpenCode "parts" array into ContentBlocks.
// Returns nil if parts can't be parsed or none produced usable content.
func partsToContentBlocks(partsRaw json.RawMessage) []agent.ContentBlock {
	var parts []openCodePart
	if err := json.Unmarshal(partsRaw, &parts); err != nil {
		return nil
	}

	var blocks []agent.ContentBlock
	for _, p := range parts {
		switch p.Type {
		case "text":
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(p.Data, &d) == nil && d.Text != "" {
				blocks = append(blocks, agent.ContentBlock{Type: "text", Text: d.Text})
			}
		case "reasoning":
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(p.Data, &d) == nil && d.Text != "" {
				blocks = append(blocks, agent.ContentBlock{Type: "thinking", Thinking: d.Text})
			}
		case "tool_call":
			var d struct {
				ID    string `json:"id"`
				Name  string `json:"name"`
				Input string `json:"input"`
			}
			if json.Unmarshal(p.Data, &d) == nil {
				blocks = append(blocks, agent.ContentBlock{
					Type:  "tool_use",
					ID:    d.ID,
					Name:  d.Name,
					Input: json.RawMessage(d.Input),
				})
			}
		case "tool_result":
			var d struct {
				ID     string `json:"id"`
				Output string `json:"output"`
			}
			if json.Unmarshal(p.Data, &d) == nil {
				content, err := json.Marshal(d.Output)
				if err != nil {
					continue
				}
				blocks = append(blocks, agent.ContentBlock{
					Type:      "tool_result",
					ToolUseID: d.ID,
					Content:   content,
				})
			}
		}
	}
	return blocks
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

	// OpenCode's SQLite-backed messages store content as a "parts" array
	// rather than "content" — check for it first.
	if partsRaw, ok := raw["parts"]; ok {
		if blocks := partsToContentBlocks(partsRaw); len(blocks) > 0 {
			msg.Content = blocks
			return msg
		}
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
