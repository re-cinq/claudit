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

// candidateSessionDirs returns the possible on-disk locations for a
// project's session storage directory, across OpenCode's known storage
// layouts. Newer OpenCode releases nest project storage under
// "project/<id>/storage/..." instead of "storage/session/<id>", so we
// probe multiple layouts rather than assuming one.
func candidateSessionDirs(dataDir, projectID string) []string {
	return []string{
		filepath.Join(dataDir, "storage", "session", projectID),
		filepath.Join(dataDir, "project", projectID, "storage", "session"),
		filepath.Join(dataDir, "project", projectID, "storage", "session", "info"),
	}
}

// candidateMessageDirs returns the possible on-disk locations for a
// session's message storage, across OpenCode's known storage layouts.
func candidateMessageDirs(dataDir, projectID, sessionID string) []string {
	return []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "project", projectID, "storage", "session", "message", sessionID),
		filepath.Join(dataDir, "project", projectID, "storage", "message", sessionID),
	}
}

// escapeSQLite escapes a value for embedding in a single-quoted SQLite
// string literal.
func escapeSQLite(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// sqliteTables lists all table names in the given SQLite database.
func sqliteTables(dbPath string) []string {
	cmd := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return splitNonEmpty(string(out))
}

// sqliteColumns lists column names for the given table via PRAGMA table_info.
func sqliteColumns(dbPath, table string) []string {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var cols []string
	for _, line := range splitNonEmpty(string(out)) {
		parts := strings.Split(line, "|")
		if len(parts) >= 2 {
			cols = append(cols, parts[1])
		}
	}
	return cols
}

// splitNonEmpty splits sqlite3 CLI output into non-empty trimmed lines.
func splitNonEmpty(s string) []string {
	var result []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

// findTable returns the first table name matching the given substring
// (case-insensitive), preferring an exact match.
func findTable(tables []string, contains string) string {
	for _, t := range tables {
		if strings.EqualFold(t, contains) {
			return t
		}
	}
	for _, t := range tables {
		if strings.Contains(strings.ToLower(t), contains) {
			return t
		}
	}
	return ""
}

// findColumn returns the first column matching one of the candidate names
// (case-insensitive; exact matches are preferred over substring matches).
func findColumn(cols []string, candidates ...string) string {
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.EqualFold(c, cand) {
				return c
			}
		}
	}
	for _, cand := range candidates {
		for _, c := range cols {
			if strings.Contains(strings.ToLower(c), strings.ToLower(cand)) {
				return c
			}
		}
	}
	return ""
}

// parseSQLiteTime attempts to parse a timestamp value read from SQLite in
// any of the formats OpenCode has used across releases: RFC3339 (with or
// without nanoseconds), space-separated, or a Unix epoch in seconds,
// milliseconds, or microseconds.
func parseSQLiteTime(s string) (time.Time, bool) {
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
		case n > 1e15:
			return time.UnixMicro(n), true
		case n > 1e12:
			return time.UnixMilli(n), true
		default:
			return time.Unix(n, 0), true
		}
	}

	return time.Time{}, false
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
