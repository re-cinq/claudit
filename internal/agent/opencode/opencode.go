package opencode

import (
	"bytes"
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

// mostRecentSessionFile returns the session ID (filename without ".json") of
// the most recently modified session file in dir, considering only files
// modified within agent.RecentSessionTimeout. ok is false if dir doesn't
// exist or has no recent session files.
func mostRecentSessionFile(dir string) (id string, modTime time.Time, ok bool) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}, false
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout

	for _, entry := range dirEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		entryModTime := info.ModTime()
		if now.Sub(entryModTime) > recentTimeout {
			continue
		}

		if !ok || entryModTime.After(modTime) {
			id = strings.TrimSuffix(entry.Name(), ".json")
			modTime = entryModTime
			ok = true
		}
	}

	return id, modTime, ok
}

// discoverFromFlatFiles tries the legacy flat file session discovery.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return nil, nil
	}

	bestSessionID, bestModTime, ok := mostRecentSessionFile(sessionDir)

	if !ok {
		// The directory keyed by our own computed project ID had no recent
		// session. OpenCode's project-ID scheme has drifted across releases
		// (it doesn't always match our git-root-commit hash), so fall back
		// to scanning every project directory for the most recent session,
		// preferring one whose recorded "directory" field matches this repo.
		dataDir, dataDirErr := GetDataDir()
		if dataDirErr != nil {
			return nil, nil
		}
		bestSessionID, bestModTime, ok = scanForRecentSession(dataDir, projectPath)
	}

	if !ok {
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

// scanForRecentSession searches every project directory under
// dataDir/storage/session for the most recently modified session file,
// preferring one whose recorded "directory" field matches projectPath. This
// is used when the exact directory keyed by our own computed project ID
// doesn't contain a recent session, which happens when OpenCode's internal
// project-ID scheme no longer matches ours.
func scanForRecentSession(dataDir, projectPath string) (id string, modTime time.Time, ok bool) {
	sessionsRoot := filepath.Join(dataDir, "storage", "session")
	projectDirs, err := os.ReadDir(sessionsRoot)
	if err != nil {
		return "", time.Time{}, false
	}

	var bestMatchesPath bool

	for _, pd := range projectDirs {
		if !pd.IsDir() {
			continue
		}
		dir := filepath.Join(sessionsRoot, pd.Name())
		candidateID, candidateModTime, candidateOK := mostRecentSessionFile(dir)
		if !candidateOK {
			continue
		}

		matchesPath := sessionMatchesDirectory(dir, candidateID, projectPath)

		better := !ok ||
			(matchesPath && !bestMatchesPath) ||
			(matchesPath == bestMatchesPath && candidateModTime.After(modTime))

		if better {
			id, modTime, ok = candidateID, candidateModTime, true
			bestMatchesPath = matchesPath
		}
	}

	return id, modTime, ok
}

// sessionMatchesDirectory reports whether the session file <dir>/<id>.json
// records a "directory" field equal to projectPath.
func sessionMatchesDirectory(dir, id, projectPath string) bool {
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return false
	}
	var s sessionInfo
	if err := json.Unmarshal(data, &s); err != nil || s.Directory == "" {
		return false
	}
	return s.Directory == projectPath
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session. OpenCode's session/message table columns have drifted across
// releases (e.g. a single JSON "data" blob column vs. flat columns per
// field, and a project_id scheme that doesn't always match our git-root-
// commit hash), so rows are read generically via `sqlite3 -json` and a
// project-scoped lookup falls back to the most recent session overall.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionID, timeUpdated := findRecentSQLiteSession(dbPath, projectID)
	if sessionID == "" {
		// Project-scoped lookup found nothing — OpenCode's project ID
		// scheme may not match ours. Fall back to the single most recent
		// session across all projects.
		sessionID, timeUpdated = findRecentSQLiteSession(dbPath, "")
	}
	if sessionID == "" {
		return nil, nil
	}

	if timeUpdated != "" {
		if t, ok := parseSQLiteTime(timeUpdated); ok && time.Since(t) > agent.RecentSessionTimeout {
			return nil, nil
		}
		// If we can't parse the time, proceed anyway — better to try than skip
	}

	transcriptData, err := fetchSQLiteMessages(dbPath, sessionID)
	if err != nil || len(transcriptData) == 0 {
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

// findRecentSQLiteSession returns the id and last-updated timestamp (raw,
// unparsed) of the most recent session, optionally scoped to projectID. If
// projectID is empty, it searches across all projects. Rows are read via
// `sqlite3 -json` and decoded generically so this survives column renames.
func findRecentSQLiteSession(dbPath, projectID string) (id, timeUpdated string) {
	var query string
	if projectID != "" {
		query = fmt.Sprintf(
			`SELECT * FROM session WHERE project_id='%s' ORDER BY rowid DESC LIMIT 1;`,
			escapeSQLiteLiteral(projectID),
		)
	} else {
		query = `SELECT * FROM session ORDER BY rowid DESC LIMIT 1;`
	}

	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return "", ""
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal(bytes.TrimSpace(output), &rows); err != nil || len(rows) == 0 {
		return "", ""
	}

	row := rows[0]
	id, _ = row["id"].(string)
	for _, key := range []string{"time_updated", "timeUpdated", "updated_at", "updatedAt"} {
		if v, ok := row[key]; ok && v != nil {
			timeUpdated = fmt.Sprintf("%v", v)
			break
		}
	}
	return id, timeUpdated
}

// parseSQLiteTime parses a timestamp in any of the formats OpenCode has used
// for session.time_updated, including millisecond/second epoch integers.
func parseSQLiteTime(raw string) (time.Time, bool) {
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n > 1e12 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}
	return time.Time{}, false
}

// fetchSQLiteMessages dumps every column of each message row for sessionID as
// a JSON array. Older OpenCode releases nested the message body in a single
// "data" JSON-text column; newer ones use flat columns per field. Both are
// handled by merging any "data" blob's keys onto the row.
func fetchSQLiteMessages(dbPath, sessionID string) ([]byte, error) {
	query := fmt.Sprintf(
		`SELECT * FROM message WHERE session_id='%s' ORDER BY rowid;`,
		escapeSQLiteLiteral(sessionID),
	)
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 {
		return nil, nil
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &rows); err != nil {
		return nil, err
	}

	messages := make([]map[string]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		merged := make(map[string]json.RawMessage, len(row))
		for k, v := range row {
			merged[k] = v
		}
		if dataRaw, ok := row["data"]; ok {
			var dataStr string
			if json.Unmarshal(dataRaw, &dataStr) == nil && dataStr != "" {
				var inner map[string]json.RawMessage
				if json.Unmarshal([]byte(dataStr), &inner) == nil {
					for k, v := range inner {
						merged[k] = v
					}
				}
			}
		}
		messages = append(messages, merged)
	}

	return json.Marshal(messages)
}

// escapeSQLiteLiteral escapes single quotes for embedding a value into a
// SQLite string literal.
func escapeSQLiteLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
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
