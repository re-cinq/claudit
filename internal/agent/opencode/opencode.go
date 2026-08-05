package opencode

import (
	"bytes"
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

// flatSessionMatch describes a candidate session file found on disk.
type flatSessionMatch struct {
	sessionID string
	modTime   time.Time
}

// discoverFromFlatFiles tries the legacy flat file session discovery.
//
// Newer OpenCode releases have been observed to change how sessions are
// partitioned on disk between versions (nested under a directory named
// after the project ID, stored flat with the project recorded as a field
// inside the session JSON, or nested under an unfamiliar key). To stay
// compatible across these layouts, we scan storage/session for anything
// that plausibly belongs to this project rather than assuming one fixed
// directory structure.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	projectID := GetProjectID(projectPath)

	match := bestMatchingSessionFile(sessionRoot, projectID, projectPath)
	if match == nil {
		return nil, nil
	}

	// The transcript path for OpenCode is the message directory
	msgDir, _ := GetMessageDir(match.sessionID)

	return &agent.SessionInfo{
		SessionID:      match.sessionID,
		TranscriptPath: msgDir,
		StartedAt:      match.modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// bestMatchingSessionFile scans dir for the most recently modified session
// JSON file (within agent.RecentSessionTimeout) that belongs to this
// project. Files inside a subdirectory literally named projectID are
// trusted without inspecting their contents (the original, still-supported
// layout). Everything else — flat files directly under dir, or
// subdirectories with a different naming scheme — is only matched if its
// JSON content references projectID or projectPath via a recognizable
// field name.
func bestMatchingSessionFile(dir, projectID, projectPath string) *flatSessionMatch {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	now := time.Now()
	var best *flatSessionMatch

	consider := func(name string, modTime time.Time, requireContentMatch bool, data []byte) {
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return
		}
		if requireContentMatch {
			var session map[string]interface{}
			if err := json.Unmarshal(data, &session); err != nil {
				return
			}
			if !sessionMatchesProject(session, projectID, projectPath) {
				return
			}
		}
		if best == nil || modTime.After(best.modTime) {
			best = &flatSessionMatch{
				sessionID: strings.TrimSuffix(name, ".json"),
				modTime:   modTime,
			}
		}
	}

	for _, entry := range entries {
		full := filepath.Join(dir, entry.Name())

		if entry.IsDir() {
			trustedDir := entry.Name() == projectID
			nested, err := os.ReadDir(full)
			if err != nil {
				continue
			}
			for _, ne := range nested {
				if ne.IsDir() || !strings.HasSuffix(ne.Name(), ".json") {
					continue
				}
				info, err := ne.Info()
				if err != nil {
					continue
				}
				if trustedDir {
					consider(ne.Name(), info.ModTime(), false, nil)
					continue
				}
				data, err := os.ReadFile(filepath.Join(full, ne.Name()))
				if err != nil {
					continue
				}
				consider(ne.Name(), info.ModTime(), true, data)
			}
			continue
		}

		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		consider(entry.Name(), info.ModTime(), true, data)
	}

	return best
}

// sessionMatchesProject checks whether a decoded session JSON object
// references the given project, trying several plausible field names since
// OpenCode's session schema has changed across releases.
func sessionMatchesProject(session map[string]interface{}, projectID, projectPath string) bool {
	candidates := []string{"projectID", "project_id", "directory", "cwd", "worktree", "path", "project"}
	for _, key := range candidates {
		v, ok := session[key]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s == projectID || s == projectPath {
			return true
		}
	}
	return false
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
//
// OpenCode's SQLite schema has changed column names across releases, so
// rather than hardcoding "project_id"/"time_updated"/"data", we introspect
// the actual table columns via SQLite's pragma_table_info and adapt.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionCols, err := sqliteTableColumns(dbPath, "session")
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	sessionID, timeCol := findRecentSessionID(dbPath, sessionCols, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	if timeCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM session WHERE id='%s';`, timeCol, sqlEscape(sessionID))
		cmd := exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			timeStr := strings.TrimSpace(string(timeOutput))
			if !isRecentTimestamp(timeStr) {
				return nil, nil
			}
		}
		// If we can't run the query, proceed anyway — better to try than skip
	}

	transcriptData := fetchSessionMessages(dbPath, sessionID)
	if len(transcriptData) == 0 {
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

// sqliteTableColumns returns the column names of a SQLite table, or an
// error if the table doesn't exist or sqlite3 can't be invoked.
func sqliteTableColumns(dbPath, table string) ([]string, error) {
	query := fmt.Sprintf(`SELECT name FROM pragma_table_info('%s');`, table)
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			cols = append(cols, line)
		}
	}
	return cols, nil
}

// firstPresentColumn returns the first candidate that appears in cols, or
// "" if none of them do.
func firstPresentColumn(cols []string, candidates ...string) string {
	set := make(map[string]bool, len(cols))
	for _, c := range cols {
		set[c] = true
	}
	for _, cand := range candidates {
		if set[cand] {
			return cand
		}
	}
	return ""
}

// findRecentSessionID locates the most recently updated session belonging
// to this project, returning its ID and the name of the time column used
// (if any) so the caller can perform a recency check.
func findRecentSessionID(dbPath string, sessionCols []string, projectID, projectPath string) (sessionID, timeCol string) {
	timeCol = firstPresentColumn(sessionCols, "time_updated", "updated", "time_modified", "mtime", "time_created", "created")
	projectCol := firstPresentColumn(sessionCols, "project_id", "projectID", "directory", "worktree", "project", "path", "cwd")

	orderBy := ""
	if timeCol != "" {
		orderBy = fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}

	tryValue := func(value string) string {
		if projectCol == "" || value == "" {
			return ""
		}
		query := fmt.Sprintf(`SELECT id FROM session WHERE %s='%s'%s LIMIT 1;`, projectCol, sqlEscape(value), orderBy)
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		output, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(output))
	}

	if id := tryValue(projectID); id != "" {
		return id, timeCol
	}
	if id := tryValue(projectPath); id != "" {
		return id, timeCol
	}

	// No recognizable project-scoping column at all: fall back to the most
	// recently active session in the database. We only do this when there
	// is no project column to match against, to avoid picking up an
	// unrelated project's session when the column simply didn't match.
	if projectCol == "" {
		query := fmt.Sprintf(`SELECT id FROM session%s LIMIT 1;`, orderBy)
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		output, err := cmd.Output()
		if err == nil {
			return strings.TrimSpace(string(output)), timeCol
		}
	}

	return "", timeCol
}

// isRecentTimestamp reports whether timeStr (in any of the formats OpenCode
// has used across releases, including raw unix seconds/milliseconds) is
// within agent.RecentSessionTimeout of now. Unparseable or empty input is
// treated as recent — better to try storing the conversation than to skip
// it based on a timestamp format we don't recognize.
func isRecentTimestamp(timeStr string) bool {
	if timeStr == "" {
		return true
	}

	formats := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
	for _, f := range formats {
		if t, err := time.Parse(f, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		var t time.Time
		if n > 1e12 {
			t = time.UnixMilli(n)
		} else {
			t = time.Unix(n, 0)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	return true
}

// fetchSessionMessages returns the messages for a session as a JSON array.
// If the message table has a single JSON blob column (the original "data"
// column), each row is patched with its id, preserving full fidelity. If
// no such column is found (message content stored differently, e.g. across
// separate columns or a "part" table), each row's own columns are dumped
// as a JSON object instead so at least the session can still be captured.
func fetchSessionMessages(dbPath, sessionID string) []byte {
	msgCols, err := sqliteTableColumns(dbPath, "message")
	if err != nil || len(msgCols) == 0 {
		return nil
	}

	sessionCol := firstPresentColumn(msgCols, "session_id", "sessionID", "session", "conversation_id")
	if sessionCol == "" {
		return nil
	}

	orderCol := firstPresentColumn(msgCols, "time_created", "created", "time", "seq", "id")
	orderBy := ""
	if orderCol != "" {
		orderBy = fmt.Sprintf(" ORDER BY %s", orderCol)
	}

	genericFields := make([]string, 0, len(msgCols))
	for _, c := range msgCols {
		genericFields = append(genericFields, fmt.Sprintf("'%s', %s", c, c))
	}
	genericQuery := fmt.Sprintf(
		`SELECT json_group_array(json_object(%s)) FROM message WHERE %s='%s'%s;`,
		strings.Join(genericFields, ", "), sessionCol, sqlEscape(sessionID), orderBy,
	)

	dataCol := firstPresentColumn(msgCols, "data", "content", "json")
	if dataCol != "" {
		query := fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', id))) FROM message WHERE %s='%s'%s;`,
			dataCol, sessionCol, sqlEscape(sessionID), orderBy,
		)
		cmd := exec.Command("sqlite3", dbPath, query)
		if output, err := cmd.Output(); err == nil {
			if data := cleanTranscriptArray(output); data != nil {
				return data
			}
		}
		// json_patch failed (dataCol wasn't a JSON object column after all) —
		// fall through to the generic per-column dump below.
	}

	cmd := exec.Command("sqlite3", dbPath, genericQuery)
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	return cleanTranscriptArray(output)
}

// cleanTranscriptArray trims sqlite3 output and filters out the
// placeholder results it returns when a query matches no rows.
func cleanTranscriptArray(output []byte) []byte {
	data := bytes.TrimSpace(output)
	if len(data) == 0 || string(data) == "[null]" || string(data) == "[]" {
		return nil
	}
	return data
}

// sqlEscape escapes single quotes for safe inclusion in a SQLite string literal.
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
