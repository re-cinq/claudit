package opencode

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// findRecentDataFile performs a best-effort recursive scan of the OpenCode data
// directory for the most recently modified session-like file. This acts as a
// resilience fallback when the flat-file and SQLite discovery paths don't
// recognize the on-disk layout used by a given OpenCode version.
func findRecentDataFile(dataDir string, timeout time.Duration) (path string, modTime time.Time, err error) {
	now := time.Now()

	walkErr := filepath.WalkDir(dataDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if t := info.ModTime(); now.Sub(t) <= timeout && (path == "" || t.After(modTime)) {
			path = p
			modTime = t
		}
		return nil
	})
	if walkErr != nil {
		return "", time.Time{}, walkErr
	}
	return path, modTime, nil
}

// deriveSessionID makes a best-effort attempt to extract a session identifier
// from a discovered data file, falling back to the file's base name.
func deriveSessionID(path string) string {
	if data, err := os.ReadFile(path); err == nil {
		var probe map[string]json.RawMessage
		if json.Unmarshal(data, &probe) == nil {
			for _, key := range []string{"id", "sessionID", "session_id", "sessionId"} {
				if raw, ok := probe[key]; ok {
					var s string
					if json.Unmarshal(raw, &s) == nil && s != "" {
						return s
					}
				}
			}
		}
	}
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// findTable finds a table name in the SQLite database matching the given hint
// (a singular, lowercase noun, e.g. "session"). Falls back to the hint itself
// if no matching table is found, preserving prior hardcoded-name behavior.
func findTable(dbPath, hint string) string {
	out, err := exec.Command("sqlite3", dbPath, "SELECT name FROM sqlite_master WHERE type='table';").Output()
	if err != nil {
		return hint
	}

	var partial string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name := strings.TrimSpace(line)
		lower := strings.ToLower(name)
		switch lower {
		case hint, hint + "s":
			return name
		}
		if partial == "" && strings.Contains(lower, hint) {
			partial = name
		}
	}
	if partial != "" {
		return partial
	}
	return hint
}

// tableColumns returns the column names of a SQLite table.
func tableColumns(dbPath, table string) []string {
	out, err := exec.Command("sqlite3", dbPath, fmt.Sprintf("PRAGMA table_info(%s);", table)).Output()
	if err != nil {
		return nil
	}

	var cols []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 1 {
			cols = append(cols, strings.TrimSpace(fields[1]))
		}
	}
	return cols
}

// pickColumn returns the first candidate column name (case-insensitive) that
// is present in cols, or "" if none match.
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

// escapeSQLString escapes single quotes for safe use in a SQLite string literal.
func escapeSQLString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
