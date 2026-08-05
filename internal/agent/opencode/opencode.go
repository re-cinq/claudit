```go
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
// It first tries flat file storage, then falls back to SQLite.
// OpenCode's on-disk storage layout and SQLite schema have changed across
// versions, so both discovery strategies try the historically-known layout
// first and fall back to schema-introspective discovery when that doesn't
// turn anything up.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// flatSessionCandidate tracks the best flat-file session match found so far.
type flatSessionCandidate struct {
	path    string
	modTime time.Time
	score   int
}

// discoverFromFlatFiles scans OpenCode's flat-file session storage for the
// most recently active session belonging to projectPath.
//
// Older OpenCode versions scope session files under a directory keyed by a
// project ID computed from the git root commit (see GetProjectID). Newer
// versions may lay sessions out differently (e.g. a flat "info" directory)
// and instead record the working directory inside each session file. To
// stay compatible with either layout, this walks the whole session storage
// tree, scores each session file by whether its own recorded directory (if
// any) matches projectPath, and falls back to the legacy project-ID-scoped
// path when no file carries directory information.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	if _, err := os.Stat(sessionRoot); err != nil {
		return nil, nil
	}

	cleanProjectPath := filepath.Clean(projectPath)
	realProjectPath := cleanProjectPath
	if rp, err := filepath.EvalSymlinks(projectPath); err == nil {
		realProjectPath = rp
	}
	legacyProjectID := GetProjectID(projectPath)
	now := time.Now()

	var best *flatSessionCandidate

	_ = filepath.Walk(sessionRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}
		if now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		score := 0
		if data, readErr := os.ReadFile(path); readErr == nil {
			var fields map[string]interface{}
			if json.Unmarshal(data, &fields) == nil {
				for _, key := range []string{"directory", "cwd", "worktree", "path", "root", "projectPath"} {
					if s, ok := fields[key].(string); ok {
						candidate := filepath.Clean(s)
						if candidate == cleanProjectPath || candidate == realProjectPath {
							score = 2
						}
					}
				}
			}
		}
		if score == 0 && legacyProjectID != "" && strings.Contains(filepath.ToSlash(path), "/"+legacyProjectID+"/") {
			score = 1
		}
		if score == 0 {
			return nil
		}

		if best == nil || score > best.score || (score == best.score && info.ModTime().After(best.modTime)) {
			best = &flatSessionCandidate{path: path, modTime: info.ModTime(), score: score}
		}
		return nil
	})

	if best == nil {
		return nil, nil
	}

	sessionID := strings.TrimSuffix(filepath.Base(best.path), ".json")
	msgDir := resolveMessageDir(best.path, sessionID)

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: msgDir,
		StartedAt:      best.modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// resolveMessageDir tries several plausible locations for a session's
// message directory, since the path relative to the session file has moved
// around across OpenCode versions. It falls back to the session file itself
// so a transcript source is always available, even if the exact message
// directory can't be located.
func resolveMessageDir(sessionFilePath, sessionID string) string {
	var candidates []string
	if msgDir, err := GetMessageDir(sessionID); err == nil {
		candidates = append(candidates, msgDir)
	}
	candidates = append(candidates,
		filepath.Join(filepath.Dir(filepath.Dir(sessionFilePath)), "message", sessionID),
		filepath.Join(filepath.Dir(sessionFilePath), "message", sessionID),
	)

	var fallback string
	for _, c := range candidates {
		info, err := os.Stat(c)
		if err != nil || !info.IsDir() {
			continue
		}
		if fallback == "" {
			fallback = c
		}
		if entries, err := os.ReadDir(c); err == nil && len(entries) > 0 {
			return c
		}
	}
	if fallback != "" {
		return fallback
	}

	return sessionFilePath
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session belonging to projectPath. It first tries the
// historically-known session/message schema, then falls back to
// introspecting the schema generically, since OpenCode has changed table
// and column names across versions.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	if info := discoverFromSQLiteLegacySchema(dbPath, projectID, projectPath); info != nil {
		return info, nil
	}

	return discoverFromSQLiteGeneric(dbPath, projectPath), nil
}

// discoverFromSQLiteLegacySchema tries the session/message schema used by
// earlier OpenCode versions: a "session" table with project_id/time_updated
// columns and a "message" table with session_id/data/time_created columns.
// Returns nil if that schema doesn't produce a usable result, so the caller
// can fall back to generic schema introspection.
func discoverFromSQLiteLegacySchema(dbPath, projectID, projectPath string) *agent.SessionInfo {
	sessionQuery := fmt.Sprintf(
		`SELECT id FROM session WHERE project_id='%s' ORDER BY time_updated DESC LIMIT 1;`,
		escapeSQLite(projectID),
	)
	sessionOutput, err := exec.Command("sqlite3", dbPath, sessionQuery).Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Check if this session was recent (within timeout)
	timeQuery := fmt.Sprintf(`SELECT time_updated FROM session WHERE id='%s';`, escapeSQLite(sessionID))
	if timeOutput, err := exec.Command("sqlite3", dbPath, timeQuery).Output(); err == nil {
		if t, ok := parseAnyTime(strings.TrimSpace(string(timeOutput))); ok {
			if time.Since(t) > agent.RecentSessionTimeout {
				return nil
			}
		}
		// If we can't parse the time, proceed anyway — better to try than skip
	}

	// Get messages for this session as a JSON array
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE session_id='%s' ORDER BY time_created;`,
		escapeSQLite(sessionID),
	)
	msgOutput, err := exec.Command("sqlite3", dbPath, msgQuery).Output()
	if err != nil {
		return nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if len(transcriptData) == 0 || string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
		return nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}
}

// discoverFromSQLiteGeneric discovers a session by introspecting the SQLite
// schema rather than assuming fixed table/column names. It looks for the
// most recently active session whose row references projectPath anywhere in
// its fields, falling back to the single most recent session overall if no
// row can be matched to a directory.
func discoverFromSQLiteGeneric(dbPath, projectPath string) *agent.SessionInfo {
	sessionTable := findSQLiteTable(dbPath, "session")
	if sessionTable == "" {
		return nil
	}

	rows, err := querySQLiteRowsAsJSON(dbPath, fmt.Sprintf("SELECT rowid AS shiftlog_rowid, * FROM %s", sessionTable))
	if err != nil || len(rows) == 0 {
		return nil
	}

	cleanProjectPath := filepath.Clean(projectPath)
	realProjectPath := cleanProjectPath
	if rp, err := filepath.EvalSymlinks(projectPath); err == nil {
		realProjectPath = rp
	}

	var best map[string]interface{}
	var bestRowID float64
	var bestMatched bool

	for _, row := range rows {
		rowID, _ := row["shiftlog_rowid"].(float64)
		matched := rowMatchesPath(row, cleanProjectPath, realProjectPath)
		if best == nil ||
			(matched && !bestMatched) ||
			(matched == bestMatched && rowID > bestRowID) {
			best = row
			bestRowID = rowID
			bestMatched = matched
		}
	}

	if best == nil {
		return nil
	}

	sessionID := stringField(best, "id")
	if sessionID == "" {
		return nil
	}

	if t, ok := findRecentTime(best); ok && time.Since(t) > agent.RecentSessionTimeout {
		return nil
	}

	transcriptData := buildTranscriptFromMessageTable(dbPath, sessionID)
	if len(transcriptData) == 0 {
		if data, err := json.Marshal([]map[string]interface{}{best}); err == nil {
			transcriptData = data
		}
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "",
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}
}

// buildTranscriptFromMessageTable finds the message table generically and
// returns the rows belonging to sessionID as a JSON array transcript.
func buildTranscriptFromMessageTable(dbPath, sessionID string) []byte {
	messageTable := findSQLiteTable(dbPath, "message")
	if messageTable == "" {
		return nil
	}

	rows, err := querySQLiteRowsAsJSON(dbPath, fmt.Sprintf("SELECT rowid AS shiftlog_rowid, * FROM %s", messageTable))
	if err != nil || len(rows) == 0 {
		return nil
	}

	var matched []map[string]interface{}
	for _, row := range rows {
		if stringFieldsContain(row, sessionID) {
			matched = append(matched, row)
		}
	}
	if len(matched) == 0 {
		return nil
	}

	sort.Slice(matched, func(i, j int) bool {
		ri, _ := matched[i]["shiftlog_rowid"].(float64)
		rj, _ := matched[j]["shiftlog_rowid"].(float64)
		return ri < rj
	})

	entries := make([]map[string]json.RawMessage, 0, len(matched))
	for _, row := range matched {
		entries = append(entries, normalizeMessageRow(row))
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return nil
	}
	return data
}

// findSQLiteTable returns the name of the table best matching keyword
// (case-insensitive substring match), preferring exact singular/plural forms
// over other tables that merely contain the keyword.
func findSQLiteTable(dbPath, keyword string) string {
	out, err := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';").Output()
	if err != nil {
		return ""
	}

	var candidates []string
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name = strings.TrimSpace(name)
		if name != "" && strings.Contains(strings.ToLower(name), keyword) {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return ""
	}

	for _, name := range candidates {
		lower := strings.ToLower(name)
		if lower == keyword || lower == keyword+"s" {
			return name
		}
	}

	sort.Slice(candidates, func(i, j int) bool { return len(candidates[i]) < len(candidates[j]) })
	return candidates[0]
}

// querySQLiteRowsAsJSON runs a query and returns each row as a generic map,
// using sqlite3's -json output mode so column names don't need to be known
// in advance.
func querySQLiteRowsAsJSON(dbPath, query string) ([]map[string]interface{}, error) {
	out, err := exec.Command("sqlite3", "-json", dbPath, query).Output()
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// rowMatchesPath reports whether any string field in row equals the given
// project path, used to find a session's row without knowing the exact
// column name OpenCode uses for the working directory.
func rowMatchesPath(row map[string]interface{}, cleanPath, realPath string) bool {
	for _, v := range row {
		s, ok := v.(string)
		if !ok || !strings.Contains(s, "/") {
			continue
		}
		candidate := filepath.Clean(s)
		if candidate == cleanPath || candidate == realPath {
			return true
		}
	}
	return false
}

// stringField returns the string value of the field matching key
// (case-insensitive).
func stringField(row map[string]interface{}, key string) string {
	for k, v := range row {
		if strings.EqualFold(k, key) {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

// stringFieldsContain reports whether any string field in row equals target.
func stringFieldsContain(row map[string]interface{}, target string) bool {
	if target == "" {
		return false
	}
	for _, v := range row {
		if s, ok := v.(string); ok && s == target {
			return true
		}
	}
	return false
}

// findRecentTime scans row for time-like fields (by column name) and
// returns the most recent value it can parse.
func findRecentTime(row map[string]interface{}) (time.Time, bool) {
	var best time.Time
	found := false
	for k, v := range row {
		lower := strings.ToLower(k)
		if !strings.Contains(lower, "time") && !strings.Contains(lower, "update") && !strings.Contains(lower, "creat") {
			continue
		}
		if t, ok := parseAnyTime(v); ok && (!found || t.After(best)) {
			best = t
			found = true
		}
	}
	return best, found
}

// parseAnyTime parses a timestamp value in whatever representation OpenCode
// happens to store it in (RFC3339 string, SQL datetime string, or a Unix
// epoch in seconds or milliseconds).
func parseAnyTime(v interface{}) (time.Time, bool) {
	switch val := v.(type) {
	case string:
		formats := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
		for _, f := range formats {
			if t, err := time.Parse(f, val); err == nil {
				return t, true
			}
		}
	case float64:
		switch {
		case val > 1e12:
			return time.UnixMilli(int64(val)), true
		case val > 1e9:
			return time.Unix(int64(val), 0), true
		}
	}
	return time.Time{}, false
}

// normalizeMessageRow converts a generic SQLite row into transcript-entry
// fields. Some OpenCode schema versions store the message payload as a JSON
// string in a single column rather than as top-level columns; when that's
// the case, its role/type/content fields are merged upward so
// parseOpenCodeEntry can find them.
func normalizeMessageRow(row map[string]interface{}) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(row))
	for k, v := range row {
		if data, err := json.Marshal(v); err == nil {
			out[k] = data
		}
	}

	for _, raw := range out {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s == "" {
			continue
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &nested); err != nil {
			continue
		}
		for _, key := range []string{"role", "type", "content", "id", "time"} {
			if _, exists := out[key]; !exists {
				if nv, ok := nested[key]; ok {
					out[key] = nv
				}
			}
		}
	}

	return out
}

// escapeSQLite escapes single quotes for use in a single-quoted SQL literal.
func escapeSQLite(s string) string {
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
```
