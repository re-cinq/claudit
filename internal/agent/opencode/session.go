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

// candidateDataDirs returns the data directories to search for OpenCode's
// storage, in priority order. OpenCode has relocated its data directory
// across versions, so the canonical XDG/macOS location is tried first and
// alternate, previously-used locations are tried afterward.
func candidateDataDirs() []string {
	seen := make(map[string]bool)
	var dirs []string

	add := func(dir string) {
		if dir != "" && !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}

	if primary, err := GetDataDir(); err == nil {
		add(primary)
	}

	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".opencode"))
		add(filepath.Join(home, ".local", "share", "opencode"))
	}

	return dirs
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

// sessionFileDirectory extracts the recorded working directory from a
// flat-file OpenCode session JSON blob, trying the field names OpenCode has
// used for this across versions. Returns "" if none are present.
func sessionFileDirectory(data []byte) string {
	var fields struct {
		Directory string `json:"directory"`
		Worktree  string `json:"worktree"`
		Cwd       string `json:"cwd"`
		Path      string `json:"path"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return ""
	}
	for _, v := range []string{fields.Directory, fields.Worktree, fields.Cwd, fields.Path} {
		if v != "" {
			return v
		}
	}
	return ""
}

// sqliteTables returns the set of table names present in the OpenCode
// SQLite database.
func sqliteTables(dbPath string) map[string]bool {
	out := querySQLiteScalar(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	tables := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			tables[line] = true
		}
	}
	return tables
}

// sqliteColumns returns the set of column names for a table, introspected
// via PRAGMA since OpenCode's schema has changed across versions.
func sqliteColumns(dbPath, table string) (map[string]bool, error) {
	cmd := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table))
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	cols := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) >= 2 && parts[1] != "" {
			cols[parts[1]] = true
		}
	}
	return cols, nil
}

// resolveName returns the first candidate present in names, or "" if none match.
func resolveName(names map[string]bool, candidates ...string) string {
	for _, c := range candidates {
		if names[c] {
			return c
		}
	}
	return ""
}

// querySQLiteScalar runs a query against the OpenCode database and returns
// the trimmed output, or "" on error or an empty result.
func querySQLiteScalar(dbPath, query string) string {
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// sqlQuote quotes and escapes a string for interpolation into a SQLite
// statement as a string literal.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// parseOpenCodeTime parses a session timestamp using the formats OpenCode
// has used across versions, including Unix epoch seconds/milliseconds.
func parseOpenCodeTime(s string) (time.Time, bool) {
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

	if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > 0 {
		if v > 1e12 {
			return time.UnixMilli(v), true
		}
		return time.Unix(v, 0), true
	}

	return time.Time{}, false
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
