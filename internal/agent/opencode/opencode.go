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
// It tries flat file storage (pre-v1.2), then SQLite (v1.2+), then falls back
// to a generic scan for the most recently written session-like file. The
// generic fallback exists because OpenCode's on-disk storage format has
// changed across versions in ways the two specific checks above may not
// recognize.
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
	if session, err := discoverFromSQLite(dataDir, projectID, projectPath); err == nil && session != nil {
		return session, nil
	}

	// Last resort: some OpenCode versions use an on-disk layout that neither
	// check above recognizes. Look for the most recently written
	// session/message file anywhere under the data directory. This is safe
	// because a given data directory in practice belongs to a single active
	// project during discovery (e.g. a fresh XDG_DATA_HOME for a session run).
	return discoverFromRecentFile(dataDir, projectPath)
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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session. Table and column names are introspected rather than hardcoded,
// since OpenCode's on-disk schema has changed across versions. Session
// matching prefers an exact project-ID match, then a project-directory match,
// then simply the most recently updated session in the database (safe because
// a given OpenCode data directory in practice belongs to a single active
// project during discovery).
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	var dbPath string
	for _, candidate := range []string{
		filepath.Join(dataDir, "opencode.db"),
		filepath.Join(projectPath, ".opencode", "opencode.db"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			dbPath = candidate
			break
		}
	}
	if dbPath == "" {
		return nil, nil
	}

	sessionTable := findTable(dbPath, "session")
	messageTable := findTable(dbPath, "message")

	sessionCols := tableColumns(dbPath, sessionTable)
	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		idCol = "id"
	}
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project")
	dirCol := pickColumn(sessionCols, "directory", "cwd", "path", "worktree")
	timeCol := pickColumn(sessionCols, "time_updated", "updated_at", "updatedat", "updated")
	if timeCol == "" {
		timeCol = pickColumn(sessionCols, "time_created", "created_at", "createdat", "created")
	}

	orderClause := "rowid"
	if timeCol != "" {
		orderClause = timeCol
	}

	var whereClauses []string
	if projectCol != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("%s='%s'", projectCol, escapeSQLString(projectID)))
	}
	if dirCol != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("%s='%s'", dirCol, escapeSQLString(projectPath)))
	}
	whereClauses = append(whereClauses, "1=1")

	var sessionID string
	for _, where := range whereClauses {
		query := fmt.Sprintf(
			`SELECT %s FROM %s WHERE %s ORDER BY %s DESC LIMIT 1;`,
			idCol, sessionTable, where, orderClause,
		)
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		if s := strings.TrimSpace(string(out)); s != "" {
			sessionID = s
			break
		}
	}
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout). Best-effort: if the
	// time column or its format can't be determined, proceed anyway rather
	// than skip a session we can't otherwise verify.
	if timeCol != "" {
		timeQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s='%s';`, timeCol, sessionTable, idCol, escapeSQLString(sessionID))
		cmd := exec.Command("sqlite3", dbPath, timeQuery)
		if timeOutput, err := cmd.Output(); err == nil {
			timeStr := strings.TrimSpace(string(timeOutput))
			parsed := false
			for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
				if t, err := time.Parse(layout, timeStr); err == nil {
					parsed = true
					if time.Since(t) > agent.RecentSessionTimeout {
						return nil, nil
					}
					break
				}
			}
			// Unix timestamps (seconds or milliseconds) are common too.
			if !parsed {
				if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
					t := time.UnixMilli(n)
					if n < 1e12 {
						t = time.Unix(n, 0)
					}
					if time.Since(t) > agent.RecentSessionTimeout {
						return nil, nil
					}
				}
			}
			// If we still can't parse the time, proceed anyway — better to try than skip
		}
	}

	// Get messages for this session as a JSON array
	messageCols := tableColumns(dbPath, messageTable)
	msgIDCol := pickColumn(messageCols, "id")
	if msgIDCol == "" {
		msgIDCol = "id"
	}
	sessionRefCol := pickColumn(messageCols, "session_id", "sessionid", "session")
	if sessionRefCol == "" {
		sessionRefCol = "session_id"
	}
	payloadCol := pickColumn(messageCols, "data", "content", "parts", "body")
	orderCol := pickColumn(messageCols, "time_created", "created_at", "createdat", "created")
	if orderCol == "" {
		orderCol = "rowid"
	}

	var msgSelect string
	switch {
	case payloadCol == "":
		msgSelect = fmt.Sprintf("json_object('id', %s)", msgIDCol)
	case strings.EqualFold(payloadCol, "data"):
		msgSelect = fmt.Sprintf("json_patch(%s, json_object('id', %s))", payloadCol, msgIDCol)
	default:
		fields := []string{fmt.Sprintf("'id', %s", msgIDCol)}
		if roleCol := pickColumn(messageCols, "role"); roleCol != "" {
			fields = append(fields, fmt.Sprintf("'role', COALESCE(%s, '')", roleCol))
		}
		fields = append(fields, fmt.Sprintf("'%s', json(%s)", payloadCol, payloadCol))
		msgSelect = fmt.Sprintf("json_object(%s)", strings.Join(fields, ", "))
	}

	msgQuery := fmt.Sprintf(
		`SELECT json_group_array(%s) FROM %s WHERE %s='%s' ORDER BY %s;`,
		msgSelect, messageTable, sessionRefCol, escapeSQLString(sessionID), orderCol,
	)
	cmd := exec.Command("sqlite3", dbPath, msgQuery)
	msgOutput, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	transcriptData := []byte(strings.TrimSpace(string(msgOutput)))
	// sqlite3 returns "[null]" when no rows match
	if len(transcriptData) == 0 || string(transcriptData) == "[null]" || string(transcriptData) == "[]" {
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

// discoverFromRecentFile is a resilience fallback for OpenCode storage layouts
// that don't match the flat-file or SQLite schemes above. It looks for the
// most recently modified session/message file anywhere under the data
// directory, which is a reliable signal when the data directory belongs to a
// single active project during discovery.
func discoverFromRecentFile(dataDir, projectPath string) (*agent.SessionInfo, error) {
	path, modTime, err := findRecentDataFile(dataDir, agent.RecentSessionTimeout)
	if err != nil || path == "" {
		return nil, nil
	}

	// If sibling session/message files exist alongside the discovered file,
	// treat the containing directory as the transcript source so they're all
	// combined; otherwise use the single file directly.
	transcriptPath := path
	if dir := filepath.Dir(path); dir != dataDir {
		if entries, err := os.ReadDir(dir); err == nil {
			count := 0
			for _, e := range entries {
				if !e.IsDir() && (strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".jsonl")) {
					count++
				}
			}
			if count > 1 {
				transcriptPath = dir
			}
		}
	}

	return &agent.SessionInfo{
		SessionID:      deriveSessionID(path),
		TranscriptPath: transcriptPath,
		StartedAt:      modTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
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

	// Try "parts" array (typed content blocks used by newer OpenCode versions,
	// e.g. {"type": "text", "text": "..."} or {"type": "text", "data": {"text": "..."}}).
	if partsRaw, ok := raw["parts"]; ok {
		var parts []struct {
			Type string          `json:"type"`
			Text string          `json:"text"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(partsRaw, &parts); err == nil {
			var blocks []agent.ContentBlock
			for _, p := range parts {
				text := p.Text
				if text == "" && len(p.Data) > 0 {
					var d struct {
						Text string `json:"text"`
					}
					if json.Unmarshal(p.Data, &d) == nil {
						text = d.Text
					}
				}
				if (p.Type == "text" || p.Type == "reasoning") && text != "" {
					blocks = append(blocks, agent.ContentBlock{Type: "text", Text: text})
				}
			}
			if len(blocks) > 0 {
				msg.Content = blocks
				return msg
			}
		}
	}

	return msg
}
