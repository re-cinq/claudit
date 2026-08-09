package opencode

import (
	"encoding/json"
	"fmt"
	"io"
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
	// Try flat file storage first (pre-v1.2 OpenCode, and any v1.2+ layout
	// that still keeps a JSON mirror)
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

// openCodeSessionCandidate is a recently modified session found on disk
// during flat-file discovery, along with the directory it was found in
// (used to locate a co-located message directory) and its raw JSON content
// (used to verify project ownership when the directory itself doesn't
// already scope the session to a single project).
type openCodeSessionCandidate struct {
	sessionID string
	sourceDir string
	modTime   time.Time
	data      []byte
}

// readOpenCodeSessionCandidate reads a session file's stat info and,
// if requireData is true, its contents.
func readOpenCodeSessionCandidate(path, sessionID, sourceDir string, requireData bool) *openCodeSessionCandidate {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil
	}

	var data []byte
	if requireData {
		data, err = os.ReadFile(path)
		if err != nil {
			return nil
		}
	}

	return &openCodeSessionCandidate{
		sessionID: sessionID,
		sourceDir: sourceDir,
		modTime:   info.ModTime(),
		data:      data,
	}
}

// openCodeSessionMatchesProject reports whether a session file's embedded
// project fields match the given project. Several field name variants are
// tried since the exact schema has changed across OpenCode versions.
func openCodeSessionMatchesProject(data []byte, projectID, projectPath string) bool {
	if len(data) == 0 {
		return false
	}

	var fields struct {
		ProjectID   string `json:"projectID"`
		ProjectIDSC string `json:"project_id"`
		Directory   string `json:"directory"`
		Cwd         string `json:"cwd"`
		Path        string `json:"path"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return false
	}

	if projectID != "" && (fields.ProjectID == projectID || fields.ProjectIDSC == projectID) {
		return true
	}
	for _, dir := range []string{fields.Directory, fields.Cwd, fields.Path} {
		if dir != "" && agent.PathsEqual(dir, projectPath) {
			return true
		}
	}
	return false
}

// scanOpenCodeSessions scans dir for OpenCode session files, considering
// both the "<sessionID>.json" layout and the "<sessionID>/info.json"
// (per-session directory) layout. When requireMatch is true, a session is
// only a candidate if its content matches projectID/projectPath (used for
// directories that aren't already scoped to a single project); otherwise
// every recent session is a candidate (used when the directory nesting
// itself provides the project scoping).
func scanOpenCodeSessions(dir string, requireMatch bool, projectID, projectPath string) *openCodeSessionCandidate {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	now := time.Now()
	var best *openCodeSessionCandidate

	consider := func(c *openCodeSessionCandidate) {
		if c == nil || now.Sub(c.modTime) > agent.RecentSessionTimeout {
			return
		}
		if requireMatch && !openCodeSessionMatchesProject(c.data, projectID, projectPath) {
			return
		}
		if best == nil || c.modTime.After(best.modTime) {
			best = c
		}
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			consider(readOpenCodeSessionCandidate(filepath.Join(dir, name, "info.json"), name, dir, true))
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		sessionID := strings.TrimSuffix(name, ".json")
		consider(readOpenCodeSessionCandidate(filepath.Join(dir, name), sessionID, dir, requireMatch))
	}

	return best
}

// openCodeCandidateToSessionInfo resolves a session candidate's transcript
// path. It prefers the standard message directory
// (<dataDir>/storage/message/<sessionID>); if that doesn't exist, it falls
// back to a message directory co-located with the session file itself
// (<sourceDir>/<sessionID>/message), which some OpenCode layouts use.
func openCodeCandidateToSessionInfo(c *openCodeSessionCandidate, projectPath string) *agent.SessionInfo {
	transcriptPath, _ := GetMessageDir(c.sessionID)
	if _, err := os.Stat(transcriptPath); err != nil {
		if alt := filepath.Join(c.sourceDir, c.sessionID, "message"); alt != transcriptPath {
			if _, err := os.Stat(alt); err == nil {
				transcriptPath = alt
			}
		}
	}

	return &agent.SessionInfo{
		SessionID:      c.sessionID,
		TranscriptPath: transcriptPath,
		StartedAt:      c.modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// discoverFromFlatFiles tries the legacy flat file session discovery.
// OpenCode versions differ on whether sessions live in a directory keyed by
// project ID (storage/session/<projectID>/<sessionID>.json) or in a single
// directory scoped by an embedded projectID/directory field instead
// (storage/session/<sessionID>.json or storage/session/<sessionID>/info.json).
// Both shapes are tried, in that order.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	projectID := GetProjectID(projectPath)

	sessionDir, err := GetSessionDir(projectPath)
	if err == nil {
		if best := scanOpenCodeSessions(sessionDir, false, projectID, projectPath); best != nil {
			return openCodeCandidateToSessionInfo(best, projectPath), nil
		}
	}

	// Fallback: the directory nesting may not scope sessions to a single
	// project anymore. Scan the top-level session storage dir and match by
	// embedded project fields instead.
	dataDir, dirErr := GetDataDir()
	if dirErr != nil {
		return nil, nil
	}
	topDir := filepath.Join(dataDir, "storage", "session")
	if topDir == sessionDir {
		return nil, nil
	}
	if best := scanOpenCodeSessions(topDir, true, projectID, projectPath); best != nil {
		return openCodeCandidateToSessionInfo(best, projectPath), nil
	}

	return nil, nil
}

// openCodeSQLiteSchema describes one possible naming of the OpenCode
// session/message tables and columns. Table/column names have changed
// across OpenCode versions (e.g. singular "session"/"message" vs plural
// "sessions"/"messages"), so several known variants are tried in order.
type openCodeSQLiteSchema struct {
	sessionTable string
	sessionID    string
	projectCol   string
	updatedCol   string
	messageTable string
	sessionFKCol string
	createdCol   string
	dataCol      string
}

var openCodeSQLiteSchemas = []openCodeSQLiteSchema{
	{"session", "id", "project_id", "time_updated", "message", "session_id", "time_created", "data"},
	{"sessions", "id", "project_id", "updated_at", "messages", "session_id", "created_at", "data"},
	{"sessions", "id", "projectID", "timeUpdated", "messages", "sessionID", "timeCreated", "data"},
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	for _, schema := range openCodeSQLiteSchemas {
		info, err := discoverFromSQLiteSchema(dbPath, projectID, projectPath, schema)
		if err != nil {
			// Schema didn't match this database (e.g. wrong table/column
			// names) - try the next candidate schema.
			continue
		}
		if info != nil {
			return info, nil
		}
	}

	return nil, nil
}

// discoverFromSQLiteSchema attempts session discovery against one specific
// table/column naming. A non-nil error means the schema didn't match
// (caller should try the next candidate schema); a nil session with a nil
// error means the schema matched but no recent session was found.
func discoverFromSQLiteSchema(dbPath, projectID, projectPath string, s openCodeSQLiteSchema) (*agent.SessionInfo, error) {
	// Find most recent session for this project
	sessionQuery := fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s='%s' ORDER BY %s DESC LIMIT 1;`,
		s.sessionID, s.sessionTable, s.projectCol, projectID, s.updatedCol,
	)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Check if this session was recent (within timeout)
	timeQuery := fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s='%s';`,
		s.updatedCol, s.sessionTable, s.sessionID, sessionID,
	)
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
		} else if ms, err := parseEpochMillis(timeStr); err == nil {
			if time.Since(time.UnixMilli(ms)) > agent.RecentSessionTimeout {
				return nil, nil
			}
		}
		// If we can't parse the time, proceed anyway — better to try than skip
	}

	// Get messages for this session as a JSON array
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(%s, json_object('id', id))) FROM %s WHERE %s='%s' ORDER BY %s;`,
		s.dataCol, s.messageTable, s.sessionFKCol, sessionID, s.createdCol,
	)
	cmd = exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, err
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

// parseEpochMillis parses a decimal Unix-epoch-milliseconds timestamp.
func parseEpochMillis(s string) (int64, error) {
	var ms int64
	_, err := fmt.Sscanf(s, "%d", &ms)
	if err != nil {
		return 0, err
	}
	return ms, nil
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
