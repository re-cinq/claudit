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
//
// OpenCode's on-disk layout and SQLite schema have both drifted across
// releases (project ID encoding, column names), so each backend is queried
// defensively through a series of degrading fallback lookups rather than
// assuming one fixed shape.
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

// discoverFromFlatFiles tries the legacy flat file session discovery.
// It first looks under the project-scoped session directory; if that
// directory is missing or empty (e.g. because OpenCode's project ID scheme
// no longer matches ours), it falls back to scanning every project
// directory for the most recently modified session file.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	sessionRoot := filepath.Join(dataDir, "storage", "session")

	bestSessionID, bestModTime := mostRecentSessionFile(filepath.Join(sessionRoot, projectID))
	if bestSessionID == "" {
		bestSessionID, bestModTime = mostRecentSessionFileAcrossProjects(sessionRoot)
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

// mostRecentSessionFile returns the session ID and mod time of the most
// recently modified session JSON file directly inside dir, ignoring files
// older than agent.RecentSessionTimeout. Returns "" if none qualify.
func mostRecentSessionFile(dir string) (string, time.Time) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}
	}

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

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = strings.TrimSuffix(entry.Name(), ".json")
			bestModTime = modTime
		}
	}

	return bestSessionID, bestModTime
}

// mostRecentSessionFileAcrossProjects scans every project subdirectory under
// sessionRoot for the most recently modified session file. Used as a hedge
// against project ID encoding changes that would otherwise hide sessions
// under a directory we don't know to look in.
func mostRecentSessionFileAcrossProjects(sessionRoot string) (string, time.Time) {
	projectDirs, err := os.ReadDir(sessionRoot)
	if err != nil {
		return "", time.Time{}
	}

	var bestSessionID string
	var bestModTime time.Time

	for _, projectDir := range projectDirs {
		if !projectDir.IsDir() {
			continue
		}

		sessionID, modTime := mostRecentSessionFile(filepath.Join(sessionRoot, projectDir.Name()))
		if sessionID == "" {
			continue
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = sessionID
			bestModTime = modTime
		}
	}

	return bestSessionID, bestModTime
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
//
// OpenCode's SQLite schema has changed across releases (e.g. the column(s)
// used to scope a session to a project), so lookups are attempted through a
// series of degrading fallback queries rather than one fixed schema.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Check sqlite3 is available
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionID := findMostRecentSessionID(dbPath, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	if !isSessionRecent(dbPath, sessionID) {
		return nil, nil
	}

	transcriptData := fetchSessionMessages(dbPath, sessionID)
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

// findMostRecentSessionID finds the most recently updated session for a
// project. It tries progressively looser queries as a hedge against schema
// changes in how OpenCode scopes sessions to a project: a renamed/removed
// project_id or time_updated column no longer aborts discovery entirely.
func findMostRecentSessionID(dbPath, projectID, projectPath string) string {
	queries := []string{
		fmt.Sprintf(`SELECT id FROM session WHERE project_id='%s' ORDER BY time_updated DESC LIMIT 1;`, projectID),
		fmt.Sprintf(`SELECT id FROM session WHERE directory='%s' ORDER BY time_updated DESC LIMIT 1;`, projectPath),
		`SELECT id FROM session ORDER BY time_updated DESC LIMIT 1;`,
		`SELECT id FROM session ORDER BY rowid DESC LIMIT 1;`,
	}

	for _, query := range queries {
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		output, err := cmd.Output()
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(string(output)); id != "" {
			return id
		}
	}

	return ""
}

// isSessionRecent reports whether a session was updated within
// agent.RecentSessionTimeout. If the timestamp column is missing or its
// value can't be parsed, it assumes the session is recent rather than
// skipping it — better to try than to silently drop a valid session.
func isSessionRecent(dbPath, sessionID string) bool {
	timeQuery := fmt.Sprintf(`SELECT time_updated FROM session WHERE id='%s';`, sessionID)
	cmd := exec.Command("sqlite3", dbPath, timeQuery)
	output, err := cmd.Output()
	if err != nil {
		return true
	}

	timeStr := strings.TrimSpace(string(output))
	if timeStr == "" {
		return true
	}

	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, format := range formats {
		if t, err := time.Parse(format, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	return true
}

// fetchSessionMessages fetches all messages for a session as a JSON array.
// It tries progressively looser queries as a hedge against schema changes in
// how OpenCode names its message columns (e.g. session_id vs sessionID).
func fetchSessionMessages(dbPath, sessionID string) []byte {
	queries := []string{
		fmt.Sprintf(`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE session_id='%s' ORDER BY time_created;`, sessionID),
		fmt.Sprintf(`SELECT json_group_array(data) FROM message WHERE session_id='%s';`, sessionID),
		fmt.Sprintf(`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE sessionID='%s' ORDER BY time_created;`, sessionID),
	}

	for _, query := range queries {
		cmd := exec.Command("sqlite3", dbPath, query)
		output, err := cmd.Output()
		if err != nil {
			continue
		}

		data := []byte(strings.TrimSpace(string(output)))
		// sqlite3 returns "[null]" when no rows match
		if len(data) == 0 || string(data) == "[null]" || string(data) == "[]" {
			continue
		}

		return data
	}

	return nil
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
		"glob":     "Grep",
	}
}

// RestoreSession finds an active or recent OpenCode session.
