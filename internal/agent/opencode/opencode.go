```go
package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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
// It first tries flat file storage, then falls back to SQLite. OpenCode has
// changed its on-disk storage layout and schema across releases, so both
// discovery paths are written to tolerate those changes rather than assume
// one fixed shape.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (pre-v1.2 OpenCode, and later releases
	// that still keep one JSON file per session under storage/session).
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite (some OpenCode v1.2+ releases).
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// discoverFromFlatFiles scans OpenCode's flat-file session storage for the
// most recent session belonging to this project. The exact directory shape
// (flat vs. nested under a project ID) has varied across OpenCode releases,
// so this walks the whole "storage/session" tree instead of assuming one
// fixed path, and matches sessions to the current project by content (the
// session's "directory" or "projectID" field) rather than by directory name.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	if info, err := os.Stat(sessionRoot); err != nil || !info.IsDir() {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout

	var bestSessionID, bestPath string
	var bestModTime time.Time
	var bestMatchesProject bool

	_ = filepath.WalkDir(sessionRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > recentTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var sess sessionInfo
		if err := json.Unmarshal(data, &sess); err != nil {
			return nil
		}

		sessionID := sess.ID
		if sessionID == "" {
			sessionID = strings.TrimSuffix(d.Name(), ".json")
		}

		matchesProject := (sess.Directory != "" && agent.PathsEqual(sess.Directory, projectPath)) ||
			(sess.ProjectID != "" && sess.ProjectID == projectID)

		// Prefer sessions that match this project. Among sessions with the
		// same match status, prefer the most recently modified one.
		better := bestSessionID == ""
		if !better {
			if matchesProject != bestMatchesProject {
				better = matchesProject
			} else {
				better = modTime.After(bestModTime)
			}
		}

		if better {
			bestSessionID = sessionID
			bestPath = path
			bestModTime = modTime
			bestMatchesProject = matchesProject
		}
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: resolveTranscriptPath(dataDir, bestSessionID, bestPath),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// resolveTranscriptPath locates the message data for a discovered session.
// OpenCode has stored messages under a few different conventions across
// releases; try each known layout and fall back to the session's own info
// file (which always exists and is readable) so a note can still be stored
// even when the message layout isn't one we recognize.
func resolveTranscriptPath(dataDir, sessionID, sessionFilePath string) string {
	candidates := []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", sessionID, "message"),
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return sessionFilePath
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session. Table and column names have changed across OpenCode
// releases, so the schema is introspected via sqlite_master/PRAGMA
// table_info instead of assuming fixed names.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := firstExistingTable(dbPath, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}
	messageTable := firstExistingTable(dbPath, "message", "messages")
	if messageTable == "" {
		return nil, nil
	}

	sessCols := sqliteColumns(dbPath, sessionTable)
	idCol := pickColumn(sessCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := pickColumn(sessCols, "project_id", "projectid", "project")
	dirCol := pickColumn(sessCols, "directory", "cwd", "path")
	timeCol := pickColumn(sessCols, "time_updated", "updated", "time")

	orderBy := idCol
	if timeCol != "" {
		orderBy = timeCol
	}

	var sessionID string
	if projectCol != "" {
		sessionID = querySQLiteValue(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
			idCol, sessionTable, projectCol, sqlEscape(projectID), orderBy))
	}
	if sessionID == "" && dirCol != "" {
		sessionID = querySQLiteValue(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
			idCol, sessionTable, dirCol, sqlEscape(projectPath), orderBy))
	}
	if sessionID == "" {
		// Last resort: most recent session regardless of project, in case
		// this OpenCode release no longer exposes a column we can scope by.
		sessionID = querySQLiteValue(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s ORDER BY %s DESC LIMIT 1;`, idCol, sessionTable, orderBy))
	}
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout), when a usable
	// timestamp column is available.
	if timeCol != "" {
		timeStr := querySQLiteValue(dbPath, fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s='%s';`, timeCol, sessionTable, idCol, sqlEscape(sessionID)))
		if timeStr != "" && !isRecentTimestamp(timeStr) {
			return nil, nil
		}
	}

	msgCols := sqliteColumns(dbPath, messageTable)
	msgIDCol := pickColumn(msgCols, "id")
	sessionRefCol := pickColumn(msgCols, "session_id", "sessionid", "session")
	dataCol := pickColumn(msgCols, "data", "content", "body")
	msgTimeCol := pickColumn(msgCols, "time_created", "created", "time")

	if sessionRefCol == "" || dataCol == "" {
		// Found a session but can't locate its messages under any known
		// schema; still report the session so a note gets stored.
		return &agent.SessionInfo{
			SessionID:      sessionID,
			StartedAt:      time.Now().Format(time.RFC3339),
			ProjectPath:    projectPath,
			TranscriptData: []byte("[]"),
		}, nil
	}

	patchExpr := dataCol
	if msgIDCol != "" {
		patchExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, msgIDCol)
	}
	orderClause := ""
	if msgTimeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s", msgTimeCol)
	}
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM %s WHERE %s='%s'%s;`,
		patchExpr, messageTable, sessionRefCol, sqlEscape(sessionID), orderClause)

	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" (or an empty result) when no rows match
	if len(transcriptData) == 0 || string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
		transcriptData = []byte("[]")
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// firstExistingTable returns the first candidate name that exists as a
// table in the SQLite database, or "" if none do.
func firstExistingTable(dbPath string, candidates ...string) string {
	for _, name := range candidates {
		out := querySQLiteValue(dbPath, fmt.Sprintf(
			`SELECT name FROM sqlite_master WHERE type='table' AND name='%s';`, name))
		if out == name {
			return name
		}
	}
	return ""
}

// sqliteColumns returns the column names of a SQLite table via PRAGMA
// table_info.
func sqliteColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// pickColumn returns the first column matching (case-insensitively) one of
// the preferred names, falling back to a substring match if no exact match
// is found. Returns "" if nothing matches.
func pickColumn(cols []string, preferred ...string) string {
	lower := make(map[string]string, len(cols))
	for _, c := range cols {
		lower[strings.ToLower(c)] = c
	}
	for _, p := range preferred {
		if c, ok := lower[strings.ToLower(p)]; ok {
			return c
		}
	}
	for _, p := range preferred {
		for lc, c := range lower {
			if strings.Contains(lc, strings.ToLower(p)) {
				return c
			}
		}
	}
	return ""
}

// querySQLiteValue runs a query expected to return a single scalar value.
func querySQLiteValue(dbPath, query string) string {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// sqlEscape escapes single quotes for safe interpolation into a SQLite
// string literal.
func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// isRecentTimestamp reports whether a timestamp string (in one of several
// formats OpenCode has used) falls within RecentSessionTimeout. If the
// timestamp can't be parsed, it's treated as recent — better to try than to
// skip a real session.
func isRecentTimestamp(timeStr string) bool {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}
	return true
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
