package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// nonShellTools are OpenCode tool names known NOT to execute arbitrary shell
// commands. Any other tool name — including ones OpenCode adds or renames
// across releases for its shell/bash tool — is treated as potentially
// shell-capable and is checked against the command text itself. This mirrors
// the shiftlog plugin (plugin.go), which extracts commands from output.args
// without gating on the tool name at all, so a renamed shell tool doesn't
// silently break commit detection.
var nonShellTools = map[string]bool{
	"write":     true,
	"read":      true,
	"edit":      true,
	"grep":      true,
	"glob":      true,
	"webfetch":  true,
	"todowrite": true,
	"todoread":  true,
	"task":      true,
}

// IsCommitCommand checks if a tool invocation represents a git commit.
func (a *Agent) IsCommitCommand(toolName, command string) bool {
	if nonShellTools[toolName] {
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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session belonging to the given project.
//
// Table and column names are resolved dynamically via schema introspection
// (sqlite_master / PRAGMA table_info) rather than hardcoded, since OpenCode
// has renamed identifiers across releases in the past (e.g. adopting SQLite
// itself as of v1.2). This keeps discovery working across such renames
// (singular/plural table names, snake_case vs camelCase columns) without
// needing to track OpenCode's exact internal schema per version.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable := findTable(dbPath, "session")
	if sessionTable == "" {
		return nil, nil
	}
	messageTable := findTable(dbPath, "message")
	if messageTable == "" {
		return nil, nil
	}

	sessionCols := tableColumns(dbPath, sessionTable)
	idCol := pickColumn(sessionCols, "id", "session_id", "sessionid")
	if idCol == "" {
		return nil, nil
	}
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project")
	dirCol := pickColumn(sessionCols, "directory", "dir", "cwd", "path", "worktree")
	updatedCol := pickColumn(sessionCols, "time_updated", "updated_at", "timeupdated", "updated", "updatedat")

	// Find the most recent session for this project. OpenCode has used both
	// a git-root-commit-hash project ID and (in some versions) the worktree
	// directory to scope sessions to a project, so try both.
	sessionID := findSessionID(dbPath, sessionTable, idCol, updatedCol, projectCol, projectID)
	if sessionID == "" {
		sessionID = findSessionID(dbPath, sessionTable, idCol, updatedCol, dirCol, projectPath)
	}
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout), when we know which
	// column holds the update time.
	if updatedCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`,
			quoteIdent(updatedCol), quoteIdent(sessionTable), quoteIdent(idCol), escapeSQL(sessionID))
		cmd := exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			timeStr := strings.TrimSpace(string(timeOutput))
			if t, ok := parseOpenCodeTime(timeStr); ok {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
			// If we can't parse the time, proceed anyway — better to try than skip.
		}
	}

	messageCols := tableColumns(dbPath, messageTable)
	msgSessionCol := pickColumn(messageCols, "session_id", "sessionid", "session")
	msgDataCol := pickColumn(messageCols, "data", "content", "json", "body")
	if msgSessionCol == "" || msgDataCol == "" {
		return nil, nil
	}
	msgTimeCol := pickColumn(messageCols, "time_created", "created_at", "timecreated", "created")
	msgIDCol := pickColumn(messageCols, "id", "message_id", "messageid")

	// Get messages for this session as a JSON array
	selectExpr := quoteIdent(msgDataCol)
	if msgIDCol != "" {
		selectExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", quoteIdent(msgDataCol), quoteIdent(msgIDCol))
	}
	orderClause := ""
	if msgTimeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s", quoteIdent(msgTimeCol))
	}

	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM %s WHERE %s='%s'%s;`,
		selectExpr, quoteIdent(messageTable), quoteIdent(msgSessionCol), escapeSQL(sessionID), orderClause,
	)
	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
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

// findSessionID returns the most recently updated session ID from the given
// table whose filterCol equals filterValue. Returns "" if filterCol or
// filterValue is empty, or if the query fails or matches nothing.
func findSessionID(dbPath, table, idCol, updatedCol, filterCol, filterValue string) string {
	if filterCol == "" || filterValue == "" {
		return ""
	}

	orderClause := ""
	if updatedCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", quoteIdent(updatedCol))
	}

	query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s'%s LIMIT 1;`,
		quoteIdent(idCol), quoteIdent(table), quoteIdent(filterCol), escapeSQL(filterValue), orderClause)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// findTable returns the name of the first table in the database matching
// base, either exactly or in pluralized form (case-insensitive), e.g. "session"
// also matches "sessions". Returns "" if no matching table is found.
func findTable(dbPath, base string) string {
	cmd := exec.Command("sqlite3", dbPath, `SELECT name FROM sqlite_master WHERE type='table';`)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	base = strings.ToLower(base)
	var pluralMatch string
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if lower == base {
			return name
		}
		if lower == base+"s" || lower == base+"es" {
			pluralMatch = name
		}
	}
	return pluralMatch
}

// tableColumns returns the column names of a table via PRAGMA table_info.
func tableColumns(dbPath, table string) []string {
	query := fmt.Sprintf(`PRAGMA table_info(%s);`, quoteIdent(table))
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) > 1 && fields[1] != "" {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// pickColumn returns the first column in columns matching one of candidates.
// Matching is case-insensitive and ignores underscores, so "project_id",
// "projectId", and "projectID" are all treated as equivalent. Returns "" if
// none match.
func pickColumn(columns []string, candidates ...string) string {
	normalized := make(map[string]string, len(columns))
	for _, c := range columns {
		normalized[normalizeIdent(c)] = c
	}
	for _, cand := range candidates {
		if col, ok := normalized[normalizeIdent(cand)]; ok {
			return col
		}
	}
	return ""
}

func normalizeIdent(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "_", "")
}

var identPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// quoteIdent quotes a SQL identifier discovered from the database schema
// itself (via sqlite_master / PRAGMA table_info), not from user input. Plain
// identifiers are passed through unquoted; anything unusual is double-quoted.
func quoteIdent(name string) string {
	if identPattern.MatchString(name) {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// escapeSQL escapes single quotes for use inside a SQL string literal.
func escapeSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// parseOpenCodeTime tries the timestamp formats OpenCode has used for
// session update times across versions.
func parseOpenCodeTime(s string) (time.Time, bool) {
	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
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
