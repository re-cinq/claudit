```go
package opencode

import (
	"fmt"
	"os/exec"
	"strings"
)

// sqliteTables returns the names of user tables in the given SQLite database.
func sqliteTables(dbPath string) []string {
	cmd := exec.Command("sqlite3", dbPath, ".tables")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// findTableLike returns a table name matching substr: an exact
// (case-insensitive) match is preferred, otherwise the first table whose
// name contains substr. Returns "" if nothing matches.
func findTableLike(dbPath, substr string) string {
	tables := sqliteTables(dbPath)

	var fallback string
	for _, t := range tables {
		if strings.EqualFold(t, substr) {
			return t
		}
		if fallback == "" && strings.Contains(strings.ToLower(t), strings.ToLower(substr)) {
			fallback = t
		}
	}
	return fallback
}

// sqliteColumns returns the column names of a table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var columns []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			columns = append(columns, fields[1])
		}
	}
	return columns
}

// pickColumn returns the first candidate present in columns
// (case-insensitive), or "" if none match.
func pickColumn(columns []string, candidates []string) string {
	for _, candidate := range candidates {
		for _, col := range columns {
			if strings.EqualFold(col, candidate) {
				return col
			}
		}
	}
	return ""
}

// escapeSQLLiteral escapes single quotes for safe inclusion in a SQL string literal.
func escapeSQLLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// sqliteSessionTranscript fetches a session's messages from SQLite as a JSON
// array, adapting to whichever message table/column names are present so it
// degrades gracefully instead of failing outright when OpenCode renames them.
func sqliteSessionTranscript(dbPath, sessionID string) []byte {
	empty := []byte("[]")

	msgTable := findTableLike(dbPath, "message")
	if msgTable == "" {
		msgTable = "message"
	}

	columns := sqliteColumns(dbPath, msgTable)
	idCol := pickColumn(columns, []string{"id"})
	sessionCol := pickColumn(columns, []string{"session_id", "sessionid", "session"})
	dataCol := pickColumn(columns, []string{"data", "content", "body", "json"})
	timeCol := pickColumn(columns, []string{"time_created", "created_at", "created", "time_updated"})

	if idCol == "" || sessionCol == "" || dataCol == "" {
		return empty
	}

	orderClause := ""
	if timeCol != "" {
		orderClause = fmt.Sprintf(" ORDER BY %s", timeCol)
	}

	query := fmt.Sprintf(
		`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s='%s'%s;`,
		dataCol, idCol, msgTable, sessionCol, escapeSQLLiteral(sessionID), orderClause,
	)
	cmd := exec.Command("sqlite3", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return empty
	}

	transcriptData := []byte(strings.TrimSpace(string(out)))
	if len(transcriptData) == 0 || string(transcriptData) == "[null]" {
		return empty
	}

	return transcriptData
}
```
