package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
// OpenCode has changed its on-disk storage layout across releases (flat files
// nested by a git-derived project ID, a local SQLite database, and other
// layouts in between), so we try each known scheme in order of specificity
// and fall back to a generic, layout-agnostic scan of the data directory
// for anything that looks like a recent session before giving up.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try flat file storage first (pre-v1.2 OpenCode)
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

	// Fall back to SQLite (OpenCode v1.2+)
	projectID := GetProjectID(projectPath)
	session, err = discoverFromSQLite(dataDir, projectID, projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Last resort: scan the data directory for anything that looks like a
	// recent session, without assuming a specific directory layout or
	// database schema. This keeps discovery working even when a newer
	// OpenCode release changes its storage format in ways the schemes above
	// don't anticipate.
	return discoverByScanning(dataDir, projectPath)
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

	// Find most recent session for this project
	sessionQuery := fmt.Sprintf(
		`SELECT id FROM session WHERE project_id='%s' ORDER BY time_updated DESC LIMIT 1;`,
		projectID,
	)
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, sessionQuery)
	sessionOutput, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(sessionOutput)) == "" {
		return nil, nil
	}
	sessionID := strings.TrimSpace(string(sessionOutput))

	// Check if this session was recent (within timeout)
	timeQuery := fmt.Sprintf(
		`SELECT time_updated FROM session WHERE id='%s';`,
		sessionID,
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
		}
		// If we can't parse the time, proceed anyway — better to try than skip
	}

	// Get messages for this session as a JSON array
	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE session_id='%s' ORDER BY time_created;`,
		sessionID,
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

// maxScannedFiles bounds how many files discoverByScanning and
// gatherSessionFiles will inspect, so a data directory accumulated over many
// projects/sessions can't turn a manual commit into an unbounded filesystem walk.
const maxScannedFiles = 20000

// sessionIDFields lists the JSON keys OpenCode has used across releases to
// identify a session.
var sessionIDFields = []string{"id", "sessionID", "session_id", "sessionId"}

// sessionDirFields lists the JSON keys OpenCode has used across releases to
// record which project directory a session belongs to.
var sessionDirFields = []string{"directory", "cwd", "path", "worktree", "projectPath", "root", "dir"}

// discoverByScanning performs a layout-agnostic scan of the OpenCode data
// directory for the most recent session belonging to projectPath. Unlike
// discoverFromFlatFiles and discoverFromSQLite, it does not assume a specific
// on-disk layout or database schema: it looks for any recently modified JSON
// file under a "session"-named path that has an id-like field, preferring
// ones whose recorded project directory matches projectPath, and then gathers
// any file that appears to belong to the chosen session id.
func discoverByScanning(dataDir, projectPath string) (*agent.SessionInfo, error) {
	if _, err := os.Stat(dataDir); err != nil {
		return nil, nil
	}

	now := time.Now()
	visited := 0

	var bestID string
	var bestModTime time.Time
	var bestMatched bool
	haveBest := false

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		visited++
		if visited > maxScannedFiles {
			return filepath.SkipAll
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		if !strings.Contains(strings.ToLower(path), "session") {
			return nil
		}

		info, err := d.Info()
		if err != nil || now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil
		}

		id := firstStringField(raw, sessionIDFields...)
		if id == "" {
			return nil
		}
		matched := dirFieldMatches(raw, projectPath)

		if !haveBest ||
			(matched && !bestMatched) ||
			(matched == bestMatched && info.ModTime().After(bestModTime)) {
			bestID = id
			bestModTime = info.ModTime()
			bestMatched = matched
			haveBest = true
		}
		return nil
	})

	if !haveBest {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestID,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: gatherSessionFiles(dataDir, bestID),
	}, nil
}

// firstStringField returns the first non-empty string value found in raw
// under any of the given keys.
func firstStringField(raw map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil && s != "" {
			return s
		}
	}
	return ""
}

// dirFieldMatches reports whether raw contains a project-directory-like field
// whose value matches projectPath.
func dirFieldMatches(raw map[string]json.RawMessage, projectPath string) bool {
	for _, k := range sessionDirFields {
		v, ok := raw[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil || s == "" {
			continue
		}
		if agent.PathsEqual(s, projectPath) {
			return true
		}
	}
	return false
}

// gatherSessionFiles walks dataDir collecting every JSON/JSONL file that
// appears to belong to sessionID — by living in a directory named after the
// session id, by having a file name that starts with the session id, or by
// containing a session id field that matches — and combines them into a
// single JSON array ordered by modification time.
func gatherSessionFiles(dataDir, sessionID string) []byte {
	type found struct {
		modTime time.Time
		data    json.RawMessage
	}
	var entries []found
	visited := 0

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		visited++
		if visited > maxScannedFiles {
			return filepath.SkipAll
		}

		name := d.Name()
		isJSON := strings.HasSuffix(name, ".json")
		isJSONL := strings.HasSuffix(name, ".jsonl")
		if !isJSON && !isJSONL {
			return nil
		}

		belongs := filepath.Base(filepath.Dir(path)) == sessionID ||
			strings.HasPrefix(name, sessionID)

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		if !belongs && isJSON {
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err == nil {
				if firstStringField(raw, "sessionID", "session_id", "sessionId") == sessionID {
					belongs = true
				}
			}
		}
		if !belongs {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		if isJSONL {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				entries = append(entries, found{modTime: info.ModTime(), data: json.RawMessage(line)})
			}
			return nil
		}

		entries = append(entries, found{modTime: info.ModTime(), data: json.RawMessage(data)})
		return nil
	})

	sort.Slice(entries, func(i, j int) bool { return entries[i].modTime.Before(entries[j].modTime) })

	messages := make([]json.RawMessage, len(entries))
	for i, e := range entries {
		messages[i] = e.data
	}
	out, err := json.Marshal(messages)
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
