package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
)

// GetDataDir returns the OpenCode data directory.
// OpenCode follows XDG conventions: it uses $XDG_DATA_HOME/opencode on Linux
// and ~/Library/Application Support/opencode on macOS.
func GetDataDir() (string, error) {
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("could not determine home directory: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", "opencode"), nil
	}

	// Linux/other: respect XDG_DATA_HOME, default to ~/.local/share
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "opencode"), nil
}

// GetProjectID returns the project identifier for OpenCode.
// For git repos, this is the root commit hash. For non-git dirs, it's "global".
func GetProjectID(projectPath string) string {
	cmd := exec.Command("git", "rev-list", "--max-parents=0", "--all")
	cmd.Dir = projectPath
	output, err := cmd.Output()
	if err != nil {
		return "global"
	}

	// Take the first line (first root commit)
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) > 0 && lines[0] != "" {
		return strings.TrimSpace(lines[0])
	}
	return "global"
}

// GetSessionDir returns the session storage directory for a project.
func GetSessionDir(projectPath string) (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}

	projectID := GetProjectID(projectPath)
	return filepath.Join(dataDir, "storage", "session", projectID), nil
}

// GetMessageDir returns the message storage directory for a session.
func GetMessageDir(sessionID string) (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(dataDir, "storage", "message", sessionID), nil
}

// sessionInfo represents an OpenCode session JSON file.
type sessionInfo struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID,omitempty"`
	Directory string `json:"directory,omitempty"`
	Title     string `json:"title,omitempty"`
}

// WriteSessionFile writes a session and its messages to OpenCode's storage.
func WriteSessionFile(projectPath, sessionID string, transcriptData []byte) (string, error) {
	sessionDir, err := GetSessionDir(projectPath)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return "", fmt.Errorf("could not create session directory: %w", err)
	}

	sessionPath := filepath.Join(sessionDir, sessionID+".json")

	// Write a minimal session file
	session := sessionInfo{
		ID:        sessionID,
		ProjectID: GetProjectID(projectPath),
		Directory: projectPath,
		Title:     "Restored session",
	}

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return "", fmt.Errorf("could not marshal session: %w", err)
	}

	if err := os.WriteFile(sessionPath, data, 0600); err != nil {
		return "", fmt.Errorf("could not write session file: %w", err)
	}

	// Write messages from transcript data
	msgDir, err := GetMessageDir(sessionID)
	if err != nil {
		return sessionPath, nil // Session created, messages optional
	}

	if err := os.MkdirAll(msgDir, 0700); err != nil {
		return sessionPath, nil
	}

	// Write the raw transcript data as a single message file for restore
	msgPath := filepath.Join(msgDir, "transcript.jsonl")
	_ = os.WriteFile(msgPath, transcriptData, 0600)

	return sessionPath, nil
}

// sqliteJSONRows runs a sqlite3 query with -json output and returns the
// decoded rows. Returns (nil, err) if sqlite3 is unavailable, the query
// fails, or the output isn't valid JSON, so callers can fall back gracefully.
func sqliteJSONRows(dbPath, query string) ([]map[string]interface{}, error) {
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, nil
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// escapeSQLiteString escapes single quotes for use in a single-quoted SQLite
// string literal.
func escapeSQLiteString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// findTableLike returns the name of a table in the database matching (or
// containing) the given substring, case-insensitively. Used to tolerate
// OpenCode CLI renaming its tables across versions.
func findTableLike(dbPath, substr string) (string, error) {
	rows, err := sqliteJSONRows(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return "", err
	}
	var contains string
	for _, r := range rows {
		name, _ := r["name"].(string)
		lower := strings.ToLower(name)
		if lower == substr || lower == substr+"s" {
			return name, nil
		}
		if contains == "" && strings.Contains(lower, substr) {
			contains = name
		}
	}
	if contains == "" {
		return "", fmt.Errorf("no table matching %q found in %s", substr, dbPath)
	}
	return contains, nil
}

// tableColumns returns the column names of a SQLite table.
func tableColumns(dbPath, table string) ([]string, error) {
	rows, err := sqliteJSONRows(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, r := range rows {
		if name, ok := r["name"].(string); ok {
			cols = append(cols, name)
		}
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("no columns found for table %s", table)
	}
	return cols, nil
}

// findColumnLike returns the first column matching any candidate substring,
// in priority order, matched case-insensitively. Returns "" if none match.
func findColumnLike(columns []string, substrs ...string) string {
	for _, substr := range substrs {
		for _, c := range columns {
			if strings.Contains(strings.ToLower(c), substr) {
				return c
			}
		}
	}
	return ""
}

// hasColumn reports whether columns contains the exact column name.
func hasColumn(columns []string, name string) bool {
	for _, c := range columns {
		if c == name {
			return true
		}
	}
	return false
}

// isRecentTimestamp reports whether the given timestamp (in one of
// OpenCode's known formats) is within RecentSessionTimeout. Unparseable
// timestamps are treated as recent — better to try than skip.
func isRecentTimestamp(timeStr string) bool {
	formats := []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
	for _, f := range formats {
		if t, err := time.Parse(f, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}
	return true
}

// findRecentSession finds the most recently updated session ID for the given
// project, trying OpenCode's known schema first (table `session`, columns
// `project_id`/`time_updated`) and falling back to schema introspection, and
// finally to "the single most recent session in the database", to tolerate
// OpenCode CLI storage changes across versions.
func findRecentSession(dbPath, projectID string) string {
	if id := findRecentSessionKnownSchema(dbPath, projectID); id != "" {
		return id
	}
	return findRecentSessionFallback(dbPath, projectID)
}

func findRecentSessionKnownSchema(dbPath, projectID string) string {
	query := fmt.Sprintf(
		`SELECT id FROM session WHERE project_id='%s' ORDER BY time_updated DESC LIMIT 1;`,
		escapeSQLiteString(projectID),
	)
	rows, err := sqliteJSONRows(dbPath, query)
	if err != nil || len(rows) == 0 {
		return ""
	}
	sessionID, _ := rows[0]["id"].(string)
	if sessionID == "" {
		return ""
	}

	timeQuery := fmt.Sprintf(`SELECT time_updated FROM session WHERE id='%s';`, escapeSQLiteString(sessionID))
	if timeRows, err := sqliteJSONRows(dbPath, timeQuery); err == nil && len(timeRows) > 0 {
		timeStr, _ := timeRows[0]["time_updated"].(string)
		if timeStr != "" && !isRecentTimestamp(timeStr) {
			return ""
		}
	}

	return sessionID
}

// findRecentSessionFallback introspects the session table's schema to locate
// the most recently updated session, first scoped to the project and then,
// if that yields nothing, across the whole database. This tolerates renamed
// tables/columns and project-identification schemes, at the cost of the
// recency guarantee applied in findRecentSessionKnownSchema above.
func findRecentSessionFallback(dbPath, projectID string) string {
	table, err := findTableLike(dbPath, "session")
	if err != nil {
		return ""
	}
	columns, err := tableColumns(dbPath, table)
	if err != nil {
		return ""
	}

	idCol := "id"
	if !hasColumn(columns, idCol) {
		idCol = findColumnLike(columns, "id")
		if idCol == "" {
			return ""
		}
	}

	timeCol := findColumnLike(columns, "update", "modif", "time", "creat")
	orderBy := "ROWID DESC"
	if timeCol != "" {
		orderBy = fmt.Sprintf("%s DESC", timeCol)
	}

	if projectCol := findColumnLike(columns, "project", "directory", "cwd", "worktree", "path"); projectCol != "" {
		query := fmt.Sprintf(`SELECT %s AS id FROM %s WHERE %s='%s' ORDER BY %s LIMIT 1;`,
			idCol, table, projectCol, escapeSQLiteString(projectID), orderBy)
		if rows, err := sqliteJSONRows(dbPath, query); err == nil && len(rows) > 0 {
			if id, _ := rows[0]["id"].(string); id != "" {
				return id
			}
		}
	}

	// Last resort: the single most recently updated session in the whole
	// database, ignoring the project filter. Safe for the common case of one
	// active session at a time (e.g. CI/test environments).
	query := fmt.Sprintf(`SELECT %s AS id FROM %s ORDER BY %s LIMIT 1;`, idCol, table, orderBy)
	if rows, err := sqliteJSONRows(dbPath, query); err == nil && len(rows) > 0 {
		if id, _ := rows[0]["id"].(string); id != "" {
			return id
		}
	}

	return ""
}

// runScalarQuery runs a query expected to return a single scalar value (e.g.
// a JSON array produced by json_group_array) and returns its raw bytes.
func runScalarQuery(dbPath, query string) []byte {
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	return []byte(strings.TrimSpace(string(output)))
}

// isNonEmptyJSONArray reports whether data is a JSON array with at least one
// element (sqlite3 returns "[null]" or "[]" when no rows match).
func isNonEmptyJSONArray(data []byte) bool {
	s := strings.TrimSpace(string(data))
	return s != "" && s != "[null]" && s != "[]"
}

// fetchSessionMessages fetches all messages for a session as a JSON array,
// trying OpenCode's known schema first (table `message`, columns
// `session_id`/`data`/`time_created`) and falling back to schema
// introspection to tolerate OpenCode CLI storage changes across versions.
func fetchSessionMessages(dbPath, sessionID string) []byte {
	query := fmt.Sprintf(
		`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE session_id='%s' ORDER BY time_created;`,
		escapeSQLiteString(sessionID),
	)
	if data := runScalarQuery(dbPath, query); isNonEmptyJSONArray(data) {
		return data
	}

	return fetchSessionMessagesFallback(dbPath, sessionID)
}

// fetchSessionMessagesFallback introspects the message table's schema and
// reconstructs a JSON array of messages without relying on column names or
// SQL functions (like json_patch) that may not apply to the real content
// shape in newer OpenCode CLI versions.
func fetchSessionMessagesFallback(dbPath, sessionID string) []byte {
	table, err := findTableLike(dbPath, "message")
	if err != nil {
		return nil
	}
	columns, err := tableColumns(dbPath, table)
	if err != nil {
		return nil
	}

	sessionCol := findColumnLike(columns, "session")
	contentCol := findColumnLike(columns, "data", "parts", "content", "body")
	if sessionCol == "" || contentCol == "" {
		return nil
	}

	idCol := "id"
	if !hasColumn(columns, idCol) {
		idCol = findColumnLike(columns, "id")
		if idCol == "" {
			idCol = "ROWID"
		}
	}

	orderCol := findColumnLike(columns, "creat", "time", "order", "index")
	orderBy := "ROWID"
	if orderCol != "" {
		orderBy = orderCol
	}

	query := fmt.Sprintf(`SELECT %s AS id, %s AS content FROM %s WHERE %s='%s' ORDER BY %s;`,
		idCol, contentCol, table, sessionCol, escapeSQLiteString(sessionID), orderBy)
	rows, err := sqliteJSONRows(dbPath, query)
	if err != nil || len(rows) == 0 {
		return nil
	}

	var messages []json.RawMessage
	for _, r := range rows {
		contentStr, _ := r["content"].(string)
		if contentStr == "" {
			continue
		}
		id, _ := r["id"].(string)

		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(contentStr), &obj); err == nil {
			if id != "" {
				if idBytes, err := json.Marshal(id); err == nil {
					obj["id"] = idBytes
				}
			}
			if merged, err := json.Marshal(obj); err == nil {
				messages = append(messages, merged)
				continue
			}
		}
		messages = append(messages, json.RawMessage(contentStr))
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
