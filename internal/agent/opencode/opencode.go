```go
package opencode

import (
	"encoding/json"
	"errors"
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
// OpenCode's on-disk layout has changed across releases: pre-v1.2 used flat
// per-project JSON files, v1.2+ added a SQLite index (opencode.db), and the
// table/column names within that database have shifted between releases
// (e.g. 1.14.x -> 1.17.x). We try each known layout in turn and fall back to
// a generic recursive scan of the data directory so future layout tweaks
// don't silently break discovery.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}
	projectID := GetProjectID(projectPath)

	// Try flat file storage first (pre-v1.2 OpenCode)
	session, err := a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}

	// Fall back to SQLite (OpenCode v1.2+)
	if session == nil {
		session, _ = discoverFromSQLite(dataDir, projectID, projectPath)
	}

	// Last resort: scan the whole data directory for anything that looks
	// like a session/message belonging to this project, regardless of the
	// directory layout the installed OpenCode version currently uses.
	if session == nil {
		session = discoverByScanning(dataDir, projectID, projectPath)
	}

	if session != nil && len(session.TranscriptData) == 0 && session.TranscriptPath == "" {
		// We found a session but couldn't locate its messages anywhere;
		// still record the commit against an empty transcript rather than
		// silently dropping it.
		session.TranscriptData = []byte("[]")
	}

	return session, nil
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

	// The transcript path for OpenCode is the message directory. If it isn't
	// where we expect, search the data directory for a directory named after
	// the session ID before giving up on it.
	dataDir, _ := GetDataDir()
	msgDir, _ := GetMessageDir(bestSessionID)
	if info, err := os.Stat(msgDir); err != nil || !info.IsDir() {
		msgDir = locateMessageDir(dataDir, bestSessionID)
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: msgDir,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// errFoundDir is a sentinel returned from filepath.WalkDir callbacks to stop
// walking as soon as a match is found.
var errFoundDir = errors.New("found")

// locateMessageDir searches the data directory for a directory named after
// sessionID, used when the conventional storage/message/<sessionID> path
// doesn't exist (e.g. the message tree gained or lost a level of nesting).
func locateMessageDir(dataDir, sessionID string) string {
	if dataDir == "" || sessionID == "" {
		return ""
	}

	storageDir := filepath.Join(dataDir, "storage")
	if _, err := os.Stat(storageDir); os.IsNotExist(err) {
		storageDir = dataDir
	}

	var found string
	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			found = path
			return errFoundDir
		}
		return nil
	})
	return found
}

// discoverByScanning walks the entire OpenCode data directory looking for a
// session record (a JSON file with an "id" plus a "projectID" or "directory"
// field) matching this project, then locates a message directory for that
// session's ID. It's a layout-agnostic fallback for when neither the
// flat-file nor SQLite storage conventions match the installed version.
func discoverByScanning(dataDir, projectID, projectPath string) *agent.SessionInfo {
	if dataDir == "" {
		return nil
	}

	storageDir := filepath.Join(dataDir, "storage")
	if _, err := os.Stat(storageDir); os.IsNotExist(err) {
		storageDir = dataDir
		if _, err := os.Stat(storageDir); err != nil {
			return nil
		}
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout
	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		info, err := d.Info()
		if err != nil || now.Sub(info.ModTime()) > recentTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var rec struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectID"`
			Directory string `json:"directory"`
		}
		if json.Unmarshal(data, &rec) != nil || rec.ID == "" {
			return nil
		}

		matches := (rec.ProjectID != "" && rec.ProjectID == projectID) ||
			(rec.Directory != "" && rec.Directory == projectPath)
		if !matches {
			return nil
		}

		if bestSessionID == "" || info.ModTime().After(bestModTime) {
			bestSessionID = rec.ID
			bestModTime = info.ModTime()
		}
		return nil
	})

	if bestSessionID == "" {
		return nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: locateMessageDir(dataDir, bestSessionID),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session. Table and column names are introspected at runtime (via
// sqlite_master and PRAGMA table_info) rather than hard-coded, since they
// have changed between OpenCode releases.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, sessionCols, err := findTable(dbPath, "session")
	if err != nil || sessionTable == "" {
		return nil, nil
	}

	idCol := findColumn(sessionCols, "id")
	projectCol := findColumn(sessionCols, "project_id", "projectid", "project")
	dirCol := findColumn(sessionCols, "directory", "cwd", "workdir", "path")
	updatedCol := findColumn(sessionCols, "time_updated", "updated_at", "updatedat", "mtime", "updated")

	if idCol == "" || (projectCol == "" && dirCol == "") {
		return nil, nil
	}

	var whereClause string
	if projectCol != "" {
		whereClause = fmt.Sprintf("%s='%s'", projectCol, projectID)
	} else {
		whereClause = fmt.Sprintf("%s='%s'", dirCol, projectPath)
	}

	orderCol := idCol
	if updatedCol != "" {
		orderCol = updatedCol
	}

	// Find most recent session for this project
	sessionQuery := fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s ORDER BY %s DESC LIMIT 1;`,
		idCol, sessionTable, whereClause, orderCol,
	)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Check if this session was recent (within timeout)
	if updatedCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, updatedCol, sessionTable, idCol, sessionID)
		cmd = exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			if !isRecentTimeString(strings.TrimSpace(string(timeOutput))) {
				return nil, nil
			}
		}
		// If we can't parse or read the time, proceed anyway — better to try than skip
	}

	info := &agent.SessionInfo{
		SessionID:   sessionID,
		StartedAt:   time.Now().Format(time.RFC3339),
		ProjectPath: projectPath,
	}

	messageTable, messageCols, err := findTable(dbPath, "message")
	if err != nil || messageTable == "" {
		return info, nil
	}

	msgIDCol := findColumn(messageCols, "id")
	msgSessionCol := findColumn(messageCols, "session_id", "sessionid")
	msgDataCol := findColumn(messageCols, "data", "content", "body", "payload")
	msgTimeCol := findColumn(messageCols, "time_created", "created_at", "createdat", "ctime", "created")

	if msgSessionCol == "" || msgDataCol == "" {
		return info, nil
	}

	patchExpr := msgDataCol
	if msgIDCol != "" {
		patchExpr = fmt.Sprintf("json_patch(%s, json_object('id', %s))", msgDataCol, msgIDCol)
	}
	msgOrderCol := msgIDCol
	if msgTimeCol != "" {
		msgOrderCol = msgTimeCol
	}

	// Get messages for this session as a JSON array
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM %s WHERE %s='%s' ORDER BY %s;`,
		patchExpr, messageTable, msgSessionCol, sessionID, msgOrderCol,
	)
	cmd = exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return info, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if string(transcriptData) != "[null]" && string(transcriptData) != "[]" && string(transcriptData) != "" {
		info.TranscriptData = transcriptData
	}

	return info, nil
}

// findTable finds a table whose name matches keyword (exact match preferred,
// substring match otherwise) and returns its name along with its column
// names in declaration order.
func findTable(dbPath, keyword string) (string, []string, error) {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	output, err := cmd.Output()
	if err != nil {
		return "", nil, err
	}

	var best string
	for _, name := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if lower == keyword {
			best = name
			break
		}
		if best == "" && strings.Contains(lower, keyword) {
			best = name
		}
	}
	if best == "" {
		return "", nil, nil
	}

	cmd = exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", best))
	infoOutput, err := cmd.Output()
	if err != nil {
		return best, nil, err
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(infoOutput)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return best, cols, nil
}

// findColumn returns the first column in cols matching one of candidates,
// trying exact (case-insensitive) matches before substring matches.
func findColumn(cols []string, candidates ...string) string {
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.EqualFold(c, cand) {
				return c
			}
		}
	}
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.Contains(strings.ToLower(c), cand) {
				return c
			}
		}
	}
	return ""
}

// isRecentTimeString reports whether s (an RFC3339-ish timestamp or a Unix
// epoch in seconds/milliseconds) falls within agent.RecentSessionTimeout of
// now. Unparseable input is treated as recent — better to try than skip.
func isRecentTimeString(s string) bool {
	if s == "" {
		return true
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		t := time.Unix(n, 0)
		if n > 1e12 {
			t = time.UnixMilli(n)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	formats := []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
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
