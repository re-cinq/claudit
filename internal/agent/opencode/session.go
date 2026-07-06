```go
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

// genericSessionFile is a permissive view over an OpenCode session JSON file.
// OpenCode has changed its on-disk field names across releases, so we accept
// several aliases for the fields we care about.
type genericSessionFile struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID"`
	Directory string `json:"directory"`
	Cwd       string `json:"cwd"`
	Path      string `json:"path"`
	Worktree  string `json:"worktree"`
}

func (s genericSessionFile) matchesProject(projectID, absProjectPath string) bool {
	if s.ID == "" {
		return false
	}
	if projectID != "" && s.ProjectID == projectID {
		return true
	}
	for _, candidate := range []string{s.Directory, s.Cwd, s.Path, s.Worktree} {
		if candidate == "" {
			continue
		}
		if absCandidate, err := filepath.Abs(candidate); err == nil && absCandidate == absProjectPath {
			return true
		}
	}
	return false
}

// findMostRecentSession walks the entire OpenCode data directory looking for
// a session JSON file belonging to projectPath. OpenCode's storage layout has
// moved around across versions (flat directories keyed by project ID, nested
// per-project directories, etc.), so instead of assuming a single fixed path
// we scan every *.json file, parse it loosely, and match it against the
// project either by its recorded project ID or by its recorded working
// directory. This makes discovery resilient to layout changes as long as
// OpenCode still writes some JSON file that references the project.
func findMostRecentSession(dataDir, projectID, projectPath string) (sessionID string, modTime time.Time, found bool) {
	absProjectPath, err := filepath.Abs(projectPath)
	if err != nil {
		absProjectPath = projectPath
	}

	now := time.Now()

	_ = filepath.WalkDir(dataDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries, keep scanning
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		if now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var sess genericSessionFile
		if err := json.Unmarshal(data, &sess); err != nil {
			return nil
		}
		if !sess.matchesProject(projectID, absProjectPath) {
			return nil
		}

		if !found || info.ModTime().After(modTime) {
			sessionID = sess.ID
			modTime = info.ModTime()
			found = true
		}
		return nil
	})

	return sessionID, modTime, found
}

// findSessionTranscriptPath locates the on-disk message storage for a
// session ID. It tries the historically-known layout first, then falls back
// to searching the data directory for anything named after the session ID.
func findSessionTranscriptPath(dataDir, sessionID string) string {
	knownCandidates := []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "project", sessionID, "storage", "message"),
	}
	for _, candidate := range knownCandidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}

	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || found != "" {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			found = path
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

// sqliteQuery runs a query against a SQLite database via the sqlite3 CLI and
// returns the output split into lines. It returns (nil, nil) for an empty
// result set.
func sqliteQuery(dbPath, query, separator string) ([]string, error) {
	args := []string{}
	if separator != "" {
		args = append(args, "-separator", separator)
	}
	args = append(args, dbPath, query)

	cmd := exec.Command("sqlite3", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}

// sqliteTableNames returns the user tables defined in the database.
func sqliteTableNames(dbPath string) []string {
	lines, err := sqliteQuery(dbPath, "SELECT name FROM sqlite_master WHERE type='table';", "")
	if err != nil {
		return nil
	}
	return lines
}

// sqliteColumnNames returns the column names of a table.
func sqliteColumnNames(dbPath, table string) []string {
	lines, err := sqliteQuery(dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table), "|")
	if err != nil {
		return nil
	}
	var cols []string
	for _, line := range lines {
		parts := strings.Split(line, "|")
		if len(parts) > 1 {
			cols = append(cols, parts[1])
		}
	}
	return cols
}

// pickTable finds the first table matching one of the preferred (lowercase)
// names exactly, then falls back to a substring match. This tolerates table
// renames across OpenCode releases (e.g. "session" vs "sessions").
func pickTable(tables []string, preferred ...string) string {
	byLower := make(map[string]string, len(tables))
	for _, t := range tables {
		byLower[strings.ToLower(t)] = t
	}
	for _, p := range preferred {
		if t, ok := byLower[p]; ok {
			return t
		}
	}
	for _, t := range tables {
		lower := strings.ToLower(t)
		for _, p := range preferred {
			if strings.Contains(lower, p) {
				return t
			}
		}
	}
	return ""
}

// pickColumn finds the first column matching one of the preferred
// (lowercase) names, tolerating column renames across OpenCode releases.
func pickColumn(cols []string, candidates ...string) string {
	byLower := make(map[string]string, len(cols))
	for _, c := range cols {
		byLower[strings.ToLower(c)] = c
	}
	for _, cand := range candidates {
		if c, ok := byLower[cand]; ok {
			return c
		}
	}
	return ""
}

// withinRecentWindow reports whether a raw timestamp value (RFC3339-ish
// string or Unix epoch in seconds/milliseconds/microseconds) falls within
// the recent-session window. Unrecognized formats are treated as "recent"
// so an unexpected time representation doesn't hide an otherwise-valid
// session.
func withinRecentWindow(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}

	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		var t time.Time
		switch {
		case n > 1_000_000_000_000_000: // microseconds
			t = time.UnixMicro(n)
		case n > 1_000_000_000_000: // milliseconds
			t = time.UnixMilli(n)
		default: // seconds
			t = time.Unix(n, 0)
		}
		return time.Since(t) <= agent.RecentSessionTimeout
	}

	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return time.Since(t) <= agent.RecentSessionTimeout
		}
	}

	return true
}
```
