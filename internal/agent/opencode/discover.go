```go
package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
)

// discoverFromFlatFilesRecursive scans the OpenCode session storage tree for the
// most recently modified session file matching the current project, regardless
// of how OpenCode nests its session files on disk. OpenCode has repeatedly
// changed this layout across releases (per-project subdirectories, a flat
// "info" directory, etc.), so this walks the whole tree instead of assuming
// one fixed shape.
func discoverFromFlatFilesRecursive(projectPath, projectID string) (*agent.SessionInfo, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return nil, nil
	}

	root := filepath.Join(dataDir, "storage", "session")
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, nil
	}

	now := time.Now()
	recentTimeout := agent.RecentSessionTimeout
	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return nil
		}
		modTime := fi.ModTime()
		if now.Sub(modTime) > recentTimeout {
			return nil
		}
		if bestSessionID != "" && !modTime.After(bestModTime) {
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

		dir := extractDirectoryField(raw)
		pid := extractProjectIDField(raw)
		if !(dir != "" && agent.PathsEqual(dir, projectPath)) && !(pid != "" && pid == projectID) {
			return nil
		}

		id := extractIDField(raw)
		if id == "" {
			id = strings.TrimSuffix(d.Name(), ".json")
		}

		bestSessionID = id
		bestModTime = modTime
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: findMessageDir(dataDir, bestSessionID),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// findMessageDir returns the first existing candidate directory that holds a
// session's messages, tolerating layout changes across OpenCode versions.
func findMessageDir(dataDir, sessionID string) string {
	candidates := []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", sessionID, "message"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return candidates[0]
}

func extractDirectoryField(raw map[string]json.RawMessage) string {
	for _, key := range []string{"directory", "cwd", "path", "worktree", "projectPath", "root", "workingDirectory"} {
		if v, ok := raw[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				return s
			}
		}
	}
	return ""
}

func extractProjectIDField(raw map[string]json.RawMessage) string {
	for _, key := range []string{"projectID", "project_id", "projectId", "project"} {
		if v, ok := raw[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				return s
			}
		}
	}
	return ""
}

func extractIDField(raw map[string]json.RawMessage) string {
	for _, key := range []string{"id", "sessionID", "session_id", "sessionId"} {
		if v, ok := raw[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				return s
			}
		}
	}
	return ""
}

// discoverFromSQLite queries the OpenCode SQLite database for the most recent
// session, discovering the actual table and column names at runtime instead of
// assuming a fixed schema. OpenCode's SQLite schema is not a stable public
// contract and column naming (snake_case vs camelCase) has changed between
// releases, which previously caused hardcoded column names to silently match
// nothing.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	tables, err := sqliteTables(dbPath)
	if err != nil || len(tables) == 0 {
		return nil, nil
	}

	sessionTable := findTableByHint(tables, "session")
	messageTable := findTableByHint(tables, "message")
	if sessionTable == "" || messageTable == "" {
		return nil, nil
	}

	sessionCols, err := sqliteColumns(dbPath, sessionTable)
	if err != nil || len(sessionCols) == 0 {
		return nil, nil
	}

	idCol := pickColumn(sessionCols, "id")
	if idCol == "" {
		return nil, nil
	}
	projectCol := pickColumn(sessionCols, "project_id", "projectid", "project")
	dirCol := pickColumn(sessionCols, "directory", "cwd", "path", "dir", "worktree")
	timeCol := pickColumn(sessionCols, "time_updated", "timeupdated", "updated", "updated_at", "mtime", "time_created", "timecreated")

	sessionID := findSessionID(dbPath, sessionTable, idCol, timeCol, projectCol, projectID, dirCol, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	if timeCol != "" {
		q := fmt.Sprintf(`SELECT "%s" FROM "%s" WHERE "%s"='%s';`, timeCol, sessionTable, idCol, escapeSQLiteString(sessionID))
		if out, err := runSQLite(dbPath, q); err == nil {
			if !isRecentTimeValue(strings.TrimSpace(string(out)), agent.RecentSessionTimeout) {
				return nil, nil
			}
		}
	}

	transcriptData := queryMessagesJSON(dbPath, messageTable, sessionID)
	if transcriptData == nil {
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

// findSessionID looks up the most relevant session row, preferring a match on
// the project column, then the directory column, then falling back to the
// most recently updated session overall.
func findSessionID(dbPath, table, idCol, timeCol, projectCol, projectID, dirCol, projectPath string) string {
	orderBy := ""
	if timeCol != "" {
		orderBy = fmt.Sprintf(` ORDER BY "%s" DESC`, timeCol)
	}

	query := func(where string) string {
		q := fmt.Sprintf(`SELECT "%s" FROM "%s"`, idCol, table)
		if where != "" {
			q += " WHERE " + where
		}
		q += orderBy + " LIMIT 1;"
		out, err := runSQLite(dbPath, q)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	if projectCol != "" && projectID != "" {
		if id := query(fmt.Sprintf(`"%s"='%s'`, projectCol, escapeSQLiteString(projectID))); id != "" {
			return id
		}
	}
	if dirCol != "" && projectPath != "" {
		if id := query(fmt.Sprintf(`"%s"='%s'`, dirCol, escapeSQLiteString(projectPath))); id != "" {
			return id
		}
	}
	return query("")
}

// queryMessagesJSON builds a JSON array of messages for the given session,
// adapting to whichever columns the message table actually has.
func queryMessagesJSON(dbPath, table, sessionID string) []byte {
	cols, err := sqliteColumns(dbPath, table)
	if err != nil || len(cols) == 0 {
		return nil
	}

	sessionCol := pickColumn(cols, "session_id", "sessionid", "session")
	if sessionCol == "" {
		return nil
	}
	idCol := pickColumn(cols, "id", "message_id", "messageid")
	timeCol := pickColumn(cols, "time_created", "timecreated", "created", "created_at")
	dataCol := pickColumn(cols, "data", "json", "payload", "body")

	orderBy := ""
	switch {
	case timeCol != "":
		orderBy = fmt.Sprintf(` ORDER BY "%s"`, timeCol)
	case idCol != "":
		orderBy = fmt.Sprintf(` ORDER BY "%s"`, idCol)
	}

	var selectExpr string
	if dataCol != "" {
		if idCol != "" && idCol != dataCol {
			selectExpr = fmt.Sprintf(`json_patch("%s", json_object('id', "%s"))`, dataCol, idCol)
		} else {
			selectExpr = fmt.Sprintf(`"%s"`, dataCol)
		}
	} else {
		roleCol := pickColumn(cols, "role", "type")
		contentCol := pickColumn(cols, "content", "text", "body")

		var parts []string
		if idCol != "" {
			parts = append(parts, fmt.Sprintf(`'id', "%s"`, idCol))
		}
		if roleCol != "" {
			parts = append(parts, fmt.Sprintf(`'role', "%s"`, roleCol))
		}
		if contentCol != "" {
			parts = append(parts, fmt.Sprintf(`'content', "%s"`, contentCol))
		}
		if timeCol != "" {
			parts = append(parts, fmt.Sprintf(`'time', json_object('created', "%s")`, timeCol))
		}
		if len(parts) == 0 {
			return nil
		}
		selectExpr = "json_object(" + strings.Join(parts, ", ") + ")"
	}

	q := fmt.Sprintf(`SELECT json_group_array(%s) FROM "%s" WHERE "%s"='%s'%s;`,
		selectExpr, table, sessionCol, escapeSQLiteString(sessionID), orderBy)

	out, err := runSQLite(dbPath, q)
	if err != nil {
		return nil
	}
	data := bytes.TrimSpace(out)
	if len(data) == 0 || string(data) == "[null]" || string(data) == "[]" {
		return nil
	}
	return data
}

func runSQLite(dbPath, query string) ([]byte, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	return cmd.Output()
}

func sqliteTables(dbPath string) ([]string, error) {
	out, err := runSQLite(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return nil, err
	}
	var tables []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			tables = append(tables, line)
		}
	}
	return tables, nil
}

func sqliteColumns(dbPath, table string) ([]string, error) {
	out, err := runSQLite(dbPath, fmt.Sprintf(`PRAGMA table_info("%s");`, table))
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

func findTableByHint(tables []string, hint string) string {
	for _, t := range tables {
		if strings.Contains(strings.ToLower(t), hint) {
			return t
		}
	}
	return ""
}

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

func escapeSQLiteString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// isRecentTimeValue reports whether raw (an epoch number in seconds,
// milliseconds or microseconds, or a common timestamp string format)
// represents a time within timeout of now. Unparseable or empty values are
// treated as recent, since it's better to try storing a session than to skip
// one just because its timestamp format wasn't recognized.
func isRecentTimeValue(raw string, timeout time.Duration) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}

	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		var t time.Time
		switch {
		case n > 1_000_000_000_000_000: // microseconds
			t = time.UnixMicro(n)
		case n > 1_000_000_000_000: // milliseconds
			t = time.UnixMilli(n)
		case n > 0:
			t = time.Unix(n, 0)
		}
		if !t.IsZero() {
			return time.Since(t) <= timeout
		}
	}

	formats := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
	for _, f := range formats {
		if t, err := time.Parse(f, raw); err == nil {
			return time.Since(t) <= timeout
		}
	}

	return true
}
```
