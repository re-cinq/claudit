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

// GetSessionDir returns the legacy flat-file session storage directory for a
// project: <dataDir>/storage/session/<projectID>. Newer OpenCode releases
// have moved session storage around; see findRecentSession for the
// version-resilient discovery path.
func GetSessionDir(projectPath string) (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}

	projectID := GetProjectID(projectPath)
	return filepath.Join(dataDir, "storage", "session", projectID), nil
}

// GetMessageDir returns the legacy flat-file message storage directory for a
// session: <dataDir>/storage/message/<sessionID>.
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

// findRecentSession walks the OpenCode data directory looking for a session
// info file belonging to the given project, matched either by OpenCode's
// projectID or by the session's recorded working directory. OpenCode has
// reorganized its on-disk storage layout across releases (e.g. moving from a
// flat storage/session/<projectID>/ tree to a project-scoped tree), so
// rather than assuming one fixed path, this scans every *.json file under
// dataDir and inspects its contents directly. Returns the most recently
// modified matching session within recentTimeout, or ok=false if none found.
func findRecentSession(dataDir, projectPath, projectID string, recentTimeout time.Duration) (id string, modTime time.Time, ok bool) {
	now := time.Now()
	cleanProjectPath := filepath.Clean(projectPath)

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}

		var si sessionInfo
		if err := json.Unmarshal(data, &si); err != nil {
			return nil
		}

		matches := (si.ProjectID != "" && si.ProjectID == projectID) ||
			(si.Directory != "" && filepath.Clean(si.Directory) == cleanProjectPath)
		if !matches {
			return nil
		}

		sessionID := si.ID
		if sessionID == "" {
			sessionID = strings.TrimSuffix(d.Name(), ".json")
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}

		fileModTime := info.ModTime()
		if now.Sub(fileModTime) > recentTimeout {
			return nil
		}

		if !ok || fileModTime.After(modTime) {
			id = sessionID
			modTime = fileModTime
			ok = true
		}

		return nil
	})

	return id, modTime, ok
}

// findDirByName walks root looking for a directory whose base name matches
// name (e.g. a session ID). This is used to locate a session's message
// directory regardless of how deeply a given OpenCode release nests it
// under the data directory.
func findDirByName(root, name string) (string, bool) {
	var found string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() && d.Name() == name {
			found = path
		}
		return nil
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
