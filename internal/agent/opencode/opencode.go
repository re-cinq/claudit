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

// sessionCandidate is a discovered OpenCode session JSON file, gathered while
// walking the storage tree without assuming a fixed directory layout.
type sessionCandidate struct {
	path      string
	sessionID string
	directory string
	modTime   time.Time
}

// findSessionFiles walks the session storage tree (bounded by RecentSessionTimeout
// via mtime filtering) collecting candidate session files. OpenCode's on-disk
// layout has changed across versions (e.g. flat "<projectID>/<id>.json" vs a
// per-session directory with its own metadata file), so instead of assuming a
// fixed nesting we scan for any *.json file that parses with a session "id"
// field, recording whatever project/cwd-like field it has (if any) for matching.
func findSessionFiles(baseDir string) []sessionCandidate {
	var candidates []sessionCandidate
	now := time.Now()

	_ = filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}
		if now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var raw struct {
			ID        string `json:"id"`
			Directory string `json:"directory"`
			CWD       string `json:"cwd"`
			Path      struct {
				CWD  string `json:"cwd"`
				Root string `json:"root"`
			} `json:"path"`
		}
		if err := json.Unmarshal(data, &raw); err != nil || raw.ID == "" {
			return nil
		}

		dir := raw.Directory
		if dir == "" {
			dir = raw.CWD
		}
		if dir == "" {
			dir = raw.Path.CWD
		}
		if dir == "" {
			dir = raw.Path.Root
		}

		candidates = append(candidates, sessionCandidate{
			path:      path,
			sessionID: raw.ID,
			directory: dir,
			modTime:   info.ModTime(),
		})
		return nil
	})

	return candidates
}

// discoverFromFlatFiles scans the flat-file session storage for a session
// belonging to projectPath. It prefers a session whose stored directory/cwd
// field matches projectPath, and falls back to the single most recently
// modified session found anywhere in storage when no field match exists
// (e.g. the CLI's session metadata schema dropped that field).
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	candidates := findSessionFiles(sessionRoot)
	if len(candidates) == 0 {
		return nil, nil
	}

	var best, fallback *sessionCandidate
	for i := range candidates {
		c := &candidates[i]
		if fallback == nil || c.modTime.After(fallback.modTime) {
			fallback = c
		}
		if c.directory != "" && agent.PathsEqual(c.directory, projectPath) {
			if best == nil || c.modTime.After(best.modTime) {
				best = c
			}
		}
	}

	chosen := best
	if chosen == nil {
		chosen = fallback
	}
	if chosen == nil {
		return nil, nil
	}

	// The message directory is usually keyed by session ID under storage/message,
	// but newer layouts may colocate messages next to the session file itself.
	transcriptPath, err := GetMessageDir(chosen.sessionID)
	if err != nil {
		transcriptPath = ""
	}
	if _, statErr := os.Stat(transcriptPath); statErr != nil {
		if sibling := filepath.Join(filepath.Dir(chosen.path), "message"); dirExists(sibling) {
			transcriptPath = sibling
		}
	}

	return &agent.SessionInfo{
		SessionID:      chosen.sessionID,
		TranscriptPath: transcriptPath,
		StartedAt:      chosen.modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
// It first tries a project-scoped lookup, then falls back to the most recently
// updated session overall if the schema no longer has a matching project column
// (or uses a different name for it) — the manual/post-commit discovery flow only
// ever expects a single active session on the machine within the recent window.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionID, updatedAt := querySQLiteSession(dbPath, fmt.Sprintf("project_id='%s'", projectID))
	scoped := sessionID != ""
	if sessionID == "" {
		sessionID, updatedAt = querySQLiteSession(dbPath, "")
	}
	if sessionID == "" {
		return nil, nil
	}

	// Only enforce the recency window when we have a timestamp to check;
	// an unscoped fallback still benefits from filtering out stale sessions.
	if updatedAt != "" && !isRecentTimestamp(updatedAt) {
		if scoped {
			return nil, nil
		}
	}

	transcriptData := querySQLiteMessages(dbPath, sessionID)
	if len(transcriptData) == 0 {
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

// querySQLiteSession returns the id and updated-timestamp of the most recent
// session matching the optional SQL WHERE clause (pass "" for no filter).
// It tries several timestamp column names since OpenCode has used different
// ones across versions, returning as soon as one succeeds.
func querySQLiteSession(dbPath, whereClause string) (id, updatedAt string) {
	where := ""
	if whereClause != "" {
		where = "WHERE " + whereClause
	}

	for _, timeCol := range []string{"time_updated", "updated_at", "time_created", "created_at"} {
		query := fmt.Sprintf(`SELECT id, %s FROM session %s ORDER BY %s DESC LIMIT 1;`, timeCol, where, timeCol)
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		output, err := cmd.Output()
		if err != nil {
			continue
		}

		line := strings.TrimSpace(string(output))
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "\t", 2)
		if parts[0] == "" {
			continue
		}

		id = parts[0]
		if len(parts) > 1 {
			updatedAt = parts[1]
		}
		return id, updatedAt
	}

	return "", ""
}

// isRecentTimestamp reports whether a SQLite timestamp value (RFC3339,
// space-separated, or Unix epoch seconds/milliseconds) falls within
// RecentSessionTimeout. Unparseable values don't block discovery.
func isRecentTimestamp(timeStr string) bool {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		t := time.Unix(n, 0)
		if n > 1_000_000_000_000 { // milliseconds
			t = time.UnixMilli(n)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	return true
}

// querySQLiteMessages returns messages for a session as a JSON array, trying
// known orderings and payload column names since OpenCode's message schema
// has changed across versions.
func querySQLiteMessages(dbPath, sessionID string) []byte {
	queries := []string{
		fmt.Sprintf(`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE session_id='%s' ORDER BY time_created;`, sessionID),
		fmt.Sprintf(`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE session_id='%s' ORDER BY rowid;`, sessionID),
		fmt.Sprintf(`SELECT json_group_array(json(parts)) FROM message WHERE session_id='%s' ORDER BY rowid;`, sessionID),
		fmt.Sprintf(`SELECT json_group_array(json(content)) FROM message WHERE session_id='%s' ORDER BY rowid;`, sessionID),
	}

	for _, query := range queries {
		cmd := exec.Command("sqlite3", dbPath, query)
		output, err := cmd.Output()
		if err != nil {
			continue
		}

		data := strings.TrimSpace(string(output))
		if data == "" || data == "[null]" || data == "[]" {
			continue
		}
		return []byte(data)
	}

	return nil
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
