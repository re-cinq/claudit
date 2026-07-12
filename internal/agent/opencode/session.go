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

// findRecentProjectSession walks the OpenCode data directory looking for the
// most recently modified session info file belonging to projectID.
//
// OpenCode has moved where session files live across releases (e.g. a flat
// storage/session/<projectID>/<id>.json layout in older versions vs. a more
// deeply nested layout in newer ones), so rather than assuming one fixed
// path depth this walks the whole data directory and matches files by
// content: it reads each candidate JSON file's "id" and "projectID" fields,
// falling back to the projectID appearing somewhere in the path if an older
// session file predates that field.
func findRecentProjectSession(dataDir, projectID string) (sessionID string, modTime time.Time, found bool) {
	now := time.Now()

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		if !strings.Contains(filepath.ToSlash(path), "session") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		fileModTime := info.ModTime()
		if now.Sub(fileModTime) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var s sessionInfo
		if err := json.Unmarshal(data, &s); err != nil || s.ID == "" {
			return nil
		}

		if s.ProjectID != "" {
			if s.ProjectID != projectID {
				return nil
			}
		} else if !strings.Contains(filepath.ToSlash(path), projectID) {
			return nil
		}

		if !found || fileModTime.After(modTime) {
			sessionID = s.ID
			modTime = fileModTime
			found = true
		}
		return nil
	})

	return sessionID, modTime, found
}

// findSessionMessageDir walks the OpenCode data directory looking for a
// directory holding the messages for sessionID. Like session info files,
// the message directory's location has moved across OpenCode releases, so
// this matches by directory name (the session ID) plus a "message" path
// segment rather than assuming one fixed path.
func findSessionMessageDir(dataDir, sessionID string) string {
	var found string

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if found != "" {
			return filepath.SkipAll
		}
		if err != nil || !d.IsDir() || d.Name() != sessionID {
			return nil
		}
		if strings.Contains(filepath.ToSlash(path), "message") {
			found = path
			return filepath.SkipAll
		}
		return nil
	})

	return found
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
