package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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
// It first tries flat file storage, then falls back to SQLite (used by
// newer OpenCode releases). Both the on-disk layout and the SQLite schema
// have changed across OpenCode versions, so both paths are written to
// tolerate unknown/renamed directories, tables, and columns rather than
// failing outright when the exact expected shape isn't found.
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

// discoverFromFlatFiles tries flat file session discovery.
//
// Historically, OpenCode stored a session's file directly under
// storage/session/<projectID>/<sessionID>.json, keyed by our computed
// project ID (the git root commit hash). Newer releases have been observed
// to change this nesting (e.g. dropping the project-scoped subdirectory, or
// keying it by something other than the root commit hash), which silently
// broke discovery since the expected directory simply doesn't exist anymore.
//
// To stay robust across these changes, this first tries the conventional
// path, and if that yields nothing, walks the entire session store and
// matches candidates by content (any field that looks like a project
// identifier or working directory) or, as a last resort, by recency alone.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	projectID := GetProjectID(projectPath)

	sessionFile, sessionID := findRecentSessionFile(sessionRoot, projectPath, projectID)
	if sessionFile == "" {
		return nil, nil
	}

	startedAt := time.Now().Format(time.RFC3339)
	if info, statErr := os.Stat(sessionFile); statErr == nil {
		startedAt = info.ModTime().Format(time.RFC3339)
	}

	sessionInfo := &agent.SessionInfo{
		SessionID:   sessionID,
		StartedAt:   startedAt,
		ProjectPath: projectPath,
	}

	// Gather messages for this session. Prefer inline transcript data (found
	// by scanning) so we don't depend on messages living at the conventional
	// path either.
	messageRoot := filepath.Join(dataDir, "storage", "message")
	if transcriptData := collectMessagesForSession(messageRoot, sessionID); len(transcriptData) > 0 {
		sessionInfo.TranscriptData = transcriptData
	} else if msgDir, err := GetMessageDir(sessionID); err == nil {
		sessionInfo.TranscriptPath = msgDir
	}

	return sessionInfo, nil
}

// findRecentSessionFile locates the most relevant recent session file under
// sessionRoot for the given project. It first looks for files that can be
// matched to the project (either because their path contains the project ID
// as a directory segment - the historical layout - or because their JSON
// content has a field identifying the project or working directory). If
// nothing matches by identity, it falls back to the single most recently
// modified session file in the store, since stale sessions from unrelated
// projects are already excluded by the recency window.
func findRecentSessionFile(sessionRoot, projectPath, projectID string) (path, sessionID string) {
	if _, err := os.Stat(sessionRoot); err != nil {
		return "", ""
	}

	now := time.Now()
	cleanProjectPath := filepath.Clean(projectPath)

	var bestMatchedPath, bestMatchedID string
	var bestMatchedTime time.Time
	var bestAnyPath, bestAnyID string
	var bestAnyTime time.Time

	_ = filepath.WalkDir(sessionRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var raw map[string]interface{}
		_ = json.Unmarshal(data, &raw)

		id := stringField(raw, "id", "sessionID", "sessionId")
		if id == "" {
			id = strings.TrimSuffix(d.Name(), ".json")
		}

		if bestAnyPath == "" || modTime.After(bestAnyTime) {
			bestAnyPath, bestAnyID, bestAnyTime = p, id, modTime
		}

		matched := false
		if candidate := stringField(raw, "projectID", "project_id", "directory", "cwd", "path", "worktree"); candidate != "" {
			if candidate == projectID || filepath.Clean(candidate) == cleanProjectPath {
				matched = true
			}
		}
		if !matched && projectID != "" && projectID != "global" {
			// Legacy layout: the project ID is a path segment rather than a
			// JSON field, e.g. storage/session/<projectID>/<sessionID>.json.
			if strings.Contains(filepath.ToSlash(p), "/"+projectID+"/") {
				matched = true
			}
		}

		if matched && (bestMatchedPath == "" || modTime.After(bestMatchedTime)) {
			bestMatchedPath, bestMatchedID, bestMatchedTime = p, id, modTime
		}

		return nil
	})

	if bestMatchedPath != "" {
		return bestMatchedPath, bestMatchedID
	}
	return bestAnyPath, bestAnyID
}

// collectMessagesForSession gathers all message JSON for a session into a
// single JSON array, tolerating OpenCode layout changes the same way
// findRecentSessionFile does: try the conventional per-session directory
// first, then fall back to scanning the whole message store and matching by
// content.
func collectMessagesForSession(messageRoot, sessionID string) []byte {
	if sessionID == "" {
		return nil
	}
	if _, err := os.Stat(messageRoot); err != nil {
		return nil
	}

	var messages []json.RawMessage

	canonicalDir := filepath.Join(messageRoot, sessionID)
	if entries, err := os.ReadDir(canonicalDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(canonicalDir, e.Name()))
			if err != nil {
				continue
			}
			messages = append(messages, json.RawMessage(data))
		}
	}

	if len(messages) == 0 {
		_ = filepath.WalkDir(messageRoot, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
				return nil
			}

			inSessionDir := strings.Contains(filepath.ToSlash(filepath.Dir(p)), "/"+sessionID)

			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}

			if !inSessionDir {
				var raw map[string]interface{}
				if err := json.Unmarshal(data, &raw); err != nil {
					return nil
				}
				if stringField(raw, "sessionID", "session_id", "sessionId") != sessionID {
					return nil
				}
			}

			messages = append(messages, json.RawMessage(data))
			return nil
		})
	}

	if len(messages) == 0 {
		return nil
	}

	data, err := json.Marshal(messages)
	if err != nil {
		return nil
	}
	return data
}

// stringField returns the first non-empty string value found in m for the
// given candidate keys.
func stringField(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session belonging to projectPath/projectID.
//
// OpenCode's SQLite schema (table and column names) has changed between
// releases, so rather than assuming fixed names this introspects the
// database (via sqlite_master and PRAGMA table_info) to find the session
// and message tables and whichever columns look like a project identifier,
// a timestamp, and message content. If a project-identifying column can't be
// found or doesn't match, it falls back to the most recently updated session
// in the database, matching the leniency already used for timestamp parsing
// below.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, sessionCols := introspectTable(dbPath, "session")
	if sessionTable == "" {
		return nil, nil
	}

	projectCol := firstColumn(sessionCols, "project_id", "projectid", "directory", "cwd", "path", "worktree_id", "worktree")
	timeCol := firstColumn(sessionCols, "time_updated", "updated", "time_modified", "modified", "updated_at", "mtime")
	orderBy := "rowid"
	if timeCol != "" {
		orderBy = quoteIdent(timeCol)
	}

	var sessionID string
	if projectCol != "" {
		for _, candidate := range []string{projectID, projectPath} {
			if candidate == "" {
				continue
			}
			q := fmt.Sprintf(`SELECT id FROM %s WHERE %s=%s ORDER BY %s DESC LIMIT 1;`,
				quoteIdent(sessionTable), quoteIdent(projectCol), sqlQuote(candidate), orderBy)
			out, err := exec.Command("sqlite3", dbPath, q).Output()
			if err == nil {
				if id := strings.TrimSpace(string(out)); id != "" {
					sessionID = id
					break
				}
			}
		}
	}

	if sessionID == "" {
		// No project column, or nothing matched it. Fall back to the most
		// recently updated session overall - the recency check below still
		// guards against picking up a stale, unrelated session.
		q := fmt.Sprintf(`SELECT id FROM %s ORDER BY %s DESC LIMIT 1;`, quoteIdent(sessionTable), orderBy)
		out, err := exec.Command("sqlite3", dbPath, q).Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return nil, nil
		}
		sessionID = strings.TrimSpace(string(out))
	}

	// Check if this session was recent (within timeout). If we can't find or
	// parse a timestamp, proceed anyway - better to try than skip.
	if timeCol != "" {
		q := fmt.Sprintf(`SELECT %s FROM %s WHERE id=%s;`, quoteIdent(timeCol), quoteIdent(sessionTable), sqlQuote(sessionID))
		out, err := exec.Command("sqlite3", dbPath, q).Output()
		if err == nil {
			if t, ok := parseFlexibleTime(strings.TrimSpace(string(out))); ok {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
		}
	}

	sessionInfo := &agent.SessionInfo{
		SessionID:   sessionID,
		StartedAt:   time.Now().Format(time.RFC3339),
		ProjectPath: projectPath,
	}

	messageTable, messageCols := introspectTable(dbPath, "message")
	if messageTable == "" {
		return sessionInfo, nil
	}

	sessionRefCol := firstColumn(messageCols, "session_id", "sessionid", "session")
	dataCol := firstColumn(messageCols, "data", "content", "body", "json")
	if sessionRefCol == "" || dataCol == "" {
		return sessionInfo, nil
	}

	orderMsgBy := "rowid"
	if tc := firstColumn(messageCols, "time_created", "created", "created_at"); tc != "" {
		orderMsgBy = quoteIdent(tc)
	}

	// Get messages for this session as a JSON array
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(%s, json_object('id', id))) FROM %s WHERE %s=%s ORDER BY %s;`,
		quoteIdent(dataCol), quoteIdent(messageTable), quoteIdent(sessionRefCol), sqlQuote(sessionID), orderMsgBy,
	)
	out, err := exec.Command("sqlite3", dbPath, msgQuery).Output()
	if err != nil {
		return sessionInfo, nil
	}

	transcriptData := strings.TrimSpace(string(out))
	// sqlite3 returns "[null]" when no rows match
	if transcriptData != "" && transcriptData != "[null]" && transcriptData != "[]" {
		sessionInfo.TranscriptData = []byte(transcriptData)
	}

	return sessionInfo, nil
}

// introspectTable finds a table whose name matches (exactly, or by
// substring) the given hint, and returns its name along with its column
// names via PRAGMA table_info. Returns ("", nil) if no matching table (or
// sqlite3 itself) is available.
func introspectTable(dbPath, hint string) (string, []string) {
	out, err := exec.Command("sqlite3", dbPath, `SELECT name FROM sqlite_master WHERE type='table';`).Output()
	if err != nil {
		return "", nil
	}

	var table string
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if strings.EqualFold(name, hint) {
			table = name
			break
		}
		if table == "" && strings.Contains(strings.ToLower(name), hint) {
			table = name
		}
	}
	if table == "" {
		return "", nil
	}

	colOut, err := exec.Command("sqlite3", dbPath, fmt.Sprintf(`PRAGMA table_info(%s);`, quoteIdent(table))).Output()
	if err != nil {
		return table, nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(colOut)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return table, cols
}

// firstColumn returns the first candidate (case-insensitively) present in cols.
func firstColumn(cols []string, candidates ...string) string {
	set := make(map[string]string, len(cols))
	for _, c := range cols {
		set[strings.ToLower(c)] = c
	}
	for _, cand := range candidates {
		if actual, ok := set[strings.ToLower(cand)]; ok {
			return actual
		}
	}
	return ""
}

// quoteIdent quotes a SQL identifier (table/column name) for safe interpolation.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqlQuote quotes a SQL string literal for safe interpolation.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// parseFlexibleTime tries a handful of timestamp formats used across
// OpenCode versions, including integer Unix epoch seconds/milliseconds.
func parseFlexibleTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
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
