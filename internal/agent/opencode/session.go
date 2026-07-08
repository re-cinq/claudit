package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
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

// --- SQLite schema-adaptive session discovery ---
//
// OpenCode's internal SQLite schema (table/column names) is an undocumented
// implementation detail that has changed across releases. Rather than
// hard-coding a single assumed schema (which breaks silently whenever
// OpenCode changes its storage layout), we try the historically-known
// schema first, then fall back to introspecting the live database via
// sqlite_master/PRAGMA table_info and matching columns by name heuristics.

var sqlIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// isSafeIdent reports whether s is safe to interpolate as a bare SQL
// identifier (table/column name). Values come from sqlite_master/PRAGMA
// output, not user input, but we validate defensively since they are
// concatenated directly into query strings (SQL identifiers cannot be
// bound as query parameters).
func isSafeIdent(s string) bool {
	return sqlIdentRe.MatchString(s)
}

// sqlEscape escapes a string for safe inclusion inside single-quoted SQL
// string literals.
func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// sqlite3Query runs a query against dbPath using the sqlite3 CLI and
// returns raw stdout. If separator is non-empty, columns are separated by
// it (default sqlite3 separator is "|").
func sqlite3Query(dbPath, separator, query string) (string, error) {
	var args []string
	if separator != "" {
		args = append(args, "-separator", separator)
	}
	args = append(args, dbPath, query)
	out, err := exec.Command("sqlite3", args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// sqliteTableNames lists all table names in the database.
func sqliteTableNames(dbPath string) ([]string, error) {
	out, err := sqlite3Query(dbPath, "", "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// sqliteTableColumns lists the column names of table via PRAGMA table_info.
func sqliteTableColumns(dbPath, table string) ([]string, error) {
	if !isSafeIdent(table) {
		return nil, fmt.Errorf("unsafe table identifier: %q", table)
	}
	out, err := sqlite3Query(dbPath, "|", fmt.Sprintf("PRAGMA table_info(%s);", table))
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) >= 2 {
			cols = append(cols, parts[1])
		}
	}
	return cols, nil
}

// findBestTable returns the table name that best matches mustContain
// (substring, case-insensitive) while not matching any of mustNotContain.
// An exact (case-insensitive) match wins immediately.
func findBestTable(tables []string, mustContain string, mustNotContain ...string) string {
	for _, t := range tables {
		if strings.EqualFold(t, mustContain) {
			return t
		}
	}
	for _, t := range tables {
		lower := strings.ToLower(t)
		if !strings.Contains(lower, mustContain) {
			continue
		}
		skip := false
		for _, bad := range mustNotContain {
			if strings.Contains(lower, bad) {
				skip = true
				break
			}
		}
		if !skip {
			return t
		}
	}
	return ""
}

// findColumn returns the first column matching one of the preferred exact
// (case-insensitive) names, falling back to the first column containing one
// of the given substrings.
func findColumn(cols []string, preferred []string, contains []string) string {
	for _, p := range preferred {
		for _, c := range cols {
			if strings.EqualFold(c, p) {
				return c
			}
		}
	}
	for _, c := range cols {
		lower := strings.ToLower(c)
		for _, sub := range contains {
			if strings.Contains(lower, sub) {
				return c
			}
		}
	}
	return ""
}

// trySessionQuery runs query, which must select an id column optionally
// followed by a time column (tab-separated), and reports whether a usable
// row was returned.
func trySessionQuery(dbPath, query string) (id, timeStr string, ok bool) {
	out, err := sqlite3Query(dbPath, "\t", query)
	if err != nil {
		return "", "", false
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return "", "", false
	}
	line = strings.SplitN(line, "\n", 2)[0]
	parts := strings.SplitN(line, "\t", 2)
	id = strings.TrimSpace(parts[0])
	if id == "" || strings.EqualFold(id, "null") {
		return "", "", false
	}
	if len(parts) > 1 {
		timeStr = strings.TrimSpace(parts[1])
	}
	return id, timeStr, true
}

// tryMessageQuery runs query, which must select a single JSON array column,
// and returns the raw bytes, or nil if the query failed or produced no data.
func tryMessageQuery(dbPath, query string) []byte {
	out, err := sqlite3Query(dbPath, "", query)
	if err != nil {
		return nil
	}
	data := strings.TrimSpace(out)
	if data == "" || data == "[null]" || data == "[]" {
		return nil
	}
	return []byte(data)
}

// findRecentSessionID locates the most relevant OpenCode session for
// projectPath in the SQLite database at dbPath, returning its id and raw
// "updated" timestamp string (format unknown ahead of time; see
// parseFlexibleTime). It first tries the historically-known schema
// (session.project_id / session.time_updated), then falls back to
// introspecting the live schema and matching by working directory.
func findRecentSessionID(dbPath, projectID, projectPath string) (sessionID, timeStr string) {
	// Fast path: historically-known schema.
	if id, ts, ok := trySessionQuery(dbPath, fmt.Sprintf(
		`SELECT id, time_updated FROM session WHERE project_id='%s' ORDER BY time_updated DESC LIMIT 1;`,
		sqlEscape(projectID))); ok {
		return id, ts
	}

	tables, err := sqliteTableNames(dbPath)
	if err != nil || len(tables) == 0 {
		return "", ""
	}

	sessionTable := findBestTable(tables, "session", "message", "part")
	if sessionTable == "" {
		return "", ""
	}
	cols, err := sqliteTableColumns(dbPath, sessionTable)
	if err != nil || len(cols) == 0 {
		return "", ""
	}

	idCol := findColumn(cols, []string{"id"}, []string{"id"})
	if idCol == "" || !isSafeIdent(idCol) {
		return "", ""
	}
	timeCol := findColumn(cols, []string{"time_updated", "updated_at", "updatedat"}, []string{"updated", "time", "created"})
	dirCol := findColumn(cols, []string{"directory", "cwd", "path"}, []string{"directory", "cwd", "path"})
	projCol := findColumn(cols, []string{"project_id", "projectid"}, []string{"project"})

	orderCol := idCol
	if timeCol != "" && isSafeIdent(timeCol) {
		orderCol = timeCol
	}
	selectCols := idCol
	if timeCol != "" && isSafeIdent(timeCol) {
		selectCols = idCol + ", " + timeCol
	}

	var candidates []string
	esc := sqlEscape(projectPath)
	if dirCol != "" && isSafeIdent(dirCol) {
		candidates = append(candidates,
			fmt.Sprintf("SELECT %s FROM %s WHERE %s = '%s' ORDER BY %s DESC LIMIT 1;",
				selectCols, sessionTable, dirCol, esc, orderCol),
			fmt.Sprintf("SELECT %s FROM %s WHERE %s LIKE '%%%s%%' ORDER BY %s DESC LIMIT 1;",
				selectCols, sessionTable, dirCol, esc, orderCol),
		)
	}
	if projCol != "" && isSafeIdent(projCol) {
		candidates = append(candidates,
			fmt.Sprintf("SELECT %s FROM %s WHERE %s = '%s' ORDER BY %s DESC LIMIT 1;",
				selectCols, sessionTable, projCol, sqlEscape(projectID), orderCol))
	}
	// Last resort: most recent session regardless of project, bounded
	// afterward by the caller's recency check.
	candidates = append(candidates,
		fmt.Sprintf("SELECT %s FROM %s ORDER BY %s DESC LIMIT 1;", selectCols, sessionTable, orderCol))

	for _, q := range candidates {
		if id, ts, ok := trySessionQuery(dbPath, q); ok {
			return id, ts
		}
	}
	return "", ""
}

// fetchSessionMessages retrieves all messages for sessionID as a JSON array,
// trying the historically-known schema first and falling back to schema
// introspection. Returns nil if no messages could be retrieved.
func fetchSessionMessages(dbPath, sessionID string) []byte {
	esc := sqlEscape(sessionID)

	// Fast path: historically-known schema.
	if data := tryMessageQuery(dbPath, fmt.Sprintf(
		`SELECT json_group_array(json_patch(data, json_object('id', id))) FROM message WHERE session_id='%s' ORDER BY time_created;`,
		esc)); data != nil {
		return data
	}

	tables, err := sqliteTableNames(dbPath)
	if err != nil || len(tables) == 0 {
		return nil
	}
	messageTable := findBestTable(tables, "message", "part")
	if messageTable == "" {
		return nil
	}
	cols, err := sqliteTableColumns(dbPath, messageTable)
	if err != nil || len(cols) == 0 {
		return nil
	}

	sessionRefCol := findColumn(cols, []string{"session_id", "sessionid"}, []string{"session"})
	idCol := findColumn(cols, []string{"id"}, []string{"id"})
	if sessionRefCol == "" || idCol == "" || !isSafeIdent(sessionRefCol) || !isSafeIdent(idCol) {
		return nil
	}
	timeCol := findColumn(cols, []string{"time_created", "created_at"}, []string{"created", "time"})
	orderCol := idCol
	if timeCol != "" && isSafeIdent(timeCol) {
		orderCol = timeCol
	}

	// Try a single JSON blob column holding the full message object.
	dataCol := findColumn(cols, []string{"data", "content", "body"}, []string{"data", "content", "body"})
	if dataCol != "" && isSafeIdent(dataCol) {
		q := fmt.Sprintf(
			`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s = '%s' ORDER BY %s;`,
			dataCol, idCol, messageTable, sessionRefCol, esc, orderCol)
		if data := tryMessageQuery(dbPath, q); data != nil {
			return data
		}
	}

	// Fall back to composing a JSON object from individual columns.
	roleCol := findColumn(cols, []string{"role"}, []string{"role"})
	textCol := findColumn(cols, []string{"text", "content"}, []string{"text", "content", "message"})
	objParts := []string{fmt.Sprintf("'id', %s", idCol)}
	if roleCol != "" && isSafeIdent(roleCol) {
		objParts = append(objParts, fmt.Sprintf("'role', %s", roleCol))
	}
	if textCol != "" && isSafeIdent(textCol) {
		objParts = append(objParts, fmt.Sprintf("'content', %s", textCol))
	}
	q := fmt.Sprintf(`SELECT json_group_array(json_object(%s)) FROM %s WHERE %s = '%s' ORDER BY %s;`,
		strings.Join(objParts, ", "), messageTable, sessionRefCol, esc, orderCol)
	return tryMessageQuery(dbPath, q)
}

// parseFlexibleTime attempts to parse s as a timestamp using several known
// OpenCode formats, including Unix epoch seconds/milliseconds/microseconds
// as a plain integer.
func parseFlexibleTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, true
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		switch {
		case n > 1_000_000_000_000_000: // microseconds
			return time.UnixMicro(n), true
		case n > 1_000_000_000_000: // milliseconds
			return time.UnixMilli(n), true
		default: // seconds
			return time.Unix(n, 0), true
		}
	}
	return time.Time{}, false
}
