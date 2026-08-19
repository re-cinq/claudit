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
// It tries, in order: the legacy per-project flat file layout (pre-v1.2),
// the flat session-info layout used by more recent 1.x releases (where all
// sessions live in one directory and carry their own project/directory
// reference), and finally a SQLite-backed store if present. OpenCode's
// on-disk format has changed across releases, so each strategy is tried
// independently and failures fall through rather than aborting discovery.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try legacy flat file storage first (pre-v1.2 OpenCode)
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Try the flat session-info layout used by newer OpenCode releases
	session, err = a.discoverFromSessionInfoDir(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite, if present
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// discoverFromFlatFiles tries the legacy flat file session discovery, where
// sessions live under a per-project directory named after the project ID.
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

// discoverFromSessionInfoDir tries OpenCode's flat session-info layout
// (storage/session/info/<sessionID>.json), used by newer 1.x releases that
// no longer nest sessions under a per-project directory. All sessions across
// all projects live in this one directory, so each file is inspected for a
// projectID or directory field matching the current project.
func (a *Agent) discoverFromSessionInfoDir(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	infoDir := filepath.Join(dataDir, "storage", "session", "info")
	dirEntries, err := os.ReadDir(infoDir)
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	now := time.Now()
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
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			continue
		}

		data, err := os.ReadFile(filepath.Join(infoDir, entry.Name()))
		if err != nil {
			continue
		}

		var meta struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectID"`
			Directory string `json:"directory"`
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}

		matches := meta.ProjectID != "" && meta.ProjectID == projectID
		if !matches && meta.Directory != "" {
			matches = agent.PathsEqual(meta.Directory, projectPath)
		}
		if !matches {
			continue
		}

		sessionID := meta.ID
		if sessionID == "" {
			sessionID = strings.TrimSuffix(entry.Name(), ".json")
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = sessionID
			bestModTime = modTime
		}
	}

	if bestSessionID == "" {
		return nil, nil
	}

	sessionInfo := &agent.SessionInfo{
		SessionID:   bestSessionID,
		StartedAt:   bestModTime.Format(time.RFC3339),
		ProjectPath: projectPath,
	}

	if msgDir := resolveMessageDir(dataDir, bestSessionID); msgDir != "" {
		sessionInfo.TranscriptPath = msgDir
	} else {
		// No message directory found under any known layout; still record
		// the session so a note gets created rather than dropping it.
		sessionInfo.TranscriptData = []byte("[]")
	}

	return sessionInfo, nil
}

// resolveMessageDir finds the OpenCode message directory for a session,
// trying both the legacy flat layout (storage/message/<id>) and the nested
// layout used by newer releases (storage/session/message/<id>). Returns ""
// if neither exists.
func resolveMessageDir(dataDir, sessionID string) string {
	candidates := []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return ""
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session. Column names are discovered dynamically via PRAGMA
// table_info rather than assumed, since OpenCode's schema has changed
// across releases.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionCols := tableColumns(dbPath, "session")
	if len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, []string{"id"})
	projectCol := pickColumn(sessionCols, []string{"project_id", "projectid", "project"})
	dirCol := pickColumn(sessionCols, []string{"directory", "cwd", "path"})
	timeCol := pickColumn(sessionCols, []string{"time_updated", "updated_at", "updatedat", "time_created", "created_at"})

	if idCol == "" {
		return nil, nil
	}

	var whereClause string
	switch {
	case projectCol != "":
		whereClause = fmt.Sprintf("WHERE %s='%s'", projectCol, projectID)
	case dirCol != "":
		whereClause = fmt.Sprintf("WHERE %s='%s'", dirCol, projectPath)
	}

	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s DESC", timeCol)
	}

	// Find most recent session for this project
	sessionQuery := fmt.Sprintf(`SELECT %s FROM session %s%s LIMIT 1;`, idCol, whereClause, orderClause)
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
			if recent, ok := isRecentTimestamp(timeStr); ok && !recent {
				return nil, nil
			}
			// If we can't parse the time, proceed anyway — better to try than skip
		}
	}

	messageCols := tableColumns(dbPath, "message")
	dataCol := pickColumn(messageCols, []string{"data", "content", "body"})
	msgIDCol := pickColumn(messageCols, []string{"id"})
	msgSessionCol := pickColumn(messageCols, []string{"session_id", "sessionid"})
	msgTimeCol := pickColumn(messageCols, []string{"time_created", "created_at", "createdat", "time_updated"})

	if dataCol == "" || msgSessionCol == "" {
		// No usable message table; still report the session so a note gets
		// created rather than dropping it entirely.
		return &agent.SessionInfo{
			SessionID:      sessionID,
			StartedAt:      time.Now().Format(time.RFC3339),
			ProjectPath:    projectPath,
			TranscriptData: []byte("[]"),
		}, nil
	}

	// Get messages for this session as a JSON array
	patchExpr := dataCol
	if msgIDCol != "" {
		patchExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", dataCol, msgIDCol)
	}
	msgOrder := ""
	if msgTimeCol != "" {
		msgOrder = fmt.Sprintf(" ORDER BY %s", msgTimeCol)
	}
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM message WHERE %s='%s'%s;`,
		patchExpr, msgSessionCol, sessionID, msgOrder,
	)
	cmd = exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
		transcriptData = []byte("[]")
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// tableColumns returns the column names of a SQLite table via PRAGMA
// table_info, or nil if the table does not exist or sqlite3 cannot be
// queried.
func tableColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) >= 2 && fields[1] != "" {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// pickColumn returns the first column in cols matching one of the candidate
// names (case-insensitive exact match preferred, substring match as
// fallback). Returns "" if nothing matches.
func pickColumn(cols []string, candidates []string) string {
	lowerCols := make(map[string]string, len(cols))
	for _, c := range cols {
		lowerCols[strings.ToLower(c)] = c
	}

	for _, cand := range candidates {
		if col, ok := lowerCols[strings.ToLower(cand)]; ok {
			return col
		}
	}

	for _, cand := range candidates {
		candLower := strings.ToLower(cand)
		for lower, orig := range lowerCols {
			if strings.Contains(lower, candLower) {
				return orig
			}
		}
	}

	return ""
}

// isRecentTimestamp parses a timestamp string in any of several formats
// OpenCode has used (RFC3339, space-separated SQL datetime, or unix epoch
// seconds/milliseconds) and reports whether it falls within
// agent.RecentSessionTimeout. ok is false if the value could not be parsed.
func isRecentTimestamp(timeStr string) (recent bool, ok bool) {
	layouts := []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout, true
		}
	}

	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		var t time.Time
		if n > 1_000_000_000_000 { // milliseconds since epoch
			t = time.UnixMilli(n)
		} else {
			t = time.Unix(n, 0)
		}
		return time.Since(t) <= agent.RecentSessionTimeout, true
	}

	return false, false
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
