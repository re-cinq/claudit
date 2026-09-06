package opencode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
)

// discoverFromSQLite queries the OpenCode SQLite database for the most
// recent session for a project. Table and column names are discovered
// dynamically via sqlite_master/PRAGMA table_info rather than hardcoded,
// since OpenCode's schema (table/column naming) has changed across versions.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, _ := findSQLiteTable(dbPath, "session")
	if sessionTable == "" {
		return nil, nil
	}
	messageTable, _ := findSQLiteTable(dbPath, "message")
	if messageTable == "" {
		return nil, nil
	}

	sessionCols, _ := sqliteColumns(dbPath, sessionTable)
	if len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := firstMatchingColumn(sessionCols, "id")
	if idCol == "" {
		idCol = "id"
	}
	projectCol := firstMatchingColumn(sessionCols, "project")
	timeCol := firstMatchingColumn(sessionCols, "updated", "update", "modified")
	if timeCol == "" {
		timeCol = firstMatchingColumn(sessionCols, "time", "created")
	}

	sessionID, sessionTime := findSQLiteSession(dbPath, sessionTable, idCol, projectCol, timeCol, projectID)
	if sessionID == "" {
		return nil, nil
	}
	if sessionTime != "" && !withinRecentTimeout(sessionTime) {
		return nil, nil
	}

	messageCols, _ := sqliteColumns(dbPath, messageTable)
	if len(messageCols) == 0 {
		return nil, nil
	}

	transcriptData := fetchSQLiteMessages(dbPath, messageTable, messageCols, sessionID)
	if len(transcriptData) == 0 {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "",
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// sqliteQuery runs a query against dbPath using the sqlite3 CLI in default
// (pipe-separated) list mode and returns trimmed stdout.
func sqliteQuery(dbPath, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// findSQLiteTable finds a table whose name matches keyword (exact, plural,
// or substring), to tolerate table renames across OpenCode versions.
func findSQLiteTable(dbPath, keyword string) (string, error) {
	out, err := sqliteQuery(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return "", err
	}

	var contains string
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if lower == keyword || lower == keyword+"s" {
			return name, nil
		}
		if contains == "" && strings.Contains(lower, keyword) {
			contains = name
		}
	}
	return contains, nil
}

// sqliteColumns returns the column names of table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) ([]string, error) {
	out, err := sqliteQuery(dbPath, fmt.Sprintf(`PRAGMA table_info("%s");`, table))
	if err != nil {
		return nil, err
	}

	var cols []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// firstMatchingColumn returns the first column (in table order) whose name
// contains one of keywords, checked in keyword priority order.
func firstMatchingColumn(cols []string, keywords ...string) string {
	for _, kw := range keywords {
		for _, c := range cols {
			if strings.Contains(strings.ToLower(c), kw) {
				return c
			}
		}
	}
	return ""
}

// findSQLiteSession returns the id (and, if available, the update-time
// value) of the best matching session: the most recent session for
// projectID if a project column was found and matches any rows, otherwise
// the most recently active session in the database overall.
func findSQLiteSession(dbPath, table, idCol, projectCol, timeCol, projectID string) (id string, updatedAt string) {
	orderCol := idCol
	if timeCol != "" {
		orderCol = timeCol
	}
	timeSelect := ""
	if timeCol != "" {
		timeSelect = fmt.Sprintf(`, "%s"`, timeCol)
	}

	if projectCol != "" {
		q := fmt.Sprintf(`SELECT "%s"%s FROM "%s" WHERE "%s"='%s' ORDER BY "%s" DESC LIMIT 1;`,
			idCol, timeSelect, table, projectCol, escapeSQLiteLiteral(projectID), orderCol)
		if out, err := sqliteQuery(dbPath, q); err == nil && out != "" {
			return splitIDAndTime(out)
		}
	}

	// No project column, or no rows for this project id: fall back to the
	// most recently active session in the database.
	q := fmt.Sprintf(`SELECT "%s"%s FROM "%s" ORDER BY "%s" DESC LIMIT 1;`,
		idCol, timeSelect, table, orderCol)
	out, err := sqliteQuery(dbPath, q)
	if err != nil || out == "" {
		return "", ""
	}
	return splitIDAndTime(out)
}

func splitIDAndTime(out string) (string, string) {
	parts := strings.SplitN(out, "|", 2)
	id := strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		return id, strings.TrimSpace(parts[1])
	}
	return id, ""
}

func escapeSQLiteLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// withinRecentTimeout reports whether timeStr, in any of several formats
// OpenCode has used for timestamps (RFC3339 variants, plain datetime, or a
// unix epoch in seconds/milliseconds), is within the recent session window.
// Unrecognized formats are treated as recent, since it's safer to consider a
// session than to silently drop it over a parsing mismatch.
func withinRecentTimeout(timeStr string) bool {
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, timeStr); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
		var t time.Time
		if n > 1e12 {
			t = time.UnixMilli(n)
		} else {
			t = time.Unix(n, 0)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	return true
}

// fetchSQLiteMessages returns a JSON array of message rows for sessionID,
// ordered by creation. Prefers a single JSON blob column (as used by earlier
// OpenCode schemas); if none is found, composes one JSON object per row from
// whatever columns the message table actually has.
func fetchSQLiteMessages(dbPath, table string, cols []string, sessionID string) []byte {
	idCol := firstMatchingColumn(cols, "id")
	if idCol == "" {
		idCol = "id"
	}
	sessionCol := firstMatchingColumn(cols, "session")
	if sessionCol == "" {
		return nil
	}
	orderCol := firstMatchingColumn(cols, "created", "time")
	if orderCol == "" {
		orderCol = idCol
	}

	var q string
	if dataCol := firstMatchingColumn(cols, "data", "payload", "body", "json"); dataCol != "" {
		q = fmt.Sprintf(
			`SELECT json_group_array(json_patch("%s", json_object('id', "%s"))) FROM "%s" WHERE "%s"='%s' ORDER BY "%s";`,
			dataCol, idCol, table, sessionCol, escapeSQLiteLiteral(sessionID), orderCol,
		)
	} else {
		fields := make([]string, 0, len(cols))
		for _, c := range cols {
			fields = append(fields, fmt.Sprintf(`'%s', "%s"`, c, c))
		}
		q = fmt.Sprintf(
			`SELECT json_group_array(json_object(%s)) FROM "%s" WHERE "%s"='%s' ORDER BY "%s";`,
			strings.Join(fields, ", "), table, sessionCol, escapeSQLiteLiteral(sessionID), orderCol,
		)
	}

	out, err := sqliteQuery(dbPath, q)
	if err != nil {
		return nil
	}
	if out == "" || out == "[null]" || out == "[]" {
		return nil
	}
	return []byte(out)
}
