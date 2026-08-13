package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
//
// OpenCode's SQLite table and column names have changed across releases
// (e.g. the session/message table names and whichever columns identify a
// project or timestamp a row). Hardcoding a specific schema silently stops
// matching anything after an upstream migration, so this discovers the
// actual table names and reads full rows, then matches/sorts on whatever
// fields are present instead of assuming fixed column names.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := findSQLiteTable(dbPath, "session")
	if sessionTable == "" {
		return nil, nil
	}

	sessionRows, err := querySQLiteJSON(dbPath, fmt.Sprintf("SELECT * FROM %s", sessionTable))
	if err != nil || len(sessionRows) == 0 {
		return nil, nil
	}

	absProjectPath, err := filepath.Abs(projectPath)
	if err != nil {
		absProjectPath = projectPath
	}

	bestRow, bestSessionID := pickBestSessionRow(sessionRows, projectID, absProjectPath)
	if bestSessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout). If we can't find a
	// recognizable timestamp, proceed anyway — better to try than skip.
	if t, ok := rowTime(bestRow); ok && time.Since(t) > agent.RecentSessionTimeout {
		return nil, nil
	}

	messageTable := findSQLiteTable(dbPath, "message")
	if messageTable == "" {
		return nil, nil
	}

	messageRows, err := querySQLiteJSON(dbPath, fmt.Sprintf("SELECT * FROM %s", messageTable))
	if err != nil || len(messageRows) == 0 {
		return nil, nil
	}

	var matched []map[string]interface{}
	for _, row := range messageRows {
		if rowReferencesSession(row, bestSessionID) {
			matched = append(matched, row)
		}
	}
	if len(matched) == 0 {
		return nil, nil
	}
	sortRowsByTime(matched)

	var entries []json.RawMessage
	for _, row := range matched {
		if data := extractMessageData(row); data != nil {
			entries = append(entries, data)
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}

	transcriptData, err := json.Marshal(entries)
	if err != nil {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// pickBestSessionRow returns the most recently updated session row that
// belongs to the given project, along with its session ID. If no row can be
// matched to the project (e.g. the project-identifying column was renamed
// upstream), it falls back to the single most recent session overall, since
// a given OpenCode data directory typically only holds sessions for one
// project at a time in practice.
func pickBestSessionRow(rows []map[string]interface{}, projectID, absProjectPath string) (map[string]interface{}, string) {
	var bestRow map[string]interface{}
	var bestSessionID string

	for _, row := range rows {
		if !rowMatchesProject(row, projectID, absProjectPath) {
			continue
		}
		id := stringFromRow(row, "id", "ID", "sessionID", "session_id")
		if id == "" {
			continue
		}
		if bestRow == nil || rowIsNewer(row, bestRow) {
			bestRow = row
			bestSessionID = id
		}
	}
	if bestSessionID != "" {
		return bestRow, bestSessionID
	}

	for _, row := range rows {
		id := stringFromRow(row, "id", "ID", "sessionID", "session_id")
		if id == "" {
			continue
		}
		if bestRow == nil || rowIsNewer(row, bestRow) {
			bestRow = row
			bestSessionID = id
		}
	}
	return bestRow, bestSessionID
}

// findSQLiteTable returns the name of the first table in dbPath whose name
// contains substr (case-insensitive), or "" if none is found. Table names
// are discovered dynamically since they've changed between OpenCode
// releases (e.g. a "session"/"message" rename or pluralization).
func findSQLiteTable(dbPath, substr string) string {
	cmd := exec.Command("sqlite3", dbPath, ".tables")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	substr = strings.ToLower(substr)
	for _, field := range strings.Fields(string(output)) {
		if strings.Contains(strings.ToLower(field), substr) {
			return field
		}
	}
	return ""
}

// querySQLiteJSON runs query against dbPath and decodes the result as JSON
// objects, one per row.
func querySQLiteJSON(dbPath, query string) ([]map[string]interface{}, error) {
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, nil
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// stringFromRow returns the first non-empty string value found in row for
// any of the given candidate column names.
func stringFromRow(row map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := row[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// rowMatchesProject reports whether any field in row identifies the given
// project, matching either the computed project ID (e.g. a git root commit
// hash) or the project's absolute path. Scanning every field, rather than a
// single hardcoded column, tolerates the project-identifying column being
// renamed upstream.
func rowMatchesProject(row map[string]interface{}, projectID, absProjectPath string) bool {
	for _, v := range row {
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		if projectID != "" && s == projectID {
			return true
		}
		if agent.PathsEqual(s, absProjectPath) {
			return true
		}
	}
	return false
}

// rowReferencesSession reports whether any field in row equals sessionID.
func rowReferencesSession(row map[string]interface{}, sessionID string) bool {
	for _, v := range row {
		if s, ok := v.(string); ok && s == sessionID {
			return true
		}
	}
	return false
}

// rowTime extracts a recency timestamp from row by checking common
// timestamp-like column names against several known encodings.
func rowTime(row map[string]interface{}) (time.Time, bool) {
	candidates := []string{
		"time_updated", "timeUpdated", "updated", "updatedAt", "updated_at",
		"time_created", "timeCreated", "created", "createdAt", "created_at",
	}
	for _, k := range candidates {
		v, ok := row[k]
		if !ok {
			continue
		}
		if t, ok := parseSQLiteTime(v); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseSQLiteTime parses a timestamp value that may be encoded as a Unix
// timestamp (seconds or milliseconds) or one of several string formats.
func parseSQLiteTime(v interface{}) (time.Time, bool) {
	switch val := v.(type) {
	case float64:
		ms := int64(val)
		switch {
		case ms > 1e12:
			return time.UnixMilli(ms), true
		case ms > 1e9:
			return time.Unix(ms, 0), true
		}
	case string:
		formats := []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05.000Z",
			"2006-01-02 15:04:05",
		}
		for _, f := range formats {
			if t, err := time.Parse(f, val); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// rowIsNewer reports whether row a is more recent than row b. A row without
// a recognizable timestamp is treated as older than any row that has one.
func rowIsNewer(a, b map[string]interface{}) bool {
	ta, aok := rowTime(a)
	tb, bok := rowTime(b)
	if aok && bok {
		return ta.After(tb)
	}
	return aok && !bok
}

// sortRowsByTime sorts rows oldest-to-newest by their recognizable
// timestamp, leaving rows without one in their original relative order.
func sortRowsByTime(rows []map[string]interface{}) {
	sort.SliceStable(rows, func(i, j int) bool {
		ti, iok := rowTime(rows[i])
		tj, jok := rowTime(rows[j])
		if iok && jok {
			return ti.Before(tj)
		}
		return false
	})
}

// extractMessageData pulls the message payload out of a message row. Most
// schemas keep the message body as a JSON blob in a "data" (or similarly
// named) column; if none is found or it isn't valid JSON, the whole row is
// used instead so no information is silently dropped.
func extractMessageData(row map[string]interface{}) json.RawMessage {
	for _, k := range []string{"data", "content", "message", "body"} {
		v, ok := row[k]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		var probe interface{}
		if json.Unmarshal([]byte(s), &probe) == nil {
			return json.RawMessage(s)
		}
	}

	data, err := json.Marshal(row)
	if err != nil {
		return nil
	}
	return data
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
