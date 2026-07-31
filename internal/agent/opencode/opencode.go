```go
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
// OpenCode's on-disk session layout (project keying scheme, flat-file vs
// SQLite backend, and SQLite column names) has changed across releases, so
// discovery here is deliberately layout-tolerant rather than hardcoded to a
// single known-good version's format.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (pre-v1.2 OpenCode, and any version that
	// still keeps a flat-file mirror alongside SQLite).
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

// flatSessionCandidate describes a session JSON file found on disk.
type flatSessionCandidate struct {
	sessionID  string
	modTime    time.Time
	matchesDir bool
}

// discoverFromFlatFiles tries flat file session discovery. It no longer
// assumes OpenCode nests session files under a directory literally named
// after our computed project ID (that naming/keying scheme has changed
// across OpenCode versions) — instead it scans every project directory and
// prefers sessions whose own recorded working directory matches ours,
// falling back to the most recently modified session otherwise.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	baseDir := filepath.Join(dataDir, "storage", "session")
	projectDirs, err := os.ReadDir(baseDir)
	if err != nil {
		return nil, nil
	}

	var best *flatSessionCandidate
	for _, projectDir := range projectDirs {
		if !projectDir.IsDir() {
			continue
		}
		dirPath := filepath.Join(baseDir, projectDir.Name())
		for _, cand := range flatSessionCandidatesInDir(dirPath, projectPath) {
			cand := cand
			if best == nil || betterFlatCandidate(cand, *best) {
				best = &cand
			}
		}
	}

	if best == nil {
		return nil, nil
	}

	// The transcript path for OpenCode is the message directory
	msgDir, _ := GetMessageDir(best.sessionID)

	return &agent.SessionInfo{
		SessionID:      best.sessionID,
		TranscriptPath: msgDir,
		StartedAt:      best.modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// betterFlatCandidate reports whether c is a better discovery match than
// current: an exact working-directory match always wins, otherwise the most
// recently modified session wins.
func betterFlatCandidate(c, current flatSessionCandidate) bool {
	if c.matchesDir != current.matchesDir {
		return c.matchesDir
	}
	return c.modTime.After(current.modTime)
}

// flatSessionCandidatesInDir returns recent session JSON files in dir,
// annotated with whether their recorded directory/cwd/worktree field
// matches projectPath.
func flatSessionCandidatesInDir(dir, projectPath string) []flatSessionCandidate {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	now := time.Now()
	var out []flatSessionCandidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			continue
		}

		matchesDir := false
		if data, err := os.ReadFile(filepath.Join(dir, entry.Name())); err == nil {
			matchesDir = sessionJSONMatchesDir(data, projectPath)
		}

		out = append(out, flatSessionCandidate{
			sessionID:  strings.TrimSuffix(entry.Name(), ".json"),
			modTime:    modTime,
			matchesDir: matchesDir,
		})
	}
	return out
}

// sessionJSONMatchesDir checks whether a session JSON blob records a
// directory/cwd/worktree/path field equal to projectPath. Field naming for
// this has varied across OpenCode releases, so several candidates are tried.
func sessionJSONMatchesDir(data []byte, projectPath string) bool {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return false
	}
	for _, key := range []string{"directory", "worktree", "cwd", "path"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) == nil && s != "" && agent.PathsEqual(s, projectPath) {
			return true
		}
	}
	return false
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session belonging to this project. Column names are discovered at
// runtime via PRAGMA table_info rather than hardcoded, since OpenCode's
// SQLite schema has changed between releases (e.g. project association and
// timestamp column naming).
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
	idCol := firstMatchingColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := firstMatchingColumn(sessionCols, "project_id", "projectid", "project")
	dirCol := firstMatchingColumn(sessionCols, "directory", "worktree", "cwd", "path")
	timeCol := firstMatchingColumn(sessionCols, "time_updated", "timeupdated", "updated_at", "updated", "time_created", "created_at", "createdat")

	orderBy := ""
	if timeCol != "" {
		orderBy = fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}

	sessionID := ""

	// Prefer matching by our computed project ID (git root commit scheme).
	if projectCol != "" {
		q := fmt.Sprintf("SELECT %s FROM session WHERE %s='%s'%s LIMIT 1;", idCol, projectCol, projectID, orderBy)
		if out, err := sqliteQuery(dbPath, q); err == nil && out != "" {
			sessionID = out
		}
	}

	// Fall back to matching by the session's recorded working directory —
	// OpenCode's project-identification scheme isn't guaranteed to be a git
	// root commit hash across versions.
	if sessionID == "" && dirCol != "" {
		q := fmt.Sprintf("SELECT %s FROM session WHERE %s='%s'%s LIMIT 1;", idCol, dirCol, projectPath, orderBy)
		if out, err := sqliteQuery(dbPath, q); err == nil && out != "" {
			sessionID = out
		}
	}

	// Last resort: the single most recently updated session in the database.
	if sessionID == "" {
		q := fmt.Sprintf("SELECT %s FROM session%s LIMIT 1;", idCol, orderBy)
		if out, err := sqliteQuery(dbPath, q); err == nil && out != "" {
			sessionID = out
		}
	}

	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout)
	if timeCol != "" {
		q := fmt.Sprintf("SELECT %s FROM session WHERE %s='%s';", timeCol, idCol, sessionID)
		if out, err := sqliteQuery(dbPath, q); err == nil && out != "" {
			if t, ok := parseFlexibleTime(out); ok {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
			// If we can't parse the time, proceed anyway — better to try than skip
		}
	}

	transcriptData := sqliteSessionMessages(dbPath, sessionID)

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// sqliteSessionMessages fetches all messages for a session as a JSON array,
// resolving the message table's column names at runtime.
func sqliteSessionMessages(dbPath, sessionID string) []byte {
	msgCols := sqliteTableColumns(dbPath, "message")
	msgIDCol := firstMatchingColumn(msgCols, "id")
	msgSessionCol := firstMatchingColumn(msgCols, "session_id", "sessionid")
	msgDataCol := firstMatchingColumn(msgCols, "data", "content", "body")
	if msgIDCol == "" || msgSessionCol == "" || msgDataCol == "" {
		return nil
	}

	orderClause := ""
	if timeCol := firstMatchingColumn(msgCols, "time_created", "timecreated", "created_at", "created"); timeCol != "" {
		orderClause = " ORDER BY " + timeCol
	}

	q := fmt.Sprintf(
		"SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM message WHERE %s='%s'%s;",
		msgDataCol, msgIDCol, msgSessionCol, sessionID, orderClause,
	)
	out, err := sqliteQuery(dbPath, q)
	if err != nil {
		return nil
	}

	// sqlite3 returns "[null]" when no rows match
	if out == "" || out == "[null]" || out == "[]" {
		return nil
	}
	return []byte(out)
}

// sqliteQuery runs a single query via the sqlite3 CLI and returns trimmed stdout.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// sqliteTableColumns returns the column names of a SQLite table via PRAGMA
// table_info, or nil if the table doesn't exist or sqlite3 fails.
func sqliteTableColumns(dbPath, table string) []string {
	out, err := sqliteQuery(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil || out == "" {
		return nil
	}
	var cols []string
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "|")
		if len(parts) >= 2 && parts[1] != "" {
			cols = append(cols, parts[1])
		}
	}
	return cols
}

// firstMatchingColumn returns the actual column name (from cols) matching
// the first candidate name, comparing case- and underscore-insensitively so
// that e.g. "project_id", "projectId", and "PROJECTID" are all treated the
// same way.
func firstMatchingColumn(cols []string, candidates ...string) string {
	normalize := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(s, "_", ""))
	}
	byNormalized := make(map[string]string, len(cols))
	for _, c := range cols {
		byNormalized[normalize(c)] = c
	}
	for _, cand := range candidates {
		if actual, ok := byNormalized[normalize(cand)]; ok {
			return actual
		}
	}
	return ""
}

// parseFlexibleTime parses a timestamp value returned by sqlite3, which may
// be formatted as an RFC3339-ish string or as an integer epoch (seconds,
// milliseconds, or nanoseconds).
func parseFlexibleTime(s string) (time.Time, bool) {
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

	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		switch {
		case n > 1e17: // nanoseconds
			return time.Unix(0, n), true
		case n > 1e14: // microseconds
			return time.Unix(0, n*1e3), true
		case n > 1e11: // milliseconds
			return time.Unix(0, n*1e6), true
		default: // seconds
			return time.Unix(n, 0), true
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
```
