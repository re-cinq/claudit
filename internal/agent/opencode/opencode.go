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
//
// OpenCode's on-disk layout has changed across releases (project-scoped flat
// files, sessions nested in their own subdirectory, or no project scoping at
// all), so after the fast, exact path lookup we fall back to scanning the
// whole session storage tree for the most recently modified candidate.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}
	projectID := GetProjectID(projectPath)

	sessionDir, err := GetSessionDir(projectPath)
	if err == nil {
		if sessionID, _, modTime := scanForSessionFile(sessionDir, "", 1); sessionID != "" {
			return &agent.SessionInfo{
				SessionID:      sessionID,
				TranscriptPath: resolveMessageDir(dataDir, sessionID),
				StartedAt:      modTime.Format(time.RFC3339),
				ProjectPath:    projectPath,
			}, nil
		}
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	sessionID, _, modTime := scanForSessionFile(sessionRoot, projectID, 3)
	if sessionID == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: resolveMessageDir(dataDir, sessionID),
		StartedAt:      modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// scanForSessionFile recursively searches dir (up to maxDepth levels) for the
// most recently modified session JSON file within the recent-session
// timeout. It prefers paths containing preferredComponent (typically the
// computed project ID) but falls back to the most recent file overall when
// nothing matches, since the project-scoping scheme itself may have changed.
func scanForSessionFile(dir, preferredComponent string, maxDepth int) (sessionID, path string, modTime time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", time.Time{}
	}

	now := time.Now()
	var bestID, bestPath string
	var bestMod time.Time
	var bestPreferred bool

	consider := func(id, p string, mt time.Time) {
		if now.Sub(mt) > agent.RecentSessionTimeout {
			return
		}
		preferred := preferredComponent != "" && strings.Contains(p, preferredComponent)
		if bestPath == "" || (preferred && !bestPreferred) || (preferred == bestPreferred && mt.After(bestMod)) {
			bestID, bestPath, bestMod, bestPreferred = id, p, mt, preferred
		}
	}

	for _, e := range entries {
		full := filepath.Join(dir, e.Name())

		if e.IsDir() {
			if maxDepth <= 0 {
				continue
			}
			if id, p, mt := scanForSessionFile(full, preferredComponent, maxDepth-1); p != "" {
				consider(id, p, mt)
			}
			continue
		}

		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		id := strings.TrimSuffix(e.Name(), ".json")
		switch id {
		case "info", "session", "meta", "data":
			// Generic filenames mean the session ID lives in the parent
			// directory name rather than the file name.
			id = filepath.Base(dir)
		}
		consider(id, full, info.ModTime())
	}

	return bestID, bestPath, bestMod
}

// resolveMessageDir locates the message directory for a session, falling
// back to a recursive search when the direct sessionID-keyed path (the
// historical layout) doesn't exist, e.g. because messages are now nested
// under a project ID as well.
func resolveMessageDir(dataDir, sessionID string) string {
	msgDir, err := GetMessageDir(sessionID)
	if err == nil {
		if info, statErr := os.Stat(msgDir); statErr == nil && info.IsDir() {
			return msgDir
		}
	}

	root := filepath.Join(dataDir, "storage", "message")
	if found := findDirNamed(root, sessionID, 3); found != "" {
		return found
	}

	return msgDir
}

// findDirNamed recursively searches root (up to maxDepth levels) for a
// directory named exactly name.
func findDirNamed(root, name string, maxDepth int) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		full := filepath.Join(root, e.Name())
		if e.Name() == name {
			return full
		}
		if maxDepth > 0 {
			if found := findDirNamed(full, name, maxDepth-1); found != "" {
				return found
			}
		}
	}

	return ""
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
//
// Table and column names are resolved dynamically via sqlite_master/
// PRAGMA table_info rather than hardcoded, since OpenCode's SQLite schema
// has changed names across releases (e.g. project_id vs projectID vs
// directory, or message vs part tables).
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := findSQLiteTable(dbPath, []string{"session", "sessions"})
	if sessionTable == "" {
		return nil, nil
	}
	sessionCols := sqliteTableColumns(dbPath, sessionTable)
	idCol := pickColumn(sessionCols, []string{"id", "session_id", "sessionID"})
	if idCol == "" {
		return nil, nil
	}
	projectCol := pickColumn(sessionCols, []string{"project_id", "projectID", "project", "directory", "worktree", "worktree_id"})
	timeCol := pickColumn(sessionCols, []string{"time_updated", "updated", "timeUpdated", "updated_at", "time_created", "created", "createdAt"})

	orderBy := "rowid DESC"
	if timeCol != "" {
		orderBy = quoteIdent(timeCol) + " DESC"
	}

	// Find the most recent session for this project.
	var sessionID string
	if projectCol != "" {
		query := fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s=%s ORDER BY %s LIMIT 1;`,
			quoteIdent(idCol), quoteIdent(sessionTable), quoteIdent(projectCol), sqlQuote(projectID), orderBy,
		)
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		if output, err := cmd.Output(); err == nil {
			sessionID = strings.TrimSpace(string(output))
		}
	}

	if sessionID == "" {
		// Either there's no project column, or it didn't match our computed
		// project ID scheme (e.g. it stores an absolute path instead of a
		// git root commit hash) — fall back to the most recent session
		// overall.
		query := fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s LIMIT 1;`, quoteIdent(idCol), quoteIdent(sessionTable), orderBy)
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		output, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(output)) == "" {
			return nil, nil
		}
		sessionID = strings.TrimSpace(string(output))
	}

	// Check if this session was recent (within timeout).
	if timeCol != "" {
		query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s=%s;`, quoteIdent(timeCol), quoteIdent(sessionTable), quoteIdent(idCol), sqlQuote(sessionID))
		cmd := exec.Command("sqlite3", dbPath, query)
		if timeOutput, err := cmd.Output(); err == nil {
			if t, ok := parseFlexibleTime(strings.TrimSpace(string(timeOutput))); ok {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
			// If we can't parse the time, proceed anyway — better to try than skip.
		}
	}

	msgTable := findSQLiteTable(dbPath, []string{"message", "messages", "part", "parts"})
	if msgTable == "" {
		return nil, nil
	}
	msgCols := sqliteTableColumns(dbPath, msgTable)
	msgSessionCol := pickColumn(msgCols, []string{"session_id", "sessionID", "session"})
	if msgSessionCol == "" {
		return nil, nil
	}
	msgOrderCol := pickColumn(msgCols, []string{"time_created", "created", "createdAt", "time_updated", "updated"})

	orderClause := " ORDER BY rowid"
	if msgOrderCol != "" {
		orderClause = " ORDER BY " + quoteIdent(msgOrderCol)
	}

	msgQuery := fmt.Sprintf(`SELECT * FROM %s WHERE %s=%s%s;`, quoteIdent(msgTable), quoteIdent(msgSessionCol), sqlQuote(sessionID), orderClause)
	cmd := exec.Command("sqlite3", "-json", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := mergeSQLiteMessageRows(msgOutput)
	if transcriptData == nil {
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

// findSQLiteTable returns the first candidate table name that exists in the
// database, or "" if none do.
func findSQLiteTable(dbPath string, candidates []string) string {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}

	existing := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			existing[name] = true
		}
	}

	for _, c := range candidates {
		if existing[c] {
			return c
		}
	}
	return ""
}

// sqliteTableColumns returns the column names of a table via PRAGMA table_info.
func sqliteTableColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// pickColumn returns the first candidate present in cols, or "" if none match.
func pickColumn(cols []string, candidates []string) string {
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

// quoteIdent quotes a SQL identifier (table/column name) for use in a query.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqlQuote quotes a SQL string literal.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// parseFlexibleTime attempts several timestamp encodings OpenCode has used
// across releases: RFC3339(Nano), a millisecond-precision "Z" format, plain
// SQL datetime, and Unix epoch seconds/milliseconds/microseconds as an
// integer.
func parseFlexibleTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}

	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, true
		}
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		switch {
		case n > 1e15:
			return time.UnixMicro(n), true
		case n > 1e12:
			return time.UnixMilli(n), true
		default:
			return time.Unix(n, 0), true
		}
	}

	return time.Time{}, false
}

// mergeSQLiteMessageRows normalizes `sqlite3 -json` output for the message
// table into a JSON array of message objects. Some schema versions store the
// full message as a JSON blob in a "data" column alongside id/session
// columns; others store role/content directly as columns. This merges any
// "data" blob into the row so downstream parsing sees a single flat object
// regardless of which layout is in effect. Returns nil if there are no rows
// or the output can't be parsed.
func mergeSQLiteMessageRows(raw []byte) []byte {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil
	}
	if len(rows) == 0 {
		return nil
	}

	merged := make([]map[string]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		entry := make(map[string]json.RawMessage, len(row))
		for k, v := range row {
			entry[k] = v
		}

		dataRaw, ok := row["data"]
		if !ok {
			merged = append(merged, entry)
			continue
		}

		var inner map[string]json.RawMessage
		if json.Unmarshal(dataRaw, &inner) == nil {
			for k, v := range inner {
				entry[k] = v
			}
		} else {
			var dataStr string
			if json.Unmarshal(dataRaw, &dataStr) == nil && dataStr != "" {
				if json.Unmarshal([]byte(dataStr), &inner) == nil {
					for k, v := range inner {
						entry[k] = v
					}
				}
			}
		}

		merged = append(merged, entry)
	}

	out, err := json.Marshal(merged)
	if err != nil {
		return nil
	}
	return out
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
