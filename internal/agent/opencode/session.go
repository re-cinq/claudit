package opencode

import (
	"encoding/json"
	"fmt"
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

// --- SQLite schema introspection ---
//
// OpenCode has changed its SQLite table and column names across releases
// (e.g. "session"/"project_id"/"time_updated"). Rather than hardcoding one
// snapshot of the schema, these helpers discover it at query time so session
// discovery keeps working across OpenCode upgrades.

// sqliteTables returns all table names in the given SQLite database.
func sqliteTables(dbPath string) ([]string, error) {
	out, err := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';").Output()
	if err != nil {
		return nil, err
	}
	var tables []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			tables = append(tables, line)
		}
	}
	return tables, nil
}

// sqliteColumns returns the column names of the given table.
func sqliteColumns(dbPath, table string) ([]string, error) {
	out, err := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table)).Output()
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 2 {
			cols = append(cols, fields[1])
		}
	}
	return cols, nil
}

// sqliteFindTable locates the first table (in candidate priority order) that
// exists in the database, returning its exact name and column list. Returns
// ("", nil, nil) if none of the candidates are present.
func sqliteFindTable(dbPath string, candidates []string) (table string, columns []string, err error) {
	tables, err := sqliteTables(dbPath)
	if err != nil {
		return "", nil, err
	}
	for _, cand := range candidates {
		for _, t := range tables {
			if strings.EqualFold(t, cand) {
				cols, cerr := sqliteColumns(dbPath, t)
				if cerr != nil {
					return "", nil, cerr
				}
				return t, cols, nil
			}
		}
	}
	return "", nil, nil
}

// sqlitePickColumn returns the first column matching one of the candidate
// names, ignoring case and underscores (so "project_id" matches "projectID").
// Returns "" if none match.
func sqlitePickColumn(cols []string, candidates ...string) string {
	norm := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(s, "_", ""))
	}
	for _, cand := range candidates {
		want := norm(cand)
		for _, c := range cols {
			if norm(c) == want {
				return c
			}
		}
	}
	return ""
}

// sqliteQuoteLiteral escapes a string for use as a single-quoted SQLite literal.
func sqliteQuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// sqliteTimestampRecent reports whether a timestamp value (an ISO-8601
// string, or epoch seconds/milliseconds/microseconds as a numeric string)
// falls within RecentSessionTimeout. parsed is false if the value couldn't
// be interpreted as a timestamp at all.
func sqliteTimestampRecent(s string) (recent bool, parsed bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return false, false
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		var t time.Time
		switch {
		case n > 1_000_000_000_000_000: // microseconds
			t = time.UnixMicro(n)
		case n > 1_000_000_000_000: // milliseconds
			t = time.UnixMilli(n)
		case n > 0:
			t = time.Unix(n, 0)
		default:
			return false, false
		}
		return time.Since(t) <= agent.RecentSessionTimeout, true
	}

	formats := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout, true
		}
	}
	return false, false
}

// fetchMessagesFromSQLite queries an OpenCode SQLite database for all
// messages belonging to sessionID, adapting to the live schema. Returns the
// messages as a JSON array, or ok=false if no usable message table/columns
// were found or the session has no messages.
func fetchMessagesFromSQLite(dbPath, sessionID string) (data []byte, ok bool) {
	messageTable, messageCols, err := sqliteFindTable(dbPath, []string{"message", "messages"})
	if err != nil || messageTable == "" {
		return nil, false
	}

	idCol := sqlitePickColumn(messageCols, "id")
	sessionCol := sqlitePickColumn(messageCols, "session_id", "sessionID", "session")
	dataCol := sqlitePickColumn(messageCols, "data", "content", "body", "json")
	timeCol := sqlitePickColumn(messageCols, "time_created", "created_at", "createTime", "timeCreated")
	if idCol == "" || sessionCol == "" || dataCol == "" {
		return nil, false
	}

	orderBy := "rowid"
	if timeCol != "" {
		orderBy = timeCol
	}

	query := fmt.Sprintf(
		`SELECT json_group_array(json_patch(%s, json_object('id', %s))) FROM %s WHERE %s=%s ORDER BY %s;`,
		dataCol, idCol, messageTable, sessionCol, sqliteQuoteLiteral(sessionID), orderBy,
	)
	out, err := exec.Command("sqlite3", dbPath, query).Output()
	if err != nil {
		return nil, false
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "[null]" || trimmed == "[]" {
		return nil, false
	}

	return []byte(trimmed), true
}

// fetchTranscriptFromSQLite resolves the database path from an OpenCode data
// directory and fetches sessionID's messages from it. Used as a fallback
// when a session's flat-file message directory doesn't exist, since
// OpenCode v1.2+ may store sessions in SQLite instead of individual files.
func fetchTranscriptFromSQLite(dataDir, sessionID string) ([]byte, bool) {
	dbPath := filepath.Join(dataDir, "opencode.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, false
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, false
	}
	return fetchMessagesFromSQLite(dbPath, sessionID)
}
