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
//
// OpenCode's on-disk storage has changed shape across releases (flat JSON
// files pre-v1.2, a SQLite database from v1.2, and project-scoped
// directories in later releases, sometimes with renamed columns/fields).
// Rather than hard-coding a single layout, discovery tries the known flat
// file layouts first, then SQLite with table/column names resolved at
// runtime, so it keeps working as long as OpenCode still records sessions
// somewhere on disk under the data directory.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)

	if session := discoverFromFlatFiles(dataDir, projectID, projectPath); session != nil {
		return session, nil
	}

	if session := discoverFromSQLite(dataDir, projectID, projectPath); session != nil {
		return session, nil
	}

	return nil, nil
}

// openCodeSessionLayout describes one possible on-disk location for a
// project's session index, and how to derive the message directory for a
// given session ID within that layout.
type openCodeSessionLayout struct {
	sessionDir func(dataDir, projectID string) string
	messageDir func(dataDir, projectID, sessionID string) string
}

var openCodeSessionLayouts = []openCodeSessionLayout{
	{
		// Pre-v1.2: storage/session/<projectID>/<sessionID>.json,
		// storage/message/<sessionID>/*.json
		sessionDir: func(dataDir, projectID string) string {
			return filepath.Join(dataDir, "storage", "session", projectID)
		},
		messageDir: func(dataDir, _, sessionID string) string {
			return filepath.Join(dataDir, "storage", "message", sessionID)
		},
	},
	{
		// Newer project-scoped layout: project/<projectID>/storage/session/<sessionID>.json,
		// project/<projectID>/storage/message/<sessionID>/*.json
		sessionDir: func(dataDir, projectID string) string {
			return filepath.Join(dataDir, "project", projectID, "storage", "session")
		},
		messageDir: func(dataDir, projectID, sessionID string) string {
			return filepath.Join(dataDir, "project", projectID, "storage", "message", sessionID)
		},
	},
}

// discoverFromFlatFiles tries flat-JSON-file session discovery across known
// OpenCode storage layouts, keyed by our own computed project ID. If that
// yields nothing, it falls back to scanning every project bucket and
// matching by the directory recorded inside each session file, in case this
// OpenCode version keys sessions differently than we assume.
func discoverFromFlatFiles(dataDir, projectID, projectPath string) *agent.SessionInfo {
	for _, layout := range openCodeSessionLayouts {
		dir := layout.sessionDir(dataDir, projectID)
		id, modTime, found := latestRecentJSONFile(dir)
		if !found {
			continue
		}
		return sessionInfoWithTranscript(id, layout.messageDir(dataDir, projectID, id), projectPath, modTime)
	}

	return discoverFlatFilesByDirectory(dataDir, projectPath)
}

// latestRecentJSONFile returns the session ID (filename without extension)
// and modification time of the most recently modified .json file directly
// inside dir, if any were modified within RecentSessionTimeout.
func latestRecentJSONFile(dir string) (id string, modTime time.Time, found bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}, false
	}

	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			continue
		}
		if !found || info.ModTime().After(modTime) {
			id = strings.TrimSuffix(entry.Name(), ".json")
			modTime = info.ModTime()
			found = true
		}
	}
	return id, modTime, found
}

// discoverFlatFilesByDirectory scans every project bucket under both known
// session-storage roots and returns the most recently modified session
// whose recorded directory/path/cwd field matches projectPath. This covers
// OpenCode versions that key sessions by something other than our computed
// project ID (e.g. a directory hash instead of the git root commit).
func discoverFlatFilesByDirectory(dataDir, projectPath string) *agent.SessionInfo {
	roots := []string{
		filepath.Join(dataDir, "storage", "session"),
		filepath.Join(dataDir, "project"),
	}

	absProjectPath, err := filepath.Abs(projectPath)
	if err != nil {
		absProjectPath = projectPath
	}

	var best *agent.SessionInfo
	var bestModTime time.Time

	for _, root := range roots {
		buckets, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, bucket := range buckets {
			if !bucket.IsDir() {
				continue
			}
			// The bucket may itself be the session dir (old layout:
			// storage/session/<projectID>) or a project dir containing a
			// nested storage/session (new layout: project/<projectID>).
			for _, dir := range []string{
				filepath.Join(root, bucket.Name()),
				filepath.Join(root, bucket.Name(), "storage", "session"),
			} {
				entries, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				for _, entry := range entries {
					if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
						continue
					}
					info, err := entry.Info()
					if err != nil || time.Since(info.ModTime()) > agent.RecentSessionTimeout {
						continue
					}
					if best != nil && !info.ModTime().After(bestModTime) {
						continue
					}
					data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
					if err != nil {
						continue
					}
					var meta struct {
						ID        string `json:"id"`
						Directory string `json:"directory"`
						Path      string `json:"path"`
						Cwd       string `json:"cwd"`
					}
					if err := json.Unmarshal(data, &meta); err != nil {
						continue
					}
					sessionDir := firstNonEmpty(meta.Directory, meta.Path, meta.Cwd)
					if sessionDir == "" || (sessionDir != projectPath && sessionDir != absProjectPath) {
						continue
					}
					id := meta.ID
					if id == "" {
						id = strings.TrimSuffix(entry.Name(), ".json")
					}
					messageDir := filepath.Join(filepath.Dir(dir), "message", id)
					best = sessionInfoWithTranscript(id, messageDir, projectPath, info.ModTime())
					bestModTime = info.ModTime()
				}
			}
		}
	}

	return best
}

// sessionInfoWithTranscript builds a SessionInfo pointing at transcriptPath
// if it exists, otherwise falls back to an empty inline transcript so a
// discovered session still results in a (possibly message-less) note
// instead of a hard failure reading a transcript path that doesn't exist.
func sessionInfoWithTranscript(sessionID, transcriptPath, projectPath string, modTime time.Time) *agent.SessionInfo {
	info := &agent.SessionInfo{
		SessionID:   sessionID,
		StartedAt:   modTime.Format(time.RFC3339),
		ProjectPath: projectPath,
	}
	if transcriptPath != "" {
		if _, err := os.Stat(transcriptPath); err == nil {
			info.TranscriptPath = transcriptPath
			return info
		}
	}
	info.TranscriptData = []byte("[]")
	return info
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// candidateSQLiteDBPaths returns known locations for OpenCode's SQLite
// database across versions.
func candidateSQLiteDBPaths(dataDir string) []string {
	return []string{
		filepath.Join(dataDir, "opencode.db"),
		filepath.Join(dataDir, "storage", "opencode.db"),
		filepath.Join(dataDir, "db", "opencode.db"),
	}
}

// discoverFromSQLite queries OpenCode's SQLite database for the most recent
// session belonging to this project. Table and column names are resolved
// dynamically (rather than hard-coded) since they have changed between
// OpenCode releases.
func discoverFromSQLite(dataDir, projectID, projectPath string) *agent.SessionInfo {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil
	}

	var dbPath string
	for _, candidate := range candidateSQLiteDBPaths(dataDir) {
		if _, err := os.Stat(candidate); err == nil {
			dbPath = candidate
			break
		}
	}
	if dbPath == "" {
		return nil
	}

	sessionTable := findSQLiteTable(dbPath, "session")
	if sessionTable == "" {
		return nil
	}
	sessionCols, err := sqliteTableColumns(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil
	}

	idCol := findColumn(sessionCols, "id")
	dirCol := findColumn(sessionCols, "", "directory", "path", "cwd")
	projCol := findColumn(sessionCols, "", "project", "worktree")
	timeCol := findColumn(sessionCols, "", "updated", "modified", "time")
	if idCol == "" {
		return nil
	}

	selectCols := []string{idCol}
	for _, c := range []string{dirCol, projCol, timeCol} {
		if c != "" {
			selectCols = append(selectCols, c)
		}
	}

	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(quoteIdents(selectCols), ", "), quoteIdent(sessionTable))
	if timeCol != "" {
		query += " ORDER BY " + quoteIdent(timeCol) + " DESC"
	}
	query += " LIMIT 25;"

	rows, err := runSQLiteJSON(dbPath, query)
	if err != nil || len(rows) == 0 {
		return nil
	}

	absProjectPath, err := filepath.Abs(projectPath)
	if err != nil {
		absProjectPath = projectPath
	}

	var chosen map[string]interface{}
	for _, row := range rows {
		if dirCol != "" {
			dir, _ := row[dirCol].(string)
			if dir == projectPath || dir == absProjectPath {
				chosen = row
				break
			}
			continue
		}
		if projCol != "" {
			pid, _ := row[projCol].(string)
			if pid == projectID {
				chosen = row
				break
			}
		}
	}
	if chosen == nil && dirCol == "" && projCol == "" {
		chosen = rows[0]
	}
	if chosen == nil {
		return nil
	}

	sessionID, _ := chosen[idCol].(string)
	if sessionID == "" {
		return nil
	}

	if timeCol != "" {
		if raw, ok := chosen[timeCol]; ok && !withinRecentTimeout(raw) {
			return nil
		}
	}

	transcriptData := fetchSQLiteMessages(dbPath, sessionID)
	if transcriptData == nil {
		transcriptData = []byte("[]")
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}
}

// fetchSQLiteMessages returns the messages for sessionID as a JSON array, or
// nil if they can't be determined (e.g. the message table/columns don't
// match any known shape).
func fetchSQLiteMessages(dbPath, sessionID string) []byte {
	messageTable := findSQLiteTable(dbPath, "message")
	if messageTable == "" {
		return nil
	}
	cols, err := sqliteTableColumns(dbPath, messageTable)
	if err != nil || len(cols) == 0 {
		return nil
	}

	idCol := findColumn(cols, "id")
	sessionCol := findColumn(cols, "", "session")
	dataCol := findColumn(cols, "", "data", "content", "body", "message")
	timeCol := findColumn(cols, "", "created", "time")
	if sessionCol == "" || dataCol == "" {
		return nil
	}

	valueExpr := quoteIdent(dataCol)
	if idCol != "" {
		valueExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", quoteIdent(dataCol), quoteIdent(idCol))
	}

	query := fmt.Sprintf("SELECT json_group_array(%s) FROM %s WHERE %s = %s",
		valueExpr, quoteIdent(messageTable), quoteIdent(sessionCol), sqlQuote(sessionID))
	if timeCol != "" {
		query += " ORDER BY " + quoteIdent(timeCol)
	}
	query += ";"

	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	data := strings.TrimSpace(string(output))
	if data == "" || data == "[null]" || data == "[]" {
		return nil
	}
	return []byte(data)
}

// runSQLiteJSON runs a sqlite3 query in JSON output mode and unmarshals the
// resulting rows.
func runSQLiteJSON(dbPath, query string) ([]map[string]interface{}, error) {
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

// findSQLiteTable returns the table whose name matches contains
// (case-insensitively, preferring an exact match).
func findSQLiteTable(dbPath, contains string) string {
	rows, err := runSQLiteJSON(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return ""
	}
	var fallback string
	for _, row := range rows {
		name, _ := row["name"].(string)
		if strings.EqualFold(name, contains) {
			return name
		}
		if fallback == "" && strings.Contains(strings.ToLower(name), contains) {
			fallback = name
		}
	}
	return fallback
}

// sqliteTableColumns returns the column names of table via PRAGMA table_info.
func sqliteTableColumns(dbPath, table string) ([]string, error) {
	rows, err := runSQLiteJSON(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(table)))
	if err != nil {
		return nil, err
	}
	cols := make([]string, 0, len(rows))
	for _, row := range rows {
		if name, ok := row["name"].(string); ok {
			cols = append(cols, name)
		}
	}
	return cols, nil
}

// findColumn returns the column matching exact (case-insensitive), or
// failing that, the first column containing one of the given substrings.
func findColumn(columns []string, exact string, contains ...string) string {
	if exact != "" {
		for _, c := range columns {
			if strings.EqualFold(c, exact) {
				return c
			}
		}
	}
	for _, sub := range contains {
		for _, c := range columns {
			if strings.Contains(strings.ToLower(c), sub) {
				return c
			}
		}
	}
	return ""
}

// withinRecentTimeout reports whether raw (a sqlite JSON value that may be a
// unix timestamp or a formatted string) is within RecentSessionTimeout of
// now. Unparseable values are treated as recent so we err on trying rather
// than skipping a real session.
func withinRecentTimeout(raw interface{}) bool {
	switch v := raw.(type) {
	case float64:
		t := time.Unix(int64(v), 0)
		if v > 1e10 { // looks like milliseconds, not seconds
			t = time.UnixMilli(int64(v))
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return true
		}
		layouts := []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05.000Z",
			"2006-01-02 15:04:05",
		}
		for _, layout := range layouts {
			if t, err := time.Parse(layout, s); err == nil {
				return time.Since(t) <= agent.RecentSessionTimeout
			}
		}
		return true
	default:
		return true
	}
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func quoteIdents(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = quoteIdent(n)
	}
	return out
}

func sqlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
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
