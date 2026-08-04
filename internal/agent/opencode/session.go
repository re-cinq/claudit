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

// discoverGeneric performs a layout-agnostic scan of OpenCode's data
// directory for the most recently touched session belonging to this
// project. OpenCode has reshuffled its on-disk storage layout across
// releases (flat per-project directories, SQLite, nested key/value style
// "session/info" + "session/message" paths); rather than hard-coding one
// shape, this walks the whole storage tree and matches on file content so
// discovery keeps working the next time the layout moves.
func discoverGeneric(dataDir, projectID, projectPath string) (*agent.SessionInfo, error) {
	storageDir := filepath.Join(dataDir, "storage")
	if _, err := os.Stat(storageDir); err != nil {
		return nil, nil
	}

	now := time.Now()
	var bestID string
	var bestModTime time.Time

	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil || now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}
		// Skip parsing files that couldn't possibly improve on the current
		// best match.
		if bestID != "" && !info.ModTime().After(bestModTime) {
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

		id := jsonStringField(raw, "id")
		if id == "" {
			id = jsonStringField(raw, "sessionID")
		}
		if id == "" {
			return nil
		}

		if !matchesProject(raw, projectID, projectPath) {
			return nil
		}

		bestID = id
		bestModTime = info.ModTime()
		return nil
	})

	if bestID == "" {
		return nil, nil
	}

	return &agent.SessionInfo{
		SessionID:      bestID,
		TranscriptPath: findMessageDir(storageDir, bestID),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// matchesProject reports whether a decoded session/info JSON object
// identifies the given project, trying every field name OpenCode has used
// for this purpose across releases.
func matchesProject(raw map[string]json.RawMessage, projectID, projectPath string) bool {
	if jsonStringField(raw, "projectID") == projectID ||
		jsonStringField(raw, "project_id") == projectID ||
		jsonStringField(raw, "directory") == projectPath ||
		jsonStringField(raw, "cwd") == projectPath ||
		jsonStringField(raw, "worktree") == projectPath {
		return true
	}

	pathRaw, ok := raw["path"]
	if !ok {
		return false
	}
	var p struct {
		Root string `json:"root"`
		Cwd  string `json:"cwd"`
	}
	if json.Unmarshal(pathRaw, &p) != nil {
		return false
	}
	return p.Root == projectPath || p.Cwd == projectPath
}

// jsonStringField extracts a string value for key from a decoded JSON
// object, returning "" if the key is absent or not a string.
func jsonStringField(raw map[string]json.RawMessage, key string) string {
	v, ok := raw[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return s
}

// findMessageDir looks for a directory named after the session ID anywhere
// under storageDir, since OpenCode has nested per-session message
// directories at different depths across releases (e.g.
// storage/message/<id> vs storage/session/message/<id>).
func findMessageDir(storageDir, sessionID string) string {
	var found string
	_ = filepath.WalkDir(storageDir, func(path string, d fs.DirEntry, err error) error {
		if found != "" {
			return filepath.SkipAll
		}
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// querySessionID finds the most recent OpenCode session ID for a project in
// the SQLite database. The "session" table's project-identifying column has
// been renamed across OpenCode releases (project_id, projectID), so this
// tries progressively looser queries; as a last resort it returns the
// single most recently updated session, which is correct for the common
// case of one project per database (as in a git-hook-driven test/repo).
func querySessionID(dbPath, projectID string) string {
	quoted := sqliteEscape(projectID)
	queries := []string{
		fmt.Sprintf(`SELECT id FROM session WHERE project_id='%s' ORDER BY time_updated DESC LIMIT 1;`, quoted),
		fmt.Sprintf(`SELECT id FROM session WHERE projectID='%s' ORDER BY time_updated DESC LIMIT 1;`, quoted),
		`SELECT id FROM session ORDER BY time_updated DESC LIMIT 1;`,
	}
	for _, q := range queries {
		cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, q)
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(string(out)); id != "" {
			return id
		}
	}
	return ""
}

// sqliteEscape escapes single quotes for use in a SQLite string literal.
func sqliteEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
```
