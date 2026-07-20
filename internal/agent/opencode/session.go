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

// FindRecentSessionFile recursively scans root (OpenCode's session storage
// directory) for the most recently modified session JSON file belonging to
// projectID/projectPath, within agent.RecentSessionTimeout.
//
// OpenCode has changed the on-disk nesting of session files across releases
// (e.g. inserting extra subdirectories under storage/session/<projectID>/),
// so callers can't assume session files sit directly inside the
// project-scoped directory. A file is treated as a match if its own content
// carries a matching "projectID" or "directory" field, or if the project ID
// still appears as a path segment (the legacy flat layout).
func FindRecentSessionFile(root, projectID, projectPath string) (sessionID string, modTime time.Time) {
	projectSegment := string(filepath.Separator) + projectID + string(filepath.Separator)

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		mt := info.ModTime()
		if time.Since(mt) > agent.RecentSessionTimeout {
			return nil
		}
		if sessionID != "" && !mt.After(modTime) {
			return nil
		}

		var si sessionInfo
		if data, readErr := os.ReadFile(path); readErr == nil {
			_ = json.Unmarshal(data, &si)
		}

		matches := (si.ProjectID != "" && si.ProjectID == projectID) ||
			(si.Directory != "" && si.Directory == projectPath) ||
			strings.Contains(path, projectSegment)
		if !matches {
			return nil
		}

		id := si.ID
		if id == "" {
			id = strings.TrimSuffix(d.Name(), ".json")
		}

		sessionID, modTime = id, mt
		return nil
	})

	return sessionID, modTime
}

// FindTranscriptDir locates the directory holding a session's message files.
// It first checks OpenCode's known message directory layout (GetMessageDir),
// then falls back to a recursive search under dataDir/storage for a
// directory named after the session ID. This tolerates OpenCode versions
// that restructure message storage relative to the session ID.
func FindTranscriptDir(dataDir, sessionID string) string {
	if dir, err := GetMessageDir(sessionID); err == nil && hasTranscriptFiles(dir) {
		return dir
	}

	storageRoot := filepath.Join(dataDir, "storage")
	var found string
	_ = filepath.WalkDir(storageRoot, func(path string, d fs.DirEntry, err error) error {
		if found != "" || err != nil || d == nil || !d.IsDir() || d.Name() != sessionID {
			return nil
		}
		if hasTranscriptFiles(path) {
			found = path
		}
		return nil
	})

	return found
}

// hasTranscriptFiles reports whether dir directly contains JSON or JSONL files.
func hasTranscriptFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".jsonl") {
			return true
		}
	}
	return false
}
```
