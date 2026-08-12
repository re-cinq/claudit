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

// maxSessionScanDepth bounds how deep the fallback session/message directory
// scans recurse below the data directory, and maxSessionScanNodes bounds the
// total number of filesystem entries visited. OpenCode has changed how it
// nests session/message storage on disk across releases, so discovery falls
// back to a bounded recursive scan rather than assuming one fixed layout.
const (
	maxSessionScanDepth = 6
	maxSessionScanNodes = 20000
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

// depthBelow returns how many path segments path is below root.
func depthBelow(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return len(strings.Split(rel, string(filepath.Separator)))
}

// FindRecentSession scans the OpenCode data directory for the most recently
// modified session belonging to projectPath, within agent.RecentSessionTimeout.
//
// It first tries the historically-documented flat-file layout,
// "storage/session/<projectID>/<sessionID>.json". If that yields nothing —
// OpenCode has changed exactly how it nests session files on disk across
// releases — it falls back to a bounded recursive scan of "storage/session"
// and matches candidate session files by their own embedded
// "projectID"/"directory" fields instead of relying on directory nesting.
func FindRecentSession(dataDir, projectID, projectPath string) (sessionID string, modTime time.Time, found bool) {
	now := time.Now()

	fastPathDir := filepath.Join(dataDir, "storage", "session", projectID)
	if entries, err := os.ReadDir(fastPathDir); err == nil {
		var bestID string
		var bestModTime time.Time
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			mt := info.ModTime()
			if now.Sub(mt) > agent.RecentSessionTimeout {
				continue
			}
			if bestID == "" || mt.After(bestModTime) {
				bestID = strings.TrimSuffix(entry.Name(), ".json")
				bestModTime = mt
			}
		}
		if bestID != "" {
			return bestID, bestModTime, true
		}
	}

	root := filepath.Join(dataDir, "storage", "session")
	var bestID string
	var bestModTime time.Time
	visited := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		visited++
		if visited > maxSessionScanNodes {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if depthBelow(root, path) > maxSessionScanDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		mt := info.ModTime()
		if now.Sub(mt) > agent.RecentSessionTimeout {
			return nil
		}
		if bestID != "" && !mt.After(bestModTime) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var parsed sessionInfo
		if err := json.Unmarshal(data, &parsed); err != nil {
			return nil
		}
		matches := (parsed.ProjectID != "" && parsed.ProjectID == projectID) ||
			(parsed.Directory != "" && agent.PathsEqual(parsed.Directory, projectPath))
		if !matches {
			return nil
		}

		id := parsed.ID
		if id == "" {
			id = strings.TrimSuffix(d.Name(), ".json")
		}
		bestID = id
		bestModTime = mt
		return nil
	})

	return bestID, bestModTime, bestID != ""
}

// FindMessageDir locates the directory containing a session's message
// files. It first tries the historically-documented flat-file layout,
// "storage/message/<sessionID>". If that doesn't exist, it falls back to a
// bounded recursive search for a directory literally named after the
// session ID anywhere under the data directory's storage tree, since
// OpenCode has changed how it nests message files on disk across releases.
func FindMessageDir(dataDir, sessionID string) string {
	direct := filepath.Join(dataDir, "storage", "message", sessionID)
	if info, err := os.Stat(direct); err == nil && info.IsDir() {
		return direct
	}

	root := filepath.Join(dataDir, "storage")
	var found string
	visited := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		visited++
		if visited > maxSessionScanNodes {
			return filepath.SkipAll
		}
		if depthBelow(root, path) > maxSessionScanDepth {
			return filepath.SkipDir
		}
		if d.Name() == sessionID {
			found = path
			return filepath.SkipAll
		}
		return nil
	})

	return found
}
```
