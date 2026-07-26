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
// OpenCode has nested message storage differently across versions (a flat
// directory of per-message files vs. deeper trees, e.g. for streamed message
// "parts"), so this walks the whole subtree rather than assuming a single
// flat level.
func (a *Agent) parseMessageDir(dir string) (*agent.Transcript, error) {
	var entries []agent.TranscriptEntry

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		name := d.Name()
		switch {
		case strings.HasSuffix(name, ".jsonl"):
			f, ferr := os.Open(path)
			if ferr != nil {
				return nil
			}
			transcript, perr := a.ParseTranscript(f)
			_ = f.Close()
			if perr == nil {
				entries = append(entries, transcript.Entries...)
			}
		case strings.HasSuffix(name, ".json"):
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}

			var raw map[string]json.RawMessage
			if uerr := json.Unmarshal(data, &raw); uerr != nil {
				return nil
			}

			entry := parseOpenCodeEntry(raw, data)
			if entry.Type != "" {
				entries = append(entries, entry)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &agent.Transcript{Entries: entries}, nil
}

// DiscoverSession finds an active or recent OpenCode session.
// It first tries flat file storage, then falls back to SQLite (some
// OpenCode versions use a local SQLite database instead of flat files).
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

// discoverFromFlatFiles tries flat-file session discovery.
//
// OpenCode's on-disk storage layout has changed across versions — sessions
// have been observed nested under a project-id-named directory
// (storage/session/<projectID>/<id>.json) as well as stored flatter with the
// project association recorded as a field inside the session JSON itself
// (e.g. "projectID" or "directory"). Rather than assuming one fixed shape,
// this walks the whole storage tree under the data dir and matches session
// files by content, falling back to directory-name matching for the legacy
// layout.
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

	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, ierr := d.Info()
		if ierr != nil || now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}

		var session map[string]interface{}
		if uerr := json.Unmarshal(data, &session); uerr != nil {
			return nil
		}

		if !sessionMatchesProject(session, path, projectID, projectPath) {
			return nil
		}

		if bestSessionID == "" || info.ModTime().After(bestModTime) {
			bestSessionID = sessionIDFromFile(session, d.Name())
			bestModTime = info.ModTime()
		}
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	// The transcript path for OpenCode is the message directory. Search for
	// a directory named after the session ID anywhere in storage, since the
	// exact nesting (e.g. storage/message/<id> vs storage/session/message/<id>)
	// has varied across versions; fall back to the legacy fixed path.
	msgDir := findMessageDir(storageDir, bestSessionID)
	if msgDir == "" {
		msgDir, _ = GetMessageDir(bestSessionID)
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// sessionMatchesProject reports whether a candidate JSON file found while
// walking storage represents a session belonging to this project.
func sessionMatchesProject(session map[string]interface{}, path, projectID, projectPath string) bool {
	// Message files typically carry a "role", session info files don't.
	if _, hasRole := session["role"]; hasRole {
		return false
	}

	if pid, ok := session["projectID"].(string); ok && pid != "" {
		return pid == projectID
	}
	if dir, ok := session["directory"].(string); ok && dir != "" {
		return agent.PathsEqual(dir, projectPath)
	}

	// Legacy layout: storage/session/<projectID>/<id>.json
	return strings.Contains(filepath.ToSlash(path), "/"+projectID+"/")
}

// sessionIDFromFile derives a session ID from a session JSON's own "id"
// field if present, falling back to the filename.
func sessionIDFromFile(session map[string]interface{}, filename string) string {
	if id, ok := session["id"].(string); ok && id != "" {
		return id
	}
	return strings.TrimSuffix(filename, ".json")
}

// findMessageDir searches the storage tree for a directory named after the
// session ID, which is where OpenCode keeps that session's messages.
// Returns "" if no such directory is found.
func findMessageDir(storageDir, sessionID string) string {
	var found string
	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, err error) error {
		if found != "" {
			return filepath.SkipDir
		}
		if err != nil {
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

// discoverFromSQLite queries an OpenCode SQLite database for the most recent
// session. Table/column names are discovered at runtime via PRAGMA
// table_info rather than hard-coded, since OpenCode's SQLite schema has
// changed names across versions (e.g. project association or timestamp
// columns being renamed).
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionCols, err := sqliteColumns(dbPath, "session")
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}

	timeCol := pickColumn(sessionCols, "time_updated", "updated_at", "updated", "time_created", "created_at", "created")

	var whereClause string
	if col := pickColumn(sessionCols, "project_id", "projectid", "project"); col != "" {
		whereClause = fmt.Sprintf("WHERE %s='%s'", col, projectID)
	} else if col := pickColumn(sessionCols, "directory", "cwd", "path"); col != "" {
		whereClause = fmt.Sprintf("WHERE %s='%s'", col, projectPath)
	}

	orderClause := "ORDER BY rowid DESC"
	if timeCol != "" {
		orderClause = fmt.Sprintf("ORDER BY %s DESC", timeCol)
	}

	// Find most recent session for this project
	sessionQuery := fmt.Sprintf(`SELECT %s FROM session %s %s LIMIT 1;`, idCol, whereClause, orderClause)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Check if this session was recent (within timeout)
	if timeCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM session WHERE %s='%s';`, timeCol, idCol, sessionID)
		cmd = exec.Command("sqlite3", dbPath, timeQuery)
		timeOutput, err := cmd.Output()
		if err == nil {
			timeStr := strings.TrimSpace(string(timeOutput))
			if t, err := time.Parse(time.RFC3339Nano, timeStr); err == nil {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			} else if t, err := time.Parse("2006-01-02T15:04:05.000Z", timeStr); err == nil {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			} else if t, err := time.Parse("2006-01-02 15:04:05", timeStr); err == nil {
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			} else if ms, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
				// Some versions store timestamps as epoch milliseconds.
				t := time.UnixMilli(ms)
				if time.Since(t) > agent.RecentSessionTimeout {
					return nil, nil
				}
			}
			// If we can't parse the time, proceed anyway — better to try than skip
		}
	}

	messageCols, err := sqliteColumns(dbPath, "message")
	if err != nil || len(messageCols) == 0 {
		return nil, nil
	}

	sessionRefCol := pickColumn(messageCols, "session_id", "sessionid", "session")
	dataCol := pickColumn(messageCols, "data", "content", "body")
	msgIDCol := pickColumn(messageCols, "id")
	timeCreatedCol := pickColumn(messageCols, "time_created", "created_at", "created")

	if sessionRefCol == "" || dataCol == "" {
		return nil, nil
	}

	patchExpr := dataCol
	if msgIDCol != "" {
		patchExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, msgIDCol)
	}

	orderMsg := ""
	if timeCreatedCol != "" {
		orderMsg = fmt.Sprintf("ORDER BY %s", timeCreatedCol)
	}

	// Get messages for this session as a JSON array
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM message WHERE %s='%s' %s;`,
		patchExpr, sessionRefCol, sessionID, orderMsg,
	)
	cmd = exec.Command("sqlite3", dbPath, msgQuery)
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

// sqliteColumns returns the column names for a table via PRAGMA table_info,
// so callers don't need to hard-code a schema that may drift across
// OpenCode versions.
func sqliteColumns(dbPath, table string) ([]string, error) {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// pickColumn returns the first candidate column name present in cols
// (case-insensitive), or "" if none match.
func pickColumn(cols []string, candidates ...string) string {
	for _, candidate := range candidates {
		for _, c := range cols {
			if strings.EqualFold(c, candidate) {
				return c
			}
		}
	}
	return ""
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
