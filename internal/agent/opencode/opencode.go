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
//
// OpenCode's on-disk storage layout and SQLite schema have both changed
// across releases (directory nesting, column renames, etc.), so this tries
// several strategies in order of specificity and falls through to the next
// whenever one comes up empty rather than erroring out:
//  1. Flat file storage grouped by project (older/legacy layout).
//  2. SQLite (opencode.db), with columns discovered dynamically so column
//     renames between releases don't break discovery.
//  3. A generic scan of the data directory for a recently written
//     session-shaped JSON file, which tolerates layout changes that neither
//     of the above account for.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (pre-v1.2 OpenCode, and any layout that
	// still groups sessions under storage/session/<projectID>/).
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	// Fall back to SQLite (OpenCode v1.2+), tolerating schema changes.
	projectID := GetProjectID(projectPath)
	if sqliteSession, sqliteErr := discoverFromSQLite(dataDir, projectID, projectPath); sqliteErr == nil && sqliteSession != nil {
		return sqliteSession, nil
	}

	// Last resort: scan the whole data directory for a recently written
	// session file without assuming a specific directory layout.
	return discoverFromDataDirScan(dataDir, projectPath)
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

// sqliteTableColumns returns the column names for a table via
// PRAGMA table_info, or nil if the table doesn't exist or can't be queried.
func sqliteTableColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 && fields[1] != "" {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// pickColumn returns the first column (case-insensitive) matching one of
// the preferred names, or "" if none match.
func pickColumn(cols []string, preferred ...string) string {
	byLower := make(map[string]string, len(cols))
	for _, c := range cols {
		byLower[strings.ToLower(c)] = c
	}
	for _, p := range preferred {
		if c, ok := byLower[p]; ok {
			return c
		}
	}
	return ""
}

// sqliteEscape escapes single quotes for inclusion in a single-quoted
// SQLite string literal.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// parseSQLiteTime parses a timestamp value from SQLite in any of the
// formats OpenCode has used across releases, including epoch seconds,
// milliseconds, and microseconds.
func parseSQLiteTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}

	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}

	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		switch {
		case n > 1e15: // microseconds
			return time.UnixMicro(n), true
		case n > 1e12: // milliseconds
			return time.UnixMilli(n), true
		default: // seconds
			return time.Unix(n, 0), true
		}
	}

	return time.Time{}, false
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session. Column names are discovered dynamically via
// PRAGMA table_info since OpenCode's schema has changed between releases
// (e.g. project_id/directory renames), and the project filter falls back
// to an unfiltered "most recent session" query if no row matches, so a
// naming mismatch degrades gracefully instead of finding nothing.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionCols := sqliteTableColumns(dbPath, "session")
	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := pickColumn(sessionCols, "time_updated", "updated", "time_created", "created", "time", "timestamp")
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project", "directory", "cwd", "path")

	orderBy := idCol
	if timeCol != "" {
		orderBy = timeCol
	}

	var sessionID string

	// Try filtering by project first, using whichever value matches the
	// column's apparent semantics (a hash-like project ID vs. an absolute
	// directory path).
	if projectCol != "" {
		compareVal := projectID
		switch projectCol {
		case "directory", "cwd", "path":
			compareVal = projectPath
		}
		sessionQuery := fmt.Sprintf(
			`SELECT %s FROM session WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
			idCol, projectCol, sqliteEscape(compareVal), orderBy,
		)
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
		if out, err := cmd.Output(); err == nil {
			sessionID = strings.TrimSpace(string(out))
		}
	}

	// If project filtering found nothing (e.g. the column holds a value we
	// didn't guess correctly), fall back to the single most recent session
	// overall rather than reporting no session at all.
	if sessionID == "" {
		sessionQuery := fmt.Sprintf(`SELECT %s FROM session ORDER BY %s DESC LIMIT 1;`, idCol, orderBy)
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return nil, nil
		}
		sessionID = strings.TrimSpace(string(out))
	}

	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout)
	if timeCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM session WHERE %s='%s';`, timeCol, idCol, sqliteEscape(sessionID))
		cmd := exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			if t, ok := parseSQLiteTime(string(timeOutput)); ok {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
			// If we can't parse the time, proceed anyway — better to try than skip
		}
	}

	// Get messages for this session as a JSON array
	msgCols := sqliteTableColumns(dbPath, "message")
	msgIDCol := pickColumn(msgCols, "id")
	msgSessionCol := pickColumn(msgCols, "session_id", "sessionid", "session")
	msgDataCol := pickColumn(msgCols, "data", "content", "body")
	msgTimeCol := pickColumn(msgCols, "time_created", "created", "time", "timestamp")

	if msgIDCol == "" || msgSessionCol == "" || msgDataCol == "" {
		// No usable message table — still report the session so the caller
		// has a session ID to work with instead of losing it entirely.
		return &agent.SessionInfo{
			SessionID:   sessionID,
			StartedAt:   time.Now().Format(time.RFC3339),
			ProjectPath: projectPath,
		}, nil
	}

	msgOrderBy := msgIDCol
	if msgTimeCol != "" {
		msgOrderBy = msgTimeCol
	}
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM message WHERE %s='%s' ORDER BY %s;`,
		msgDataCol, msgIDCol, msgSessionCol, sqliteEscape(sessionID), msgOrderBy,
	)
	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if len(transcriptData) == 0 || string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
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

// discoverFromDataDirScan is a last-resort fallback that walks the entire
// OpenCode data directory looking for a recently-written session file,
// without assuming a specific directory layout. This tolerates upstream
// storage-format changes that the flat-file and SQLite strategies above
// don't account for.
func discoverFromDataDirScan(dataDir, projectPath string) (*agent.SessionInfo, error) {
	storageRoot := filepath.Join(dataDir, "storage")
	if info, err := os.Stat(storageRoot); err != nil || !info.IsDir() {
		return nil, nil
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout

	var bestID string
	var bestPath string
	var bestModTime time.Time

	_ = filepath.WalkDir(storageRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
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
		if bestPath != "" && !modTime.After(bestModTime) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil
		}

		idRaw, ok := raw["id"]
		if !ok {
			return nil
		}
		var id string
		if err := json.Unmarshal(idRaw, &id); err != nil || id == "" {
			return nil
		}

		// Session-shaped files carry a title/directory/projectID field;
		// message-shaped files carry role/type/sessionID fields instead,
		// and we don't want to mistake a message file for a session.
		_, hasTitle := raw["title"]
		_, hasDirectory := raw["directory"]
		_, hasProjectID := raw["projectID"]
		if !hasTitle && !hasDirectory && !hasProjectID {
			return nil
		}

		bestID = id
		bestPath = path
		bestModTime = modTime
		return nil
	})

	if bestID == "" {
		return nil, nil
	}

	transcriptPath := findSessionMessages(storageRoot, bestID)
	if transcriptPath == "" {
		transcriptPath = bestPath
	}

	return &agent.SessionInfo{
		SessionID:      bestID,
		TranscriptPath: transcriptPath,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// findSessionMessages searches for a directory named after sessionID
// anywhere under storageRoot, which OpenCode uses to group a session's
// message files regardless of how deeply that directory is nested.
func findSessionMessages(storageRoot, sessionID string) string {
	var found string
	_ = filepath.WalkDir(storageRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			found = path
			return filepath.SkipDir
		}
		return nil
	})
	return found
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

	// Newer OpenCode releases split message metadata (role, id, time) from
	// content, which lives in a separate "parts" array of typed blocks.
	if partsRaw, ok := raw["parts"]; ok {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(partsRaw, &parts); err == nil && len(parts) > 0 {
			for _, p := range parts {
				if p.Type == "text" && p.Text != "" {
					msg.Content = append(msg.Content, agent.ContentBlock{Type: "text", Text: p.Text})
				}
			}
			if len(msg.Content) > 0 {
				return msg
			}
		}
	}

	return msg
}
