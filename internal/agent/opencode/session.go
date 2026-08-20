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

// sessionRecord is a loosely-typed view of an OpenCode session JSON file,
// used for content-based discovery. OpenCode's on-disk session layout has
// changed across versions (nested under a project directory, flat with an
// embedded project reference, etc.), so rather than assuming one fixed
// directory shape we match session records by their content.
type sessionRecord struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID"`
	Project   string `json:"project"`
	Directory string `json:"directory"`
	CWD       string `json:"cwd"`
	Path      string `json:"path"`
	Worktree  string `json:"worktree"`
}

// matchesProject reports whether this session record belongs to the given
// project, checking several possible field names OpenCode has used to
// reference a project/working directory.
func (s sessionRecord) matchesProject(projectPath, projectID string) bool {
	for _, v := range []string{s.ProjectID, s.Project, s.Worktree} {
		if v != "" && v == projectID {
			return true
		}
	}
	for _, v := range []string{s.Directory, s.CWD, s.Path} {
		if v != "" && v == projectPath {
			return true
		}
	}
	return false
}

// FindRecentSession recursively searches dataDir for the most recently
// modified session JSON file belonging to the given project. It matches by
// file content (an "id" field plus a project/directory reference) rather
// than a fixed directory layout, so it keeps working when OpenCode changes
// how it nests session files on disk between versions.
func FindRecentSession(dataDir, projectPath, projectID string) (sessionID string, modTime time.Time, found bool) {
	root := filepath.Join(dataDir, "storage")
	now := time.Now()

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil || now.Sub(info.ModTime()) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var rec sessionRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil
		}

		if rec.ID == "" || !rec.matchesProject(projectPath, projectID) {
			return nil
		}

		if !found || info.ModTime().After(modTime) {
			sessionID = rec.ID
			modTime = info.ModTime()
			found = true
		}

		return nil
	})

	return sessionID, modTime, found
}

// FindMessageDir recursively searches dataDir for a non-empty directory
// holding a session's messages. OpenCode has stored these under different
// parent paths across versions (e.g. storage/message/<id> vs
// storage/session/message/<id>), so this looks for any directory named
// after the session ID rather than a single fixed path.
func FindMessageDir(dataDir, sessionID string) (string, bool) {
	root := filepath.Join(dataDir, "storage")
	var found string

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || d.Name() != sessionID {
			return nil
		}

		entries, err := os.ReadDir(path)
		if err != nil || len(entries) == 0 {
			return nil
		}

		found = path
		return fs.SkipAll
	})

	return found, found != ""
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
```
