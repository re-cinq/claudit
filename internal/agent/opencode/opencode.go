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
// OpenCode's on-disk storage layout has changed across releases (flat JSON
// files directly under a per-project directory, sessions nested one level
// deeper, or a SQLite database with column/table names that don't match
// older releases). Rather than betting everything on one hard-coded layout,
// discovery tries the historically-known flat-file convention first, then a
// schema-introspecting SQLite query, and finally falls back to a resilient
// recursive scan of the data directory for the most recently modified
// session-like file. This keeps `shiftlog store --manual` working across
// OpenCode versions even when the exact storage format shifts.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (pre-v1.2 OpenCode, and known layout)
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

	projectID := GetProjectID(projectPath)

	// Fall back to SQLite (OpenCode v1.2+), introspecting the schema so we
	// don't depend on exact table/column names that may have changed.
	if session, err := discoverFromSQLite(dataDir, projectID, projectPath); err == nil && session != nil {
		return session, nil
	}

	// Last resort: recursively scan the data directory for a recently
	// modified session-like JSON file, in case this OpenCode version nests
	// storage differently than either of the layouts above expect.
	return discoverFromDataDirScan(dataDir, projectID, projectPath)
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

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session. Table and column names are discovered at query time
// (via sqlite_master / PRAGMA table_info) instead of being hard-coded, since
// they have changed between OpenCode releases.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, sessionCols, err := findTable(dbPath, "session")
	if err != nil || sessionTable == "" {
		return nil, nil
	}
	idCol := findColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := findColumn(sessionCols, "project")
	timeCol := findColumn(sessionCols, "updated")
	if timeCol == "" {
		timeCol = findColumn(sessionCols, "created")
	}

	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", quoteIdent(timeCol))
	}

	whereClause := ""
	if projectCol != "" {
		whereClause = fmt.Sprintf(" WHERE %s='%s'", quoteIdent(projectCol), escapeSQLLiteral(projectID))
	}

	sessionQuery := fmt.Sprintf("SELECT %s FROM %s%s%s LIMIT 1;",
		quoteIdent(idCol), quoteIdent(sessionTable), whereClause, orderClause)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Recency check is best-effort: an unparseable timestamp doesn't block
	// discovery, it just means we can't confirm freshness.
	if timeCol != "" {
		timeQuery := fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s';",
			quoteIdent(timeCol), quoteIdent(sessionTable), quoteIdent(idCol), escapeSQLLiteral(sessionID))
		cmd = exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			if !isRecentTimestamp(strings.TrimSpace(string(timeOutput))) {
				return nil, nil
			}
		}
	}

	messageTable, messageCols, err := findTable(dbPath, "message")
	if err != nil || messageTable == "" {
		// We at least know the session ID; let the caller fall back to the
		// standard on-disk message directory convention.
		msgDir, _ := GetMessageDir(sessionID)
		return &agent.SessionInfo{
			SessionID:      sessionID,
			TranscriptPath: msgDir,
			StartedAt:      time.Now().Format(time.RFC3339),
			ProjectPath:    projectPath,
		}, nil
	}

	msgIDCol := findColumn(messageCols, "id")
	sessionFKCol := findColumn(messageCols, "session")
	dataCol := findColumn(messageCols, "data")
	msgTimeCol := findColumn(messageCols, "created")

	if sessionFKCol == "" || dataCol == "" {
		msgDir, _ := GetMessageDir(sessionID)
		return &agent.SessionInfo{
			SessionID:      sessionID,
			TranscriptPath: msgDir,
			StartedAt:      time.Now().Format(time.RFC3339),
			ProjectPath:    projectPath,
		}, nil
	}

	idPatch := quoteIdent(dataCol)
	if msgIDCol != "" {
		idPatch = fmt.Sprintf("json_patch(%s, json_object('id', %s))", quoteIdent(dataCol), quoteIdent(msgIDCol))
	}

	msgOrder := ""
	if msgTimeCol != "" {
		msgOrder = fmt.Sprintf(" ORDER BY %s", quoteIdent(msgTimeCol))
	}

	msgQuery := fmt.Sprintf(
		"SELECT json_group_array(%s) FROM %s WHERE %s='%s'%s;",
		idPatch, quoteIdent(messageTable), quoteIdent(sessionFKCol), escapeSQLLiteral(sessionID), msgOrder,
	)
	cmd = exec.Command("sqlite3", dbPath, msgQuery)
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

// findTable finds the shortest table name in the SQLite database containing
// the given substring (case-insensitive) — e.g. "session" over
// "session_history" — and returns its name and column names.
func findTable(dbPath, nameHint string) (string, []string, error) {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	output, err := cmd.Output()
	if err != nil {
		return "", nil, err
	}

	var tableName string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(strings.ToLower(line), nameHint) {
			if tableName == "" || len(line) < len(tableName) {
				tableName = line
			}
		}
	}
	if tableName == "" {
		return "", nil, nil
	}

	cmd = exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(tableName)))
	output, err = cmd.Output()
	if err != nil {
		return "", nil, err
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}

	return tableName, cols, nil
}

// findColumn finds the first column whose name contains the given substring
// (case-insensitive).
func findColumn(cols []string, hint string) string {
	for _, c := range cols {
		if strings.Contains(strings.ToLower(c), hint) {
			return c
		}
	}
	return ""
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func escapeSQLLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// isRecentTimestamp reports whether a timestamp string is within the recent
// session window. It tolerates several known text formats as well as Unix
// epoch seconds/milliseconds/microseconds/nanoseconds. Unparseable
// timestamps are treated as recent so discovery doesn't fail closed just
// because a format wasn't recognized.
func isRecentTimestamp(s string) bool {
	if s == "" {
		return true
	}

	formats := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		var t time.Time
		switch {
		case n > 1e18: // nanoseconds
			t = time.Unix(0, n)
		case n > 1e15: // microseconds
			t = time.Unix(0, n*1000)
		case n > 1e12: // milliseconds
			t = time.Unix(0, n*int64(time.Millisecond))
		default: // seconds
			t = time.Unix(n, 0)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	return true
}

// sessionCandidate represents a plausible session file found during a
// resilient scan of OpenCode's data directory.
type sessionCandidate struct {
	sessionID    string
	path         string
	modTime      time.Time
	projectMatch bool
}

// betterCandidate reports whether a is preferable to b: a candidate whose
// embedded metadata (or path) matches the current project always wins,
// otherwise the more recently modified candidate wins.
func betterCandidate(a, b sessionCandidate) bool {
	if a.projectMatch != b.projectMatch {
		return a.projectMatch
	}
	return a.modTime.After(b.modTime)
}

// extractSessionID pulls a session identifier out of a parsed JSON session
// file, trying field names used by known OpenCode versions before falling
// back to the filename.
func extractSessionID(raw map[string]json.RawMessage, filename string) string {
	for _, key := range []string{"id", "sessionID", "sessionId", "session_id"} {
		if v, ok := raw[key]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err == nil && s != "" {
				return s
			}
		}
	}
	name := strings.TrimSuffix(filename, filepath.Ext(filename))
	if name != "" && !strings.EqualFold(name, "info") {
		return name
	}
	return ""
}

// candidateMatchesProject checks whether a session file's embedded metadata
// or on-disk path corresponds to the project we're looking for.
func candidateMatchesProject(raw map[string]json.RawMessage, path, projectID, projectPath string) bool {
	for _, key := range []string{"projectID", "project_id", "projectId"} {
		if v, ok := raw[key]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err == nil && s == projectID {
				return true
			}
		}
	}
	for _, key := range []string{"directory", "cwd", "worktree"} {
		if v, ok := raw[key]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err == nil && s == projectPath {
				return true
			}
		}
	}
	return projectID != "" && projectID != "global" && strings.Contains(path, projectID)
}

// discoverFromDataDirScan recursively scans dataDir for session-like JSON
// files when neither the known flat-file layout nor the SQLite layout
// produced a result. This tolerates OpenCode versions that nest session
// storage differently than either of those hard-coded conventions expect
// (e.g. an extra "project/<id>/" segment, or per-session subdirectories
// instead of flat "<id>.json" files).
func discoverFromDataDirScan(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	info, err := os.Stat(dataDir)
	if err != nil || !info.IsDir() {
		return nil, nil
	}

	now := time.Now()
	var best *sessionCandidate

	const maxDepth = 6
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > maxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			full := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				walk(full, depth+1)
				continue
			}
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			fi, err := entry.Info()
			if err != nil || now.Sub(fi.ModTime()) > agent.RecentSessionTimeout {
				continue
			}
			data, err := os.ReadFile(full)
			if err != nil {
				continue
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil {
				continue
			}

			sessionID := extractSessionID(raw, entry.Name())
			if sessionID == "" {
				continue
			}

			candidate := sessionCandidate{
				sessionID:    sessionID,
				path:         full,
				modTime:      fi.ModTime(),
				projectMatch: candidateMatchesProject(raw, full, projectID, projectPath),
			}

			if best == nil || betterCandidate(candidate, *best) {
				c := candidate
				best = &c
			}
		}
	}
	walk(dataDir, 0)

	if best == nil {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      best.sessionID,
		TranscriptPath: filepath.Dir(best.path),
		StartedAt:      best.modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
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
