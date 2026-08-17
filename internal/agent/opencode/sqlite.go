package opencode

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// sqliteRow is a single row returned by the sqlite3 CLI in -json mode.
type sqliteRow map[string]interface{}

// runSQLiteJSON runs a query against the given database using the sqlite3
// CLI's -json output mode and decodes the result into a slice of rows.
// It returns a nil slice (not an error) when the query matches no rows,
// since the CLI prints nothing in that case.
func runSQLiteJSON(dbPath, query string) ([]sqliteRow, error) {
	cmd := exec.Command("sqlite3", "-json", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, nil
	}

	var rows []sqliteRow
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// quoteSQLIdent quotes a SQL identifier discovered from the database itself
// (table/column names), guarding against identifiers containing quotes.
func quoteSQLIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteSQLLiteral escapes a string for use as a single-quoted SQL literal.
func quoteSQLLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// findTable finds a table whose name matches keyword (case-insensitive,
// substring match, e.g. "session" matches "session" or "sessions") and
// returns its name along with its column names, in declaration order.
// OpenCode's on-disk schema has changed across releases (table/column
// renames), so callers should not assume exact names and instead pick
// columns by pattern via pickColumn.
func findTable(dbPath, keyword string) (table string, columns []string, err error) {
	rows, err := runSQLiteJSON(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil || len(rows) == 0 {
		return "", nil, err
	}

	keyword = strings.ToLower(keyword)
	var best string
	for _, row := range rows {
		name, _ := row["name"].(string)
		if name == "" || strings.HasPrefix(name, "sqlite_") {
			continue
		}
		lower := strings.ToLower(name)
		if lower == keyword || lower == keyword+"s" {
			best = name
			break
		}
		if best == "" && strings.Contains(lower, keyword) {
			best = name
		}
	}
	if best == "" {
		return "", nil, nil
	}

	colRows, err := runSQLiteJSON(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", quoteSQLIdent(best)))
	if err != nil {
		return "", nil, err
	}
	for _, row := range colRows {
		if name, ok := row["name"].(string); ok && name != "" {
			columns = append(columns, name)
		}
	}
	return best, columns, nil
}

// pickColumn returns the first column matching any of the given
// case-insensitive regexp patterns, tried in priority order.
func pickColumn(columns []string, patterns ...string) string {
	for _, pattern := range patterns {
		re := regexp.MustCompile("(?i)" + pattern)
		for _, col := range columns {
			if re.MatchString(col) {
				return col
			}
		}
	}
	return ""
}

// parseFlexibleTime parses a timestamp value that may be stored as an ISO
// string, a unix seconds/milliseconds/microseconds number, or a numeric
// string.
func parseFlexibleTime(v interface{}) (time.Time, bool) {
	switch val := v.(type) {
	case float64:
		return unixNumberToTime(val), true
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return time.Time{}, false
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return unixNumberToTime(n), true
		}
		layouts := []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05.000Z",
			"2006-01-02 15:04:05",
		}
		for _, layout := range layouts {
			if t, err := time.Parse(layout, s); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

func unixNumberToTime(n float64) time.Time {
	switch {
	case n > 1e14: // microseconds
		return time.UnixMicro(int64(n))
	case n > 1e11: // milliseconds
		return time.UnixMilli(int64(n))
	case n > 1e5: // seconds
		return time.Unix(int64(n), 0)
	default:
		return time.Time{}
	}
}
