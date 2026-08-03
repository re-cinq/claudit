```go
package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

func (a *Agent) Name() agent.Name    { return agent.OpenCode }
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
		"bash":     true,
		"shell":    true,
		"terminal": true,
		"execute":  true,
		"run":      true,
		"command":  true,
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

// parseMessageDir reads all message files from an OpenCode message directory,
// merging in any separately-stored content parts, and parses the result.
func (a *Agent) parseMessageDir(dir string) (*agent.Transcript, error) {
	data, err := buildOpenCodeMessagesJSON(dir)
	if err != nil {
		return nil, err
	}
	return a.ParseTranscript(bytes.NewReader(data))
}

// buildOpenCodeMessagesJSON reads all message files (.json and .jsonl) from an
// OpenCode message directory and combines them into a single JSON array. Newer
// OpenCode versions store a message's content ("parts": text, tool calls, tool
// results) separately from its envelope (role, timestamps) under a sibling
// storage/part/<sessionID>/<messageID>/ directory (see GetPartDir); when present,
// those parts are merged into the message object under a "parts" key so
// parseOpenCodeMessage can recover the actual content.
func buildOpenCodeMessagesJSON(msgDir string) ([]byte, error) {
	dirEntries, err := os.ReadDir(msgDir)
	if err != nil {
		return nil, err
	}

	sessionID := filepath.Base(msgDir)
	partDir, _ := GetPartDir(sessionID)

	var messages []json.RawMessage
	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		name := de.Name()

		switch {
		case strings.HasSuffix(name, ".jsonl"):
			data, err := os.ReadFile(filepath.Join(msgDir, name))
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					messages = append(messages, json.RawMessage(line))
				}
			}

		case strings.HasSuffix(name, ".json"):
			data, err := os.ReadFile(filepath.Join(msgDir, name))
			if err != nil {
				continue
			}

			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil {
				messages = append(messages, json.RawMessage(data))
				continue
			}

			if parts := readOpenCodePartFiles(partDir, messageIDFromRaw(raw, name)); len(parts) > 0 {
				raw["parts"] = parts
				if merged, err := json.Marshal(raw); err == nil {
					data = merged
				}
			}
			messages = append(messages, json.RawMessage(data))
		}
	}

	return json.Marshal(messages)
}

// readOpenCodePartFiles reads all part JSON files for a message from
// <partBaseDir>/<messageID>/*.json and returns them as a marshaled JSON array,
// or nil if the directory doesn't exist or is empty.
func readOpenCodePartFiles(partBaseDir, messageID string) json.RawMessage {
	if partBaseDir == "" || messageID == "" {
		return nil
	}

	dir := filepath.Join(partBaseDir, messageID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var items []json.RawMessage
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		items = append(items, json.RawMessage(data))
	}
	if len(items) == 0 {
		return nil
	}

	marshaled, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	return marshaled
}

// messageIDFromRaw extracts a message ID from a parsed message object,
// falling back to the filename (without extension) if the "id" field is absent.
func messageIDFromRaw(raw map[string]json.RawMessage, filename string) string {
	if idRaw, ok := raw["id"]; ok {
		var id string
		if json.Unmarshal(idRaw, &id) == nil && id != "" {
			return id
		}
	}
	return strings.TrimSuffix(filename, filepath.Ext(filename))
}

// DiscoverSession finds an active or recent OpenCode session.
// OpenCode's on-disk storage format has changed across versions, so several
// strategies are tried in order of confidence: flat JSON files under the XDG
// data directory (the classic layout), a SQLite database in the XDG data
// directory, and finally a SQLite database local to the project (some
// versions/configurations keep .opencode/opencode.db in the repo itself).
// Each strategy is cheap to rule out and returns (nil, nil) when it doesn't
// apply, so trying several in sequence is safe.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	if session, err := a.discoverFromFlatFiles(projectPath); session != nil || err != nil {
		return session, err
	}

	projectID := GetProjectID(projectPath)

	if dataDir, err := GetDataDir(); err == nil {
		dbPath := filepath.Join(dataDir, "opencode.db")
		if session, err := discoverFromSQLiteAt(dbPath, projectID, projectPath); session != nil || err != nil {
			return session, err
		}
	}

	// Some OpenCode versions/configurations keep the database in the project
	// itself rather than the global XDG data directory.
	projectDBPath := filepath.Join(projectPath, ".opencode", "opencode.db")
	return discoverFromSQLiteAt(projectDBPath, projectID, projectPath)
}

// discoverFromFlatFiles tries the flat-file session discovery, merging in any
// separately-stored message parts (see buildOpenCodeMessagesJSON).
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

	var transcriptData []byte
	if msgDir != "" {
		if data, err := buildOpenCodeMessagesJSON(msgDir); err == nil {
			trimmed := strings.TrimSpace(string(data))
			if trimmed != "" && trimmed != "null" && trimmed != "[]" {
				transcriptData = data
			}
		}
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// discoverFromSQLiteAt queries an OpenCode SQLite database for the most
// recent session matching projectID. OpenCode's SQLite table and column names
// have changed across versions, so they are discovered at runtime via
// sqlite_master/PRAGMA table_info rather than hardcoded, and message rows are
// read back with `sqlite3 -json` (which serializes whatever columns exist)
// instead of assuming specific column names.
func discoverFromSQLiteAt(dbPath, projectID, projectPath string) (*agent.SessionInfo, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	tables, err := sqliteTables(dbPath)
	if err != nil || len(tables) == 0 {
		return nil, nil
	}

	sessionTable := matchTable(tables, "session")
	messageTable := matchTable(tables, "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols, err := sqliteColumns(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}
	messageCols, err := sqliteColumns(dbPath, messageTable)
	if err != nil || len(messageCols) == 0 {
		return nil, nil
	}

	idCol := matchColumn(sessionCols, "id")
	sessionLinkCol := matchColumn(messageCols, "session")
	if idCol == "" || sessionLinkCol == "" {
		return nil, nil
	}

	projectCol := matchColumn(sessionCols, "project")
	updatedCol := firstMatch(sessionCols, "updated", "modified", "time_updated")
	orderCol := firstMatch(messageCols, "created", "time_created", "sequence")

	where := ""
	if projectCol != "" {
		where = fmt.Sprintf("WHERE %s = '%s'", projectCol, escapeSQLiteLiteral(projectID))
	}
	orderBy := ""
	if updatedCol != "" {
		orderBy = fmt.Sprintf("ORDER BY %s DESC", updatedCol)
	}

	sessionQuery := fmt.Sprintf("SELECT %s FROM %s %s %s LIMIT 1;", idCol, sessionTable, where, orderBy)
	cmd := exec.Command("sqlite3", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Recency check, best-effort: if we can't find/parse an updated-at
	// column, proceed anyway rather than skipping a possibly-valid session.
	if updatedCol != "" {
		timeQuery := fmt.Sprintf("SELECT %s FROM %s WHERE %s = '%s';",
			updatedCol, sessionTable, idCol, escapeSQLiteLiteral(sessionID))
		cmd = exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			if t, ok := parseOpenCodeTimestamp(strings.TrimSpace(string(timeOutput))); ok {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
		}
	}

	msgQuery := fmt.Sprintf("SELECT * FROM %s WHERE %s = '%s'", messageTable, sessionLinkCol, escapeSQLiteLiteral(sessionID))
	if orderCol != "" {
		msgQuery += fmt.Sprintf(" ORDER BY %s", orderCol)
	}
	msgQuery += ";"

	cmd = exec.Command("sqlite3", "-json", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	if len(transcriptData) == 0 || string(transcriptData) == "[]" || string(transcriptData) == "null" {
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

// sqliteTables returns all table names in a SQLite database.
func sqliteTables(dbPath string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var tables []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			tables = append(tables, line)
		}
	}
	return tables, nil
}

// sqliteColumns returns all column names for a SQLite table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 && fields[1] != "" {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// matchTable finds the table whose name best matches hint (e.g. "session"
// matches "session" or "sessions" exactly, then falls back to substring match).
func matchTable(tables []string, hint string) string {
	for _, want := range []string{hint, hint + "s"} {
		for _, t := range tables {
			if strings.EqualFold(t, want) {
				return t
			}
		}
	}
	for _, t := range tables {
		if strings.Contains(strings.ToLower(t), hint) {
			return t
		}
	}
	return ""
}

// matchColumn finds the column whose name best matches hint.
func matchColumn(cols []string, hint string) string {
	for _, c := range cols {
		if strings.EqualFold(c, hint) {
			return c
		}
	}
	for _, c := range cols {
		if strings.Contains(strings.ToLower(c), hint) {
			return c
		}
	}
	return ""
}

// firstMatch returns the first column matching any of the given hints.
func firstMatch(cols []string, hints ...string) string {
	for _, h := range hints {
		if c := matchColumn(cols, h); c != "" {
			return c
		}
	}
	return ""
}

// escapeSQLiteLiteral escapes single quotes for use in a SQLite string literal.
func escapeSQLiteLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// parseOpenCodeTimestamp parses a SQLite timestamp value that may be a Unix
// epoch (seconds or milliseconds) or one of several common string formats.
func parseOpenCodeTimestamp(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}

	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
		if n > 1_000_000_000_000 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}

	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
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

	// Parse timestamp: either a nested {"time":{"created":...}} (legacy flat
	// file format) or a top-level "created_at" epoch value (SQLite/newer format).
	if timeRaw, ok := raw["time"]; ok {
		var timeObj struct {
			Created string `json:"created"`
		}
		if err := json.Unmarshal(timeRaw, &timeObj); err == nil {
			entry.Timestamp = timeObj.Created
		}
	} else if createdRaw, ok := raw["created_at"]; ok {
		var v json.Number
		if err := json.Unmarshal(createdRaw, &v); err == nil {
			if t, ok := parseOpenCodeTimestamp(v.String()); ok {
				entry.Timestamp = t.UTC().Format(time.RFC3339)
			}
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

	// Newer OpenCode versions store content as a "parts" array (either
	// embedded directly, or as a JSON-encoded string when it comes from a
	// SQLite TEXT column) rather than a "content" field.
	if partsRaw, ok := raw["parts"]; ok {
		var blocks []agent.ContentBlock
		for _, item := range decodeOpenCodeParts(partsRaw) {
			if block, ok := openCodePartToBlock(item); ok {
				blocks = append(blocks, block)
			}
		}
		if len(blocks) > 0 {
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

// decodeOpenCodeParts normalizes a "parts" field into a slice of raw part
// objects. It accepts either a JSON array directly, or a JSON string
// containing an encoded array (as produced when reading a SQLite TEXT column).
func decodeOpenCodeParts(raw json.RawMessage) []json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}

	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil || s == "" {
			return nil
		}
		trimmed = []byte(s)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil
	}
	return items
}

// openCodePartToBlock converts a single OpenCode "part" object into a
// ContentBlock. OpenCode's part schema wraps type-specific fields either
// directly on the part object or nested under a "data" key depending on
// version, so both layouts are checked.
func openCodePartToBlock(item json.RawMessage) (agent.ContentBlock, bool) {
	var part struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Tool  string          `json:"tool"`
		Name  string          `json:"name"`
		ID    string          `json:"id"`
		Input json.RawMessage `json:"input"`
		State json.RawMessage `json:"state"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(item, &part); err != nil {
		return agent.ContentBlock{}, false
	}

	if len(part.Data) > 0 {
		var nested struct {
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			ID    string          `json:"id"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(part.Data, &nested) == nil {
			if part.Text == "" {
				part.Text = nested.Text
			}
			if part.Name == "" {
				part.Name = nested.Name
			}
			if part.ID == "" {
				part.ID = nested.ID
			}
			if len(part.Input) == 0 {
				part.Input = nested.Input
			}
		}
	}

	toolName := part.Tool
	if toolName == "" {
		toolName = part.Name
	}

	input := part.Input
	if len(input) == 0 && len(part.State) > 0 {
		var state struct {
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(part.State, &state) == nil {
			input = state.Input
		}
	}

	switch part.Type {
	case "text":
		if part.Text == "" {
			return agent.ContentBlock{}, false
		}
		return agent.ContentBlock{Type: "text", Text: part.Text}, true
	case "reasoning":
		if part.Text == "" {
			return agent.ContentBlock{}, false
		}
		return agent.ContentBlock{Type: "thinking", Thinking: part.Text}, true
	case "tool", "tool_call", "tool-call", "tool_use":
		return agent.ContentBlock{Type: "tool_use", ID: part.ID, Name: toolName, Input: input}, true
	case "tool_result", "tool-result":
		return agent.ContentBlock{Type: "tool_result", ToolUseID: part.ID, Content: input}, true
	default:
		return agent.ContentBlock{}, false
	}
}
```
