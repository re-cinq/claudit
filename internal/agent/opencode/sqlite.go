package opencode

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
)

// sqliteTableColumns returns the column names of a SQLite table using PRAGMA
// table_info. Returns nil if the table doesn't exist or sqlite3 fails.
func sqliteTableColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
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

// pickColumn returns the first candidate present in cols (case-insensitive),
// or "" if none of the candidates are present. Used to tolerate OpenCode
// renaming its SQLite schema columns across releases.
func pickColumn(cols []string, candidates ...string) string {
	lower := make(map[string]string, len(cols))
	for _, c := range cols {
		lower[strings.ToLower(c)] = c
	}
	for _, cand := range candidates {
		if actual, ok := lower[strings.ToLower(cand)]; ok {
			return actual
		}
	}
	return ""
}

// escapeSQLiteLiteral escapes single quotes for use in a SQLite string literal.
func escapeSQLiteLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// isRecentTimestamp reports whether raw (a formatted date, or an epoch
// seconds/milliseconds integer) is within the recent-session timeout window.
// Unparseable or empty values are treated as recent so a timestamp format
// shift alone doesn't block session discovery.
func isRecentTimestamp(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}

	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		var t time.Time
		if n > 1_000_000_000_000 {
			t = time.UnixMilli(n)
		} else {
			t = time.Unix(n, 0)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, raw); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	return true
}
