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
// OpenCode's on-disk storage layout has changed across versions (nesting,
// column names, even flat-files vs SQLite), so rather than assuming one
// fixed layout, this walks the data directory and matches session records
// by their recorded project directory. That field is far less likely to
// change shape than the storage layout itself, since OpenCode needs it
// for its own project-scoped session listing.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
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
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// sessionCandidate captures the fields OpenCode has used, across versions,
// to record which project directory a session belongs to.
type sessionCandidate struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID"`
	Directory string `json:"directory"`
	Cwd       string `json:"cwd"`
	Worktree  string `json:"worktree"`
	Path      struct {
		Cwd      string `json:"cwd"`
		Root     string `json:"root"`
		Worktree string `json:"worktree"`
	} `json:"path"`
}

// matchesProject reports whether a session candidate belongs to projectPath,
// matching on the literal directory the session recorded (robust to storage
// layout changes) or, failing that, on the legacy root-commit-hash project ID.
func (c *sessionCandidate) matchesProject(projectPath, projectID string) bool {
	if c.ID == "" {
		return false
	}
	clean := filepath.Clean(projectPath)
	for _, p := range []string{c.Directory, c.Cwd, c.Worktree, c.Path.Cwd, c.Path.Root, c.Path.Worktree} {
		if p != "" && filepath.Clean(p) == clean {
			return true
		}
	}
	return c.ProjectID != "" && c.ProjectID == projectID
}

// discoverFromFlatFiles walks the OpenCode data directory for a session
// belonging to projectPath. It does not assume a fixed nesting scheme,
// since that has changed across OpenCode releases.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	storageDir := filepath.Join(dataDir, "storage")
	if _, err := os.Stat(storageDir); err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout

	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(storageDir, func(path string, d os.DirEntry, walkErr error) error {
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

		var candidate sessionCandidate
		if err := json.Unmarshal(data, &candidate); err != nil {
			return nil
		}
		if !candidate.matchesProject(projectPath, projectID) {
			return nil
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = candidate.ID
			bestModTime = modTime
		}
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: collectMessagesForSession(storageDir, bestSessionID),
	}, nil
}

// collectMessagesForSession gathers message records for a session, wherever
// they live under storageDir (a subdirectory named after the session ID, or
// files whose path merely contains it), and returns them as a JSON array.
func collectMessagesForSession(storageDir, sessionID string) []byte {
	var messages []json.RawMessage

	_ = filepath.WalkDir(storageDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		if !strings.Contains(path, sessionID) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var probe struct {
			Role string `json:"role"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &probe); err != nil {
			return nil
		}
		// Session-info records (matched separately) don't carry role/type;
		// this distinguishes message records from the session record itself.
		if probe.Role == "" && probe.Type == "" {
			return nil
		}

		messages = append(messages, json.RawMessage(data))
		return nil
	})

	if len(messages) == 0 {
		return nil
	}
	out, err := json.Marshal(messages)
	if err != nil {
		return nil
	}
	return out
}

// discoverFromSQLite queries an OpenCode SQLite database for the most recent
// session, introspecting the actual table/column names present rather than
// assuming a fixed schema (which has changed across OpenCode releases).
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	tables, err := sqliteTables(dbPath)
	if err != nil || len(tables) == 0 {
		return nil, nil
	}

	sessionTable := findTable(tables, "session")
	if sessionTable == "" {
		return nil, nil
	}
	messageTable := findTable(tables, "message")

	sessionCols, err := sqliteColumns(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	timeCol := pickColumn(sessionCols, "timeupdated", "updatedat", "updated", "timecreated", "createdat", "created")
	pathCol := pickColumn(sessionCols, "directory", "cwd", "worktree", "path")
	projectCol := pickColumn(sessionCols, "projectid", "project")

	var whereClause string
	switch {
	case pathCol != "":
		whereClause = fmt.Sprintf("%s='%s'", pathCol, escapeSQLite(projectPath))
	case projectCol != "":
		whereClause = fmt.Sprintf("%s='%s'", projectCol, escapeSQLite(projectID))
	default:
		whereClause = "1=1"
	}
	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}

	sessionQuery := fmt.Sprintf("SELECT %s FROM %s WHERE %s%s LIMIT 1;", idCol, sessionTable, whereClause, orderClause)
	cmd := exec.Command("sqlite3", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	if timeCol != "" {
		cmd = exec.Command("sqlite3", dbPath,
			fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s';", timeCol, sessionTable, idCol, escapeSQLite(sessionID)))
		if timeOutput, err := cmd.Output(); err == nil {
			if t, ok := parseFlexibleTime(strings.TrimSpace(string(timeOutput))); ok {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
			// If we can't parse the time, proceed anyway — better to try than skip.
		}
	}

	var transcriptData []byte
	if messageTable != "" {
		if msgCols, err := sqliteColumns(dbPath, messageTable); err == nil && len(msgCols) > 0 {
			msgIDCol := pickColumn(msgCols, "id")
			sessionRefCol := pickColumn(msgCols, "sessionid", "session")
			dataCol := pickColumn(msgCols, "data", "content", "body")
			msgTimeCol := pickColumn(msgCols, "timecreated", "createdat", "created", "timeupdated", "updated")

			if sessionRefCol != "" && dataCol != "" {
				selectExpr := dataCol
				if msgIDCol != "" {
					selectExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, msgIDCol)
				}
				order := ""
				if msgTimeCol != "" {
					order = fmt.Sprintf(" ORDER BY %s", msgTimeCol)
				}
				msgQuery := fmt.Sprintf(
					"SELECT json_group_array(%s) FROM %s WHERE %s='%s'%s;",
					selectExpr, messageTable, sessionRefCol, escapeSQLite(sessionID), order,
				)
				cmd = exec.Command("sqlite3", dbPath, msgQuery)
				if msgOutput, err := cmd.Output(); err == nil {
					out := strings.TrimSpace(string(msgOutput))
					if out != "" && out != "[null]" && out != "[]" {
						transcriptData = []byte(out)
					}
				}
			}
		}
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "",
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// sqliteTables lists table names present in a SQLite database.
func sqliteTables(dbPath string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var tables []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			tables = append(tables, line)
		}
	}
	return tables, nil
}

// sqliteColumns lists column names for a table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// findTable returns the table whose name best matches want (exact, plural, then substring).
func findTable(tables []string, want string) string {
	for _, t := range tables {
		if strings.EqualFold(t, want) || strings.EqualFold(t, want+"s") {
			return t
		}
	}
	for _, t := range tables {
		if strings.Contains(strings.ToLower(t), want) {
			return t
		}
	}
	return ""
}

// pickColumn returns the first column matching one of the candidate names,
// ignoring case and underscores, trying exact matches before substring ones.
func pickColumn(cols []string, candidates ...string) string {
	norm := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(s, "_", ""))
	}
	for _, want := range candidates {
		for _, c := range cols {
			if norm(c) == norm(want) {
				return c
			}
		}
	}
	for _, want := range candidates {
		for _, c := range cols {
			if strings.Contains(norm(c), norm(want)) {
				return c
			}
		}
	}
	return ""
}

// escapeSQLite escapes single quotes for embedding a value in a SQL literal.
func escapeSQLite(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// parseFlexibleTime tries several timestamp formats OpenCode has used,
// including raw Unix seconds/milliseconds.
func parseFlexibleTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	formats := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
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
```
