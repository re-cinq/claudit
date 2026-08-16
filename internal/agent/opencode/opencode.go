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
//
// OpenCode's on-disk storage has changed shape across releases: pre-v1.2 it
// wrote one flat JSON file per session under a project-scoped directory,
// v1.2+ moved to SQLite, and later releases have continued to shift table
// and column names and even the database's location. Rather than hardcode
// a single layout, we try each known strategy from most to least specific,
// and the SQLite path introspects the schema at runtime instead of
// assuming fixed table/column names, so a rename or restructure upstream
// degrades gracefully instead of silently finding nothing.
func (a *Agent) DiscoverSession(projectPath string) (*agent.SessionInfo, error) {
	if session, _ := a.discoverFromFlatFiles(projectPath); session != nil {
		return session, nil
	}

	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	if session := discoverFromSQLite(dataDir, projectID, projectPath); session != nil {
		return session, nil
	}

	return discoverGeneric(dataDir, projectPath), nil
}

// discoverFromFlatFiles tries the legacy flat file session discovery.
// It first looks under our best-guess project-scoped directory (keyed by
// git root commit hash), then, since newer OpenCode versions may key
// project directories differently, falls back to scanning every project
// directory under storage/session for the most recently modified session.
func (a *Agent) discoverFromFlatFiles(projectPath string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	projectID := GetProjectID(projectPath)
	sessionRoot := filepath.Join(dataDir, "storage", "session")

	if best := latestSessionFile(filepath.Join(sessionRoot, projectID)); best != "" {
		return flatFileSessionInfo(best, projectPath), nil
	}

	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		return nil, nil
	}

	var bestPath string
	var bestModTime time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := latestSessionFile(filepath.Join(sessionRoot, entry.Name()))
		if candidate == "" {
			continue
		}
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if bestPath == "" || info.ModTime().After(bestModTime) {
			bestPath = candidate
			bestModTime = info.ModTime()
		}
	}

	if bestPath == "" {
		return nil, nil
	}
	return flatFileSessionInfo(bestPath, projectPath), nil
}

// latestSessionFile returns the most recently modified *.json session file
// in dir that falls within the recent-session timeout, or "" if none qualify
// or dir doesn't exist.
func latestSessionFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	now := time.Now()
	var best string
	var bestModTime time.Time
	for _, entry := range entries {
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
		if best == "" || modTime.After(bestModTime) {
			best = filepath.Join(dir, entry.Name())
			bestModTime = modTime
		}
	}
	return best
}

func flatFileSessionInfo(sessionFilePath, projectPath string) *agent.SessionInfo {
	info, err := os.Stat(sessionFilePath)
	if err != nil {
		return nil
	}

	sessionID := strings.TrimSuffix(filepath.Base(sessionFilePath), ".json")
	msgDir, _ := GetMessageDir(sessionID)

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: msgDir,
		StartedAt:      info.ModTime().Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// discoverFromSQLite queries an OpenCode SQLite database for the most recent
// session belonging to this project. Table names, column names, and the
// database's location have all changed between OpenCode releases, so this
// discovers the schema at runtime via SQLite's own metadata (sqlite_master,
// PRAGMA table_info) instead of assuming fixed names, and degrades to
// "most recent session regardless of project" if project scoping can't be
// determined, and to an empty transcript if messages can't be read — a
// discovered session with no messages is still useful, whereas assuming
// away discovery entirely is not.
func discoverFromSQLite(dataDir, projectID, projectPath string) *agent.SessionInfo {
	dbPath := findSQLiteDB(dataDir)
	if dbPath == "" {
		return nil
	}

	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil
	}

	tables := sqliteTables(dbPath)
	sessionTable := findTableLike(tables, "session", "sessions")
	if sessionTable == "" {
		return nil
	}

	sessionCols := sqliteColumns(dbPath, sessionTable)
	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil
	}
	timeCol := pickColumn(sessionCols,
		"time_updated", "updated_at", "updatedat", "time_created", "created_at", "createdat", "time")
	projectCol := pickColumn(sessionCols,
		"project_id", "projectid", "directory", "worktree", "cwd", "path")

	orderClause := ""
	if timeCol != "" {
		orderClause = " ORDER BY " + quoteIdent(timeCol) + " DESC"
	}

	var sessionID string
	if projectCol != "" {
		query := fmt.Sprintf(
			"SELECT %s FROM %s WHERE %s='%s' OR %s='%s'%s LIMIT 1;",
			quoteIdent(idCol), quoteIdent(sessionTable),
			quoteIdent(projectCol), sqlQuote(projectID),
			quoteIdent(projectCol), sqlQuote(projectPath),
			orderClause,
		)
		if out, err := exec.Command("sqlite3", dbPath, query).Output(); err == nil {
			sessionID = strings.TrimSpace(string(out))
		}
	}

	if sessionID == "" {
		query := fmt.Sprintf("SELECT %s FROM %s%s LIMIT 1;",
			quoteIdent(idCol), quoteIdent(sessionTable), orderClause)
		out, err := exec.Command("sqlite3", dbPath, query).Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return nil
		}
		sessionID = strings.TrimSpace(string(out))
	}

	if timeCol != "" {
		query := fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s' LIMIT 1;",
			quoteIdent(timeCol), quoteIdent(sessionTable), quoteIdent(idCol), sqlQuote(sessionID))
		if out, err := exec.Command("sqlite3", dbPath, query).Output(); err == nil {
			if !withinRecentTimeout(strings.TrimSpace(string(out))) {
				return nil
			}
		}
	}

	transcriptData := buildTranscriptFromMessages(dbPath, tables, sessionID)
	if len(transcriptData) == 0 {
		transcriptData = []byte("[]")
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "",
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}
}

// findSQLiteDB locates OpenCode's SQLite database, trying known locations
// before falling back to a shallow scan of the data directory.
func findSQLiteDB(dataDir string) string {
	candidates := []string{
		filepath.Join(dataDir, "opencode.db"),
		filepath.Join(dataDir, "storage", "opencode.db"),
		filepath.Join(dataDir, "db.sqlite"),
		filepath.Join(dataDir, "db.sqlite3"),
		filepath.Join(dataDir, "opencode.sqlite"),
		filepath.Join(dataDir, "opencode.sqlite3"),
		filepath.Join(dataDir, "storage", "db.sqlite"),
		filepath.Join(dataDir, "storage", "db.sqlite3"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}

	var found string
	_ = filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || found != "" || info == nil {
			return nil
		}
		if info.IsDir() {
			rel, relErr := filepath.Rel(dataDir, path)
			if relErr == nil && strings.Count(rel, string(filepath.Separator)) > 3 {
				return filepath.SkipDir
			}
			return nil
		}
		name := strings.ToLower(info.Name())
		if strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".sqlite") || strings.HasSuffix(name, ".sqlite3") {
			found = path
		}
		return nil
	})
	return found
}

// sqliteTables returns the names of tables in the database.
func sqliteTables(dbPath string) []string {
	out, err := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';").Output()
	if err != nil {
		return nil
	}
	var tables []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			tables = append(tables, line)
		}
	}
	return tables
}

// findTableLike returns the first table name matching one of the candidates,
// preferring an exact case-insensitive match before falling back to a
// substring match.
func findTableLike(tables []string, candidates ...string) string {
	for _, cand := range candidates {
		for _, t := range tables {
			if strings.EqualFold(t, cand) {
				return t
			}
		}
	}
	for _, cand := range candidates {
		for _, t := range tables {
			if strings.Contains(strings.ToLower(t), strings.ToLower(cand)) {
				return t
			}
		}
	}
	return ""
}

// sqliteColumns returns the column names for a table.
func sqliteColumns(dbPath, table string) []string {
	out, err := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(table))).Output()
	if err != nil {
		return nil
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return cols
}

// pickColumn returns the first candidate that exists in cols
// (case-insensitive), in priority order.
func pickColumn(cols []string, candidates ...string) string {
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.EqualFold(c, cand) {
				return c
			}
		}
	}
	return ""
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func sqlQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// withinRecentTimeout reports whether a raw timestamp value read from
// SQLite (which may be an RFC3339 string or a Unix timestamp in seconds,
// milliseconds, microseconds, or nanoseconds) falls within
// agent.RecentSessionTimeout. Unparseable values don't block discovery.
func withinRecentTimeout(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}

	if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Since(parseEpoch(ms)) <= agent.RecentSessionTimeout
	}

	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}
	return true
}

// parseEpoch converts a Unix timestamp of unknown unit (seconds,
// milliseconds, microseconds, or nanoseconds — OpenCode's schema has used
// different units across versions) into a time.Time.
func parseEpoch(v int64) time.Time {
	switch {
	case v > 1e17:
		return time.Unix(0, v)
	case v > 1e14:
		return time.Unix(0, v*int64(time.Microsecond))
	case v > 1e11:
		return time.Unix(0, v*int64(time.Millisecond))
	default:
		return time.Unix(v, 0)
	}
}

// buildTranscriptFromMessages queries the message table for a session and
// normalizes rows into a JSON array of {id, role, content, time} objects
// that ParseTranscript/parseOpenCodeEntry understand, regardless of the
// underlying column layout — OpenCode has stored full-message JSON blobs,
// typed "parts" arrays, and plain text columns across different versions.
func buildTranscriptFromMessages(dbPath string, tables []string, sessionID string) []byte {
	msgTable := findTableLike(tables, "message", "messages")
	if msgTable == "" {
		return nil
	}
	cols := sqliteColumns(dbPath, msgTable)

	idCol := pickColumn(cols, "id")
	sessionCol := pickColumn(cols, "session_id", "sessionid", "session")
	timeCol := pickColumn(cols, "time_created", "created_at", "createdat", "time", "created")
	dataCol := pickColumn(cols, "data")
	roleCol := pickColumn(cols, "role", "type")
	contentCol := pickColumn(cols, "content", "parts", "body", "text")

	if idCol == "" || sessionCol == "" {
		return nil
	}

	selectCols := []string{quoteIdent(idCol)}
	if dataCol != "" {
		selectCols = append(selectCols, quoteIdent(dataCol))
	}
	if roleCol != "" {
		selectCols = append(selectCols, quoteIdent(roleCol))
	}
	if contentCol != "" && contentCol != dataCol {
		selectCols = append(selectCols, quoteIdent(contentCol))
	}
	if timeCol != "" {
		selectCols = append(selectCols, quoteIdent(timeCol))
	}

	orderClause := ""
	if timeCol != "" {
		orderClause = " ORDER BY " + quoteIdent(timeCol) + " ASC"
	}

	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s='%s'%s;",
		strings.Join(selectCols, ", "), quoteIdent(msgTable),
		quoteIdent(sessionCol), sqlQuote(sessionID), orderClause)

	out, err := exec.Command("sqlite3", "-json", dbPath, query).Output()
	if err != nil {
		return nil
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) == 0 {
		return nil
	}

	var messages []json.RawMessage
	for _, row := range rows {
		msg := map[string]json.RawMessage{}

		if dataCol != "" {
			if raw, ok := row[dataCol]; ok {
				var embedded map[string]json.RawMessage
				if json.Unmarshal(raw, &embedded) == nil {
					msg = embedded
				}
			}
		}

		if raw, ok := row[idCol]; ok {
			msg["id"] = raw
		}
		if roleCol != "" {
			if raw, ok := row[roleCol]; ok {
				if _, exists := msg["role"]; !exists {
					msg["role"] = raw
				}
			}
		}
		if contentCol != "" && contentCol != dataCol {
			if raw, ok := row[contentCol]; ok {
				if _, exists := msg["content"]; !exists {
					msg["content"] = normalizeContentValue(raw)
				}
			}
		}
		if timeCol != "" {
			if raw, ok := row[timeCol]; ok {
				msg["time"] = raw
			}
		}

		encoded, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		messages = append(messages, encoded)
	}

	if len(messages) == 0 {
		return nil
	}

	data, err := json.Marshal(messages)
	if err != nil {
		return nil
	}
	return data
}

// normalizeContentValue converts a raw SQLite TEXT column value for message
// content into the shape parseOpenCodeMessage expects: either a plain JSON
// string, or a JSON array of {type, text} blocks. Newer OpenCode versions
// store a JSON-encoded array of typed "parts" (e.g.
// [{"type":"text","data":{"text":"..."}}]) rather than plain text.
func normalizeContentValue(raw json.RawMessage) json.RawMessage {
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		return raw
	}

	trimmed := strings.TrimSpace(asString)
	if trimmed == "" {
		return raw
	}

	if strings.HasPrefix(trimmed, "[") {
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &parts); err == nil {
			blocks := make([]map[string]interface{}, 0, len(parts))
			for _, part := range parts {
				text := extractPartText(part)
				if text == "" {
					continue
				}
				blocks = append(blocks, map[string]interface{}{
					"type": "text",
					"text": text,
				})
			}
			if len(blocks) > 0 {
				if encoded, err := json.Marshal(blocks); err == nil {
					return encoded
				}
			}
		}
	}

	return raw
}

// extractPartText pulls a text value out of a typed OpenCode message part,
// which may store it directly ("text") or nested under "data".
func extractPartText(part map[string]json.RawMessage) string {
	if t, ok := part["text"]; ok {
		var s string
		if json.Unmarshal(t, &s) == nil {
			return s
		}
	}
	if d, ok := part["data"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(d, &nested) == nil {
			if t, ok := nested["text"]; ok {
				var s string
				if json.Unmarshal(t, &s) == nil {
					return s
				}
			}
		}
	}
	return ""
}

// discoverGeneric is a last-resort fallback for OpenCode layouts not
// otherwise recognized. It walks the data directory for the most recently
// modified session-shaped JSON file (one with a top-level "id" field)
// within the recent-session window, using its containing directory as the
// transcript source.
func discoverGeneric(dataDir, projectPath string) *agent.SessionInfo {
	var bestPath string
	var bestModTime time.Time
	var bestID string

	_ = filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			rel, relErr := filepath.Rel(dataDir, path)
			if relErr == nil && strings.Count(rel, string(filepath.Separator)) > 5 {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}
		if time.Since(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}
		if bestPath != "" && !info.ModTime().After(bestModTime) {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		var probe struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &probe) != nil || probe.ID == "" {
			return nil
		}

		bestPath = path
		bestModTime = info.ModTime()
		bestID = probe.ID
		return nil
	})

	if bestPath == "" {
		return nil
	}

	return &agent.SessionInfo{
		SessionID:      bestID,
		TranscriptPath: filepath.Dir(bestPath),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
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
