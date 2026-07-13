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

// sessionFileHeader captures the fields we need to identify a session file
// regardless of exactly where OpenCode places it on disk.
type sessionFileHeader struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID"`
	Directory string `json:"directory"`
}

// scanForSessionFile performs a layout-agnostic recursive scan of the OpenCode
// data directory for a session file belonging to the given project.
//
// OpenCode's on-disk session layout has changed across releases (this package
// already tracks the move from flat storage/session/<projectID>/ files to a
// SQLite-backed store in discoverFromSQLite). Rather than hardcoding another
// specific directory shape that may again drift out from under us on the next
// release, this walks the whole data directory looking for any recently
// modified *.json file whose content identifies it as belonging to this
// project (via a "projectID" or "directory" field), independent of nesting.
func scanForSessionFile(dataDir, projectPath string) (*agent.SessionInfo, error) {
	if _, err := os.Stat(dataDir); err != nil {
		return nil, nil
	}

	expectedProjectID := GetProjectID(projectPath)
	now := time.Now()

	var bestPath string
	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var header sessionFileHeader
		if err := json.Unmarshal(data, &header); err != nil || header.ID == "" {
			return nil
		}

		matches := (header.ProjectID != "" && header.ProjectID == expectedProjectID) ||
			(header.Directory != "" && agent.PathsEqual(header.Directory, projectPath))
		if !matches {
			return nil
		}

		if bestPath == "" || modTime.After(bestModTime) {
			bestPath = path
			bestSessionID = header.ID
			bestModTime = modTime
		}
		return nil
	})

	if bestSessionID == "" {
		return nil, nil
	}

	// Prefer a dedicated message directory for this session if one exists
	// anywhere under the data directory; otherwise fall back to the session
	// file itself so a note can still be recorded with at least the session
	// identity.
	transcriptPath := bestPath
	if msgDir := findMessageDir(dataDir, bestSessionID); msgDir != "" {
		transcriptPath = msgDir
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: transcriptPath,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}

// findMessageDir searches for a directory named after the session ID anywhere
// under dataDir. OpenCode stores per-session message files in a directory
// keyed by session ID, but the parent path has varied across releases (e.g.
// storage/message/<id> vs. storage/session/message/<id>).
func findMessageDir(dataDir, sessionID string) string {
	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if found != "" {
			return filepath.SkipAll
		}
		if err != nil || !d.IsDir() {
			return nil
		}
		if d.Name() == sessionID {
			found = path
			return filepath.SkipDir
		}
		return nil
	})
	return found
}
```
