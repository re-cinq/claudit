```go
package opencode

import (
	"encoding/json"
	"fmt"
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

// directoryKeys are the known key names OpenCode has used to record a
// session's working directory in its session JSON file.
var directoryKeys = []string{"directory", "cwd", "path", "projectpath", "workingdirectory", "workingdir", "dir"}

// lookupStringCI returns the string value of the first key in candidates
// found in m, matching key names case-insensitively.
func lookupStringCI(m map[string]interface{}, candidates []string) string {
	for k, v := range m {
		lk := strings.ToLower(k)
		for _, c := range candidates {
			if lk == c {
				if s, ok := v.(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

// findSessionFileByDirectory recursively scans dataDir's session storage for
// a session JSON file, modified within timeout, whose recorded directory
// matches projectPath. Unlike GetSessionDir, it does not assume the session
// lives under a specific project-ID subdirectory, so it tolerates OpenCode
// changing how project IDs are derived or where sessions are nested. If no
// file records a recognizable directory field but exactly one recent session
// exists, that session is returned as a best-effort match.
func findSessionFileByDirectory(dataDir, projectPath string, timeout time.Duration) (sessionID string, modTime time.Time, found bool) {
	root := filepath.Join(dataDir, "storage")
	now := time.Now()

	var fallbackID string
	var fallbackModTime time.Time
	fallbackCount := 0

	_ = filepath.Walk(root, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == "message" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}
		if now.Sub(info.ModTime()) > timeout {
			return nil
		}

		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil
		}

		id := lookupStringCI(raw, []string{"id"})
		if id == "" {
			id = strings.TrimSuffix(filepath.Base(p), ".json")
		}

		dir := lookupStringCI(raw, directoryKeys)
		if dir != "" {
			if agent.PathsEqual(dir, projectPath) {
				if !found || info.ModTime().After(modTime) {
					sessionID, modTime, found = id, info.ModTime(), true
				}
			}
			return nil
		}

		// This version's schema doesn't record a recognizable directory
		// field - track as an unscoped fallback, used only if unambiguous.
		fallbackCount++
		if fallbackID == "" || info.ModTime().After(fallbackModTime) {
			fallbackID, fallbackModTime = id, info.ModTime()
		}
		return nil
	})

	if !found && fallbackCount == 1 {
		sessionID, modTime, found = fallbackID, fallbackModTime, true
	}

	return sessionID, modTime, found
}

// findMessageDir locates a session's message directory when the standard
// storage/message/<sessionID> path doesn't exist, tolerating storage-layout
// changes across OpenCode versions.
func findMessageDir(dataDir, sessionID string) string {
	root := filepath.Join(dataDir, "storage")
	var found string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil || found != "" {
			return nil
		}
		if info.IsDir() && info.Name() == sessionID {
			found = p
			return filepath.SkipDir
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
