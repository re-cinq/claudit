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

// discoverFromSQLite queries the OpenCode SQLite database for the most recent session.
//
// OpenCode's on-disk SQLite schema is not a stable public contract and has
// changed across releases (table/column names have been renamed at least
// once already). Rather than hardcode a specific schema version, this
// introspects the actual tables and columns at query time so discovery
// keeps working as OpenCode's storage format evolves.
func discoverFromSQLite(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil
	}

	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, nil
	}

	sessionTable, err := findSQLiteTable(dbPath, "session", "sessions")
	if err != nil || sessionTable == nil {
		return nil, nil
	}

	idCol := pickColumn(sessionTable.Columns, "id")
	projectCol := pickColumn(sessionTable.Columns,
		"project_id", "projectid", "project", "directory", "cwd", "worktree", "path")
	timeCol := pickColumn(sessionTable.Columns,
		"time_updated", "updated_at", "updatedat",
		"time_created", "created_at", "createdat")
	if idCol == "" || projectCol == "" {
		return nil, nil
	}

	orderClause := ""
	if timeCol != "" {
		orderClause = " ORDER BY " + quoteIdent(timeCol) + " DESC"
	}
	sessionQuery := fmt.Sprintf(
		"SELECT * FROM %s WHERE %s = %s%s LIMIT 1;",
		quoteIdent(sessionTable.Name), quoteIdent(projectCol), sqlQuote(projectID), orderClause,
	)

	sessionRows, err := runSQLiteJSONQuery(dbPath, sessionQuery)
	if err != nil || len(sessionRows) == 0 {
		return nil, nil
	}
	row := sessionRows[0]

	sessionID := stringValue(row[idCol])
	if sessionID == "" {
		return nil, nil
	}

	// Check if this session was recent (within timeout). If we can't
	// determine recency, proceed anyway — better to try than skip.
	if timeCol != "" && !isRecentTimestamp(row[timeCol]) {
		return nil, nil
	}

	messageTable, err := findSQLiteTable(dbPath, "message", "messages")
	if err != nil || messageTable == nil {
		return nil, nil
	}

	sessionFKCol := pickColumn(messageTable.Columns, "session_id", "sessionid", "session")
	if sessionFKCol == "" {
		return nil, nil
	}
	msgOrderCol := pickColumn(messageTable.Columns,
		"time_created", "created_at", "createdat",
		"time_updated", "updated_at", "updatedat", "id")

	msgOrderClause := ""
	if msgOrderCol != "" {
		msgOrderClause = " ORDER BY " + quoteIdent(msgOrderCol)
	}
	msgQuery := fmt.Sprintf(
		"SELECT * FROM %s WHERE %s = %s%s;",
		quoteIdent(messageTable.Name), quoteIdent(sessionFKCol), sqlQuote(sessionID), msgOrderClause,
	)

	msgOut, err := exec.Command("sqlite3", "-json", dbPath, msgQuery).Output()
	if err != nil {
		return nil, nil
	}

	// sqlite3 -json prints nothing (not "[]") when a query matches zero rows.
	transcriptData := []byte(strings.TrimSpace(string(msgOut)))
	if len(transcriptData) == 0 {
		transcriptData = []byte("[]")
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: "", // no file path for SQLite
		StartedAt:      time.Now().Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: transcriptData,
	}, nil
}

// sqliteTableInfo describes an introspected SQLite table and its columns.
type sqliteTableInfo struct {
	Name    string
	Columns []string
}

// findSQLiteTable finds the first table in dbPath matching one of the given
// candidate names (case-insensitive, checked in priority order) and returns
// its name and column list. Returns (nil, nil) if no candidate matches.
func findSQLiteTable(dbPath string, candidates ...string) (*sqliteTableInfo, error) {
	out, err := exec.Command("sqlite3", "-json", dbPath,
		"SELECT name FROM sqlite_master WHERE type='table';").Output()
	if err != nil {
		return nil, err
	}

	var tables []struct {
		Name string `json:"name"`
	}
	if strings.TrimSpace(string(out)) != "" {
		if err := json.Unmarshal(out, &tables); err != nil {
			return nil, err
		}
	}

	var match string
	for _, cand := range candidates {
		for _, t := range tables {
			if strings.EqualFold(t.Name, cand) {
				match = t.Name
				break
			}
		}
		if match != "" {
			break
		}
	}
	if match == "" {
		return nil, nil
	}

	colOut, err := exec.Command("sqlite3", "-json", dbPath,
		fmt.Sprintf("PRAGMA table_info(%s);", quoteIdent(match))).Output()
	if err != nil {
		return nil, err
	}

	var cols []struct {
		Name string `json:"name"`
	}
	if strings.TrimSpace(string(colOut)) != "" {
		if err := json.Unmarshal(colOut, &cols); err != nil {
			return nil, err
		}
	}

	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}

	return &sqliteTableInfo{Name: match, Columns: names}, nil
}

// runSQLiteJSONQuery runs a query against dbPath and decodes the result as a
// slice of column-name-keyed rows.
func runSQLiteJSONQuery(dbPath, query string) ([]map[string]interface{}, error) {
	out, err := exec.Command("sqlite3", "-json", dbPath, query).Output()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil, nil
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// pickColumn returns the first column in cols matching one of the candidates
// (case-insensitive, checked in priority order), or "" if none match.
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

// quoteIdent quotes a SQLite identifier (table or column name).
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqlQuote quotes a SQLite string literal.
func sqlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// stringValue converts a JSON-decoded value to its string form.
func stringValue(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", val)
	}
}

// isRecentTimestamp reports whether a JSON-decoded timestamp value (an
// RFC3339-ish string, or a numeric Unix epoch in seconds or milliseconds)
// falls within RecentSessionTimeout. Values that can't be interpreted are
// treated as recent — better to try than to skip a real session.
func isRecentTimestamp(v interface{}) bool {
	switch val := v.(type) {
	case string:
		layouts := []string{
			time.RFC3339Nano,
			"2006-01-02T15:04:05.000Z",
			"2006-01-02 15:04:05",
		}
		for _, layout := range layouts {
			if t, err := time.Parse(layout, val); err == nil {
				return time.Since(t) <= agent.RecentSessionTimeout
			}
		}
		return true
	case float64:
		sec := int64(val)
		if sec > 1_000_000_000_000 {
			sec /= 1000 // milliseconds -> seconds
		}
		return time.Since(time.Unix(sec, 0)) <= agent.RecentSessionTimeout
	default:
		return true
	}
}
