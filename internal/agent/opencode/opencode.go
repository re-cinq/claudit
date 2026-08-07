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
// OpenCode's on-disk storage layout has changed across versions (per-project
// flat files, a global session index, and briefly SQLite), so discovery
// tries each known layout in turn, from newest to oldest.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	// Try the current global session-index layout (OpenCode v1.x+), where
	// sessions are no longer partitioned into per-project directories but
	// instead carry their own "directory"/"projectID" field.
	session, err := a.discoverFromSessionInfo(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Try flat file storage (legacy, pre-v1.2 OpenCode)
	session, err = a.discoverFromFlatFiles(projectPath)
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session, nil
	}

	// Fall back to SQLite (some OpenCode v1.2.x builds)
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	return discoverFromSQLite(dataDir, projectID, projectPath)
}

// sessionInfoCandidate holds the fields discoverFromSessionInfo cares about
// from a session metadata JSON file, tolerant of the exact field names
// OpenCode uses for the project-scoping directory.
type sessionInfoCandidate struct {
	ID        string
	Directory string
	ProjectID string
	UpdatedAt int64 // epoch milliseconds, 0 if unknown
}

// extractSessionCandidate pulls session-identifying fields out of a raw JSON
// object, tolerating the several field names OpenCode has used for the
// session's working directory across versions.
func extractSessionCandidate(raw map[string]json.RawMessage) (sessionInfoCandidate, bool) {
	var c sessionInfoCandidate

	if v, ok := raw["id"]; ok {
		_ = json.Unmarshal(v, &c.ID)
	}
	if c.ID == "" {
		return c, false
	}

	for _, key := range []string{"directory", "cwd", "worktree", "path", "root", "workdir"} {
		if v, ok := raw[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				c.Directory = s
				break
			}
		}
	}

	if v, ok := raw["projectID"]; ok {
		_ = json.Unmarshal(v, &c.ProjectID)
	}

	if v, ok := raw["time"]; ok {
		var t struct {
			Created int64 `json:"created"`
			Updated int64 `json:"updated"`
		}
		if json.Unmarshal(v, &t) == nil {
			c.UpdatedAt = t.Updated
			if c.UpdatedAt == 0 {
				c.UpdatedAt = t.Created
			}
		}
	}
	if c.UpdatedAt == 0 {
		if v, ok := raw["updated"]; ok {
			_ = json.Unmarshal(v, &c.UpdatedAt)
		}
	}

	return c, true
}

// discoverFromSessionInfo scans OpenCode's session metadata files for one
// belonging to projectPath. Newer OpenCode versions keep a single global
// index of session files (rather than nesting them under a per-project
// directory), so this walks the whole session storage tree and matches on
// each session's own directory/project fields instead of relying on a fixed
// path layout.
func (a *Agent) discoverFromSessionInfo(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	if _, err := os.Stat(sessionRoot); err != nil {
		return nil, nil
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout
	projectID := GetProjectID(projectPath)
	cleanProject := filepath.Clean(projectPath)

	var bestID string
	var bestUpdated time.Time

	_ = filepath.WalkDir(sessionRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		// Message/part files live under their own subtrees and aren't
		// session metadata, so skip them.
		sep := string(filepath.Separator)
		if strings.Contains(path, sep+"message"+sep) || strings.Contains(path, sep+"part"+sep) {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}

		var raw map[string]json.RawMessage
		if json.Unmarshal(data, &raw) != nil {
			return nil
		}

		candidate, ok := extractSessionCandidate(raw)
		if !ok {
			return nil
		}

		matches := candidate.Directory != "" && filepath.Clean(candidate.Directory) == cleanProject
		if !matches && candidate.Directory != "" {
			matches = agent.PathsEqual(candidate.Directory, projectPath)
		}
		if !matches {
			matches = candidate.ProjectID != "" && candidate.ProjectID == projectID
		}
		if !matches {
			return nil
		}

		var updated time.Time
		if candidate.UpdatedAt > 0 {
			updated = time.UnixMilli(candidate.UpdatedAt)
		} else if info, statErr := d.Info(); statErr == nil {
			updated = info.ModTime()
		}
		if now.Sub(updated) > recentTimeout {
			return nil
		}

		if bestID == "" || updated.After(bestUpdated) {
			bestID = candidate.ID
			bestUpdated = updated
		}
		return nil
	})

	if bestID == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestID,
		StartedAt:      bestUpdated.Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: buildTranscriptFromMessages(bestID),
	}, nil
}

// buildTranscriptFromMessages reconstructs a transcript JSON array for a
// session directly from OpenCode's on-disk message (and, if needed, part)
// files, since newer OpenCode versions may split message content into
// separate "part" files alongside message metadata. Always returns a
// non-nil, valid JSON array, even if no messages could be read.
func buildTranscriptFromMessages(sessionID string) []byte {
	type outMessage struct {
		Role    string                `json:"role,omitempty"`
		ID      string                `json:"id,omitempty"`
		Content []agent.ContentBlock  `json:"content,omitempty"`
	}

	partsByMessage := map[string][]agent.ContentBlock{}
	if partDirs, err := GetSessionPartDirs(sessionID); err == nil {
		for _, partDir := range partDirs {
			msgDirs, err := os.ReadDir(partDir)
			if err != nil {
				continue
			}
			for _, msgDirEntry := range msgDirs {
				if !msgDirEntry.IsDir() {
					continue
				}
				msgID := msgDirEntry.Name()
				partFiles, err := os.ReadDir(filepath.Join(partDir, msgID))
				if err != nil {
					continue
				}
				for _, pf := range partFiles {
					if pf.IsDir() || !strings.HasSuffix(pf.Name(), ".json") {
						continue
					}
					data, err := os.ReadFile(filepath.Join(partDir, msgID, pf.Name()))
					if err != nil {
						continue
					}
					var part struct {
						Type string `json:"type"`
						Text string `json:"text"`
					}
					if json.Unmarshal(data, &part) == nil && part.Text != "" {
						partsByMessage[msgID] = append(partsByMessage[msgID], agent.ContentBlock{Type: "text", Text: part.Text})
					}
				}
			}
		}
	}

	var out []outMessage
	seen := map[string]bool{}

	if msgDirs, err := GetSessionMessageDirs(sessionID); err == nil {
		for _, msgDir := range msgDirs {
			entries, err := os.ReadDir(msgDir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				id := strings.TrimSuffix(e.Name(), ".json")
				if seen[id] {
					continue
				}

				data, err := os.ReadFile(filepath.Join(msgDir, e.Name()))
				if err != nil {
					continue
				}

				var raw map[string]json.RawMessage
				if json.Unmarshal(data, &raw) != nil {
					continue
				}

				var role string
				if v, ok := raw["role"]; ok {
					_ = json.Unmarshal(v, &role)
				}
				if role == "" {
					continue
				}
				if v, ok := raw["id"]; ok {
					var msgID string
					if json.Unmarshal(v, &msgID) == nil && msgID != "" {
						id = msgID
					}
				}

				content := extractInlineContent(raw)
				if len(content) == 0 {
					content = partsByMessage[id]
				}

				seen[id] = true
				out = append(out, outMessage{Role: role, ID: id, Content: content})
			}
		}
	}

	data, err := json.Marshal(out)
	if err != nil || string(data) == "null" {
		return []byte("[]")
	}
	return data
}

// extractInlineContent extracts message content directly embedded in a
// message JSON object, for OpenCode versions that don't split content into
// separate part files.
func extractInlineContent(raw map[string]json.RawMessage) []agent.ContentBlock {
	contentRaw, ok := raw["content"]
	if !ok {
		return nil
	}

	var text string
	if err := json.Unmarshal(contentRaw, &text); err == nil && text != "" {
		return []agent.ContentBlock{{Type: "text", Text: text}}
	}

	var blocks []agent.ContentBlock
	if err := json.Unmarshal(contentRaw, &blocks); err == nil && len(blocks) > 0 {
		return blocks
	}

	return nil
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
