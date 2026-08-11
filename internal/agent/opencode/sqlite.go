```go
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
// recent session belonging to projectPath/projectID.
//
// OpenCode's SQLite schema (table and column names) has changed across
// releases (e.g. between the 1.14.x and 1.18.x series), so instead of
// assuming fixed names this introspects the actual schema at query time via
// sqlite_master/PRAGMA table_info and picks the best-matching columns from a
// list of known historical/likely names. This keeps session discovery
// working across upstream schema renames without needing an exact version
// pin.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, sessionCols := findTable(dbPath, "session", "sessions")
	if sessionTable == "" {
		return nil, nil
	}
	idCol := pickColumn(sessionCols, "id", "session_id", "sessionid")
	if idCol == "" {
		return nil, nil
	}
	timeCol := pickColumn(sessionCols,
		"time_updated", "updated_at", "updatedat", "updated",
		"time_created", "created_at", "createdat", "mtime")

	sessionID, sessionTimeRaw := findSessionForProject(dbPath, sessionTable, idCol, timeCol, sessionCols, projectID, projectPath)
	if sessionID == "" {
		return nil, nil
	}

	if sessionTimeRaw != "" {
		if t, ok := parseSQLiteTime(sessionTimeRaw); ok {
			if time.Since(t) > agent.RecentSessionTimeout {
				return nil, nil
			}
		}
		// If we can't parse the time, proceed anyway — better to try than skip.
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

// findSessionForProject locates the most recent session ID for the given
// project. It tries an ID-based match first (e.g. project_id/projectID
// columns storing the git root commit hash), then a path-based match (e.g.
// directory/cwd/path columns storing the absolute project path), and
// finally falls back to the single most recently updated session overall if
// no project-identifying column can be found or matched. Returns the
// session ID and its raw time value (if a time column was found); both
// empty if nothing matched.
func findSessionForProject(dbPath, table, idCol, timeCol string, cols []string, projectID, projectPath string) (string, string) {
	orderBy := ""
	if timeCol != "" {
		orderBy = fmt.Sprintf(" ORDER BY %s DESC", quoteIdent(timeCol))
	}

	tryMatch := func(col, value string) (string, string) {
		if col == "" || value == "" {
			return "", ""
		}
		selectCols := quoteIdent(idCol)
		if timeCol != "" {
			selectCols = quoteIdent(idCol) + ", " + quoteIdent(timeCol)
		}
		query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s=%s%s LIMIT 1;`,
			selectCols, quoteIdent(table), quoteIdent(col), sqlQuote(value), orderBy)
		out, err := runSQLite(dbPath, "-separator", "\t", query)
		if err != nil {
			return "", ""
		}
		out = strings.TrimSpace(out)
		if out == "" {
			return "", ""
		}
		parts := strings.SplitN(out, "\t", 2)
		id := parts[0]
		t := ""
		if len(parts) > 1 {
			t = parts[1]
		}
		return id, t
	}

	if idCandidate := pickColumn(cols, "project_id", "projectid", "projectID"); idCandidate != "" {
		if id, t := tryMatch(idCandidate, projectID); id != "" {
			return id, t
		}
	}

	if pathCandidate := pickColumn(cols, "directory", "cwd", "path", "worktree", "root", "project_path", "projectpath"); pathCandidate != "" {
		if id, t := tryMatch(pathCandidate, projectPath); id != "" {
			return id, t
		}
		if resolved, err := filepath.EvalSymlinks(projectPath); err == nil && resolved != projectPath {
			if id, t := tryMatch(pathCandidate, resolved); id != "" {
				return id, t
			}
		}
	}

	// No project-identifying column found (or nothing matched it) — fall
	// back to the single most recently updated session. This keeps
	// single-project setups (the common case, including tests) working even
	// if the project-matching column was renamed to something unrecognized.
	if timeCol != "" {
		query := fmt.Sprintf(`SELECT %s, %s FROM %s ORDER BY %s DESC LIMIT 1;`,
			quoteIdent(idCol), quoteIdent(timeCol), quoteIdent(table), quoteIdent(timeCol))
		out, err := runSQLite(dbPath, "-separator", "\t", query)
		if err == nil {
			out = strings.TrimSpace(out)
			if out != "" {
				parts := strings.SplitN(out, "\t", 2)
				id := parts[0]
				t := ""
				if len(parts) > 1 {
					t = parts[1]
				}
				return id, t
			}
		}
	}

	return "", ""
}

// fetchSQLiteMessages returns the messages for a session as a JSON array,
// adapting to whatever message schema the installed OpenCode version uses.
func fetchSQLiteMessages(dbPath, sessionID string) ([]byte, error) {
	messageTable, messageCols := findTable(dbPath, "message", "messages")
	if messageTable == "" {
		return nil, fmt.Errorf("no message table found")
	}

	msgIDCol := pickColumn(messageCols, "id")
	sessionIDCol := pickColumn(messageCols, "session_id", "sessionid", "sessionID")
	if msgIDCol == "" || sessionIDCol == "" {
		return nil, fmt.Errorf("message table missing id/session_id columns")
	}
	timeCol := pickColumn(messageCols, "time_created", "created_at", "createdat", "time", "created")
	dataCol := pickColumn(messageCols, "data", "payload", "body", "json")

	var selectExpr string
	if dataCol != "" {
		// Known-good shape: a single JSON blob column holding the full
		// message. Merge the row id into it (matching prior behavior).
		selectExpr = fmt.Sprintf("json_patch(m.%s, json_object('id', m.%s))",
			quoteIdent(dataCol), quoteIdent(msgIDCol))
	} else {
		fields := []string{fmt.Sprintf("'id', m.%s", quoteIdent(msgIDCol))}
		if roleCol := pickColumn(messageCols, "role"); roleCol != "" {
			fields = append(fields, fmt.Sprintf("'role', m.%s", quoteIdent(roleCol)))
		}
		if typeCol := pickColumn(messageCols, "type"); typeCol != "" {
			fields = append(fields, fmt.Sprintf("'type', m.%s", quoteIdent(typeCol)))
		}
		if timeCol != "" {
			fields = append(fields, fmt.Sprintf("'time', m.%s", quoteIdent(timeCol)))
		}
		if contentExpr := partsContentExpr(dbPath, msgIDCol); contentExpr != "" {
			fields = append(fields, fmt.Sprintf("'content', %s", contentExpr))
		}
		selectExpr = fmt.Sprintf("json_object(%s)", strings.Join(fields, ", "))
	}

	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY m.%s", quoteIdent(timeCol))
	}

	query := fmt.Sprintf(`SELECT json_group_array(%s) FROM %s m WHERE m.%s=%s%s;`,
		selectExpr, quoteIdent(messageTable), quoteIdent(sessionIDCol), sqlQuote(sessionID), orderClause)

	out, err := runSQLite(dbPath, query)
	if err != nil {
		return nil, err
	}
	data := strings.TrimSpace(out)
	if data == "" || data == "[null]" || data == "[]" {
		return nil, nil
	}
	return []byte(data), nil
}

// partsContentExpr builds a SQL expression that aggregates a message's
// content parts into a JSON array of {"type", "text"} blocks, for schemas
// where message content lives in a separate part/parts table rather than
// inline on the message row. Returns "" if no such table can be found.
func partsContentExpr(dbPath, msgIDCol string) string {
	partTable, partCols := findTable(dbPath, "part", "parts", "message_part")
	if partTable == "" {
		return ""
	}
	partMsgIDCol := pickColumn(partCols, "message_id", "messageid", "messageID")
	partTextCol := pickColumn(partCols, "text", "content", "value", "body")
	if partMsgIDCol == "" || partTextCol == "" {
		return ""
	}
	typeExpr := "'text'"
	if partTypeCol := pickColumn(partCols, "type"); partTypeCol != "" {
		typeExpr = fmt.Sprintf("COALESCE(pt.%s, 'text')", quoteIdent(partTypeCol))
	}
	return fmt.Sprintf(
		"(SELECT json_group_array(json_object('type', %s, 'text', pt.%s)) FROM %s pt WHERE pt.%s = m.%s)",
		typeExpr, quoteIdent(partTextCol), quoteIdent(partTable), quoteIdent(partMsgIDCol), quoteIdent(msgIDCol),
	)
}

// findTable returns the first candidate table name that exists in the
// database (case-insensitive), along with its column names. Returns
// ("", nil) if none of the candidates exist.
func findTable(dbPath string, candidates ...string) (string, []string) {
	out, err := runSQLite(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil {
		return "", nil
	}
	existing := strings.Split(strings.TrimSpace(out), "\n")
	for _, want := range candidates {
		for _, have := range existing {
			have = strings.TrimSpace(have)
			if strings.EqualFold(have, want) {
				return have, tableColumns(dbPath, have)
			}
		}
	}
	return "", nil
}

// tableColumns returns the column names for a table via PRAGMA table_info.
func tableColumns(dbPath, table string) []string {
	out, err := runSQLite(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(table)))
	if err != nil {
		return nil
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 2 {
			continue
		}
		cols = append(cols, fields[1])
	}
	return cols
}

// pickColumn returns the first candidate present in cols (case-insensitive
// match), or "" if none are present.
func pickColumn(cols []string, candidates ...string) string {
	for _, want := range candidates {
		for _, have := range cols {
			if strings.EqualFold(have, want) {
				return have
			}
		}
	}
	return ""
}

// quoteIdent quotes a SQLite identifier (table/column name) for safe
// inclusion in a query.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqlQuote quotes a SQLite string literal.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// runSQLite runs the sqlite3 CLI against dbPath with the given arguments,
// returning stdout.
func runSQLite(dbPath string, args ...string) (string, error) {
	fullArgs := append([]string{dbPath}, args...)
	cmd := exec.Command("sqlite3", fullArgs...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseSQLiteTime parses a time value from SQLite in any of the formats
// OpenCode has used: RFC3339 variants, a space-separated datetime, or an
// epoch timestamp in seconds, milliseconds, or microseconds.
func parseSQLiteTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, raw); err == nil {
			return t, true
		}
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		switch {
		case n > 1e15: // microseconds
			return time.UnixMicro(n), true
		case n > 1e12: // milliseconds
			return time.UnixMilli(n), true
		default: // seconds
			return time.Unix(n, 0), true
		}
	}
	return time.Time{}, false
}
```
