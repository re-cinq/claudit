package opencode

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

// findOpenCodeDB locates the OpenCode SQLite database. Beyond the well-known
// "opencode.db" path, this falls back to a bounded scan of the data
// directory since newer OpenCode releases have relocated storage files.
func findOpenCodeDB(dataDir string) (string, error) {
	primary := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(primary); err == nil {
		return primary, nil
	}

	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		return "", nil
	}

	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() {
			if path == dataDir {
				return nil
			}
			rel, relErr := filepath.Rel(dataDir, path)
			if relErr == nil && strings.Count(rel, string(filepath.Separator)) >= 3 {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".db") {
			found = path
			return filepath.SkipAll
		}
		return nil
	})

	return found, nil
}

// sqlQuote safely quotes a string literal for inline use in a SQLite statement.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// runSQLite executes a query against the OpenCode SQLite database and returns
// trimmed stdout. When tabSeparated is true, columns are separated by tabs.
func runSQLite(dbPath, query string, tabSeparated bool) (string, error) {
	var args []string
	if tabSeparated {
		args = append(args, "-separator", "\t")
	}
	args = append(args, dbPath, query)

	cmd := exec.Command("sqlite3", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// listTables returns all table names in the SQLite database.
func listTables(dbPath string) ([]string, error) {
	out, err := runSQLite(dbPath, `SELECT name FROM sqlite_master WHERE type='table';`, false)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// findTableByKeyword finds a table whose name matches (or contains) the given
// keyword, tolerating renamed/pluralized tables across OpenCode versions.
func findTableByKeyword(dbPath, keyword string) (string, error) {
	tables, err := listTables(dbPath)
	if err != nil {
		return "", err
	}

	kw := strings.ToLower(keyword)
	for _, t := range tables {
		lt := strings.ToLower(t)
		if lt == kw || lt == kw+"s" {
			return t, nil
		}
	}
	for _, t := range tables {
		if strings.Contains(strings.ToLower(t), kw) {
			return t, nil
		}
	}
	return "", nil
}

// tableColumns returns the column names of a SQLite table.
func tableColumns(dbPath, table string) ([]string, error) {
	out, err := runSQLite(dbPath, fmt.Sprintf(`PRAGMA table_info("%s");`, table), true)
	if err != nil || out == "" {
		return nil, err
	}

	var cols []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// pickColumn returns the first column matching exact (case-insensitive), or
// failing that, the first column whose name contains one of the substrings
// (checked in priority order across all columns).
func pickColumn(cols []string, exact string, contains ...string) string {
	if exact != "" {
		for _, c := range cols {
			if strings.EqualFold(c, exact) {
				return c
			}
		}
	}
	for _, substr := range contains {
		if substr == "" {
			continue
		}
		for _, c := range cols {
			if strings.Contains(strings.ToLower(c), substr) {
				return c
			}
		}
	}
	return ""
}

// withinRecentTimeout reports whether a stored timestamp value (which may be
// an RFC3339-ish string or a Unix epoch in seconds/ms/us/ns) is within
// agent.RecentSessionTimeout. Unparseable or empty values are treated as
// recent rather than blocking discovery on a format we don't recognize.
func withinRecentTimeout(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		var t time.Time
		switch {
		case n > 1_000_000_000_000_000_000:
			t = time.Unix(0, n) // nanoseconds
		case n > 1_000_000_000_000_000:
			t = time.Unix(0, n*1e3) // microseconds
		case n > 1_000_000_000_000:
			t = time.Unix(0, n*1e6) // milliseconds
		default:
			t = time.Unix(n, 0) // seconds
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	return true
}

// findRecentSession locates the most recently updated session ID and its raw
// timestamp value. It first tries scoping to the current project, then falls
// back to the most recent session overall — OpenCode's project scoping key
// (column name and value format) has changed across versions, so an exact
// match may legitimately miss a session that is otherwise a good candidate.
func findRecentSession(dbPath, table, idCol, projectCol, timeCol, projectID, projectPath string) (id, updatedAt string) {
	var orderBy string
	if timeCol != "" {
		orderBy = fmt.Sprintf(` ORDER BY "%s" DESC`, timeCol)
	}

	selectCols := fmt.Sprintf(`"%s"`, idCol)
	if timeCol != "" {
		selectCols += fmt.Sprintf(`, "%s"`, timeCol)
	}

	if projectCol != "" {
		q := fmt.Sprintf(`SELECT %s FROM "%s" WHERE "%s" IN (%s, %s)%s LIMIT 1;`,
			selectCols, table, projectCol, sqlQuote(projectID), sqlQuote(projectPath), orderBy)
		if out, err := runSQLite(dbPath, q, true); err == nil && out != "" {
			return splitFirstTwo(out)
		}
	}

	q := fmt.Sprintf(`SELECT %s FROM "%s"%s LIMIT 1;`, selectCols, table, orderBy)
	if out, err := runSQLite(dbPath, q, true); err == nil && out != "" {
		return splitFirstTwo(out)
	}

	return "", ""
}

// splitFirstTwo splits a single tab-separated result line into its first two
// fields, treating a missing second field as an empty string.
func splitFirstTwo(line string) (string, string) {
	lines := strings.Split(strings.TrimSpace(line), "\n")
	fields := strings.Split(lines[0], "\t")
	first := fields[0]
	second := ""
	if len(fields) > 1 {
		second = fields[1]
	}
	return first, second
}

// fetchMessages retrieves all messages for a session as a JSON array,
// merging each message's id into its stored data payload.
func fetchMessages(dbPath, table, idCol, sessionRefCol, dataCol, timeCol, sessionID string) ([]byte, error) {
	var orderBy string
	if timeCol != "" {
		orderBy = fmt.Sprintf(` ORDER BY "%s"`, timeCol)
	}

	q := fmt.Sprintf(
		`SELECT json_group_array(json_patch("%s", json_object('id', "%s"))) FROM "%s" WHERE "%s"=%s%s;`,
		dataCol, idCol, table, sessionRefCol, sqlQuote(sessionID), orderBy,
	)
	out, err := runSQLite(dbPath, q, false)
	if err != nil {
		return nil, err
	}
	if out == "" || out == "[null]" || out == "[]" {
		return nil, nil
	}
	return []byte(out), nil
}
