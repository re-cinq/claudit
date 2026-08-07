```go
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

// directoryFieldNames are the JSON/column field names OpenCode has used
// (across versions) to record the working directory a session belongs to.
var directoryFieldNames = []string{"directory", "cwd", "path", "projectPath", "worktree", "root"}

// sessionLikeFieldNames are field names that, together with "id", indicate a
// JSON file describes session metadata rather than a single message.
var sessionLikeFieldNames = []string{"title", "directory", "cwd", "path", "projectPath", "worktree", "root", "projectID", "project_id"}

// messageOnlyFieldNames indicate a JSON file is a message, not session info.
var messageOnlyFieldNames = []string{"role", "parts", "content"}

// scanSessionTree performs a best-effort recursive scan of the OpenCode data
// directory for a session belonging to projectPath. It does not assume any
// particular on-disk layout, so it keeps working across OpenCode storage
// refactors: it looks for any JSON file that looks like session metadata
// (has an "id" field, and either a directory-like field or other session-ish
// fields), preferring one whose recorded directory matches projectPath, and
// otherwise falling back to the most recently modified candidate within the
// recent-session window.
func scanSessionTree(dataDir, projectPath string) *agent.SessionInfo {
	info, err := os.Stat(dataDir)
	if err != nil || !info.IsDir() {
		return nil
	}

	now := time.Now()
	var (
		bestID       string
		bestModTime  time.Time
		bestDirMatch bool
		found        bool
	)

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return nil
		}
		modTime := fi.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
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

		if hasAnyField(raw, messageOnlyFieldNames) {
			return nil
		}

		sessionID := stringFieldOrDefault(raw, "id", strings.TrimSuffix(d.Name(), ".json"))
		if sessionID == "" || !hasAnyField(raw, sessionLikeFieldNames) {
			return nil
		}

		dirMatch := false
		if dir := firstStringField(raw, directoryFieldNames); dir != "" {
			dirMatch = agent.PathsEqual(dir, projectPath)
		}

		switch {
		case !found:
			bestID, bestModTime, bestDirMatch, found = sessionID, modTime, dirMatch, true
		case dirMatch && !bestDirMatch:
			bestID, bestModTime, bestDirMatch = sessionID, modTime, dirMatch
		case dirMatch == bestDirMatch && modTime.After(bestModTime):
			bestID, bestModTime, bestDirMatch = sessionID, modTime, dirMatch
		}
		return nil
	})

	if !found {
		return nil
	}

	return &agent.SessionInfo{
		SessionID:      bestID,
		TranscriptPath: findMessageDir(dataDir, bestID),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// findMessageDir searches dataDir for a directory named after sessionID that
// contains message-like JSON/JSONL files, regardless of where it's nested.
func findMessageDir(dataDir, sessionID string) string {
	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, walkErr error) error {
		if found != "" {
			return filepath.SkipAll
		}
		if walkErr != nil || !d.IsDir() || d.Name() != sessionID {
			return nil
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if !e.IsDir() && (strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".jsonl")) {
				found = path
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}

func hasAnyField(raw map[string]json.RawMessage, names []string) bool {
	for _, name := range names {
		if _, ok := raw[name]; ok {
			return true
		}
	}
	return false
}

func firstStringField(raw map[string]json.RawMessage, names []string) string {
	for _, name := range names {
		if v, ok := raw[name]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err == nil && s != "" {
				return s
			}
		}
	}
	return ""
}

func stringFieldOrDefault(raw map[string]json.RawMessage, name, def string) string {
	if v, ok := raw[name]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err == nil && s != "" {
			return s
		}
	}
	return def
}

// --- SQLite introspection helpers ---
//
// OpenCode's SQLite schema (table/column names) has changed across versions.
// Rather than hardcoding names that may drift again, these helpers inspect
// the database at runtime and pick the best-matching table/column names from
// a list of known historical and plausible candidates.

func sqliteQuery(dbPath, query string) (string, error) {
	out, err := exec.Command("sqlite3", dbPath, query).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func sqliteTables(dbPath string) []string {
	out, err := sqliteQuery(dbPath, "SELECT name FROM sqlite_master WHERE type='table';")
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func sqliteColumns(dbPath, table string) []string {
	out, err := exec.Command("sqlite3", "-json", dbPath,
		fmt.Sprintf("PRAGMA table_info(%s);", quoteSQLiteIdent(table))).Output()
	if err != nil {
		return nil
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil
	}
	cols := make([]string, 0, len(rows))
	for _, r := range rows {
		cols = append(cols, r.Name)
	}
	return cols
}

func pickTable(tables, candidates []string) string {
	for _, c := range candidates {
		for _, t := range tables {
			if strings.EqualFold(t, c) {
				return t
			}
		}
	}
	return ""
}

func pickColumn(cols, candidates []string) string {
	for _, c := range candidates {
		for _, col := range cols {
			if strings.EqualFold(col, c) {
				return col
			}
		}
	}
	return ""
}

func quoteSQLiteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func escapeSQLiteLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// parseFlexibleTime parses a timestamp that may be an RFC3339-ish string or a
// numeric Unix epoch in seconds, milliseconds, microseconds, or nanoseconds.
func parseFlexibleTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}

	for _, f := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(f, s); err == nil {
			return t, true
		}
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		switch {
		case n > 1e17: // nanoseconds
			return time.Unix(0, n), true
		case n > 1e14: // microseconds
			return time.Unix(0, n*1000), true
		case n > 1e11: // milliseconds
			return time.Unix(0, n*int64(time.Millisecond)), true
		default: // seconds
			return time.Unix(n, 0), true
		}
	}

	return time.Time{}, false
}
```
