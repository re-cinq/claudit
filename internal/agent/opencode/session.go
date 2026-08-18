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

// sessionSearchRoots returns the directories that may contain OpenCode
// session files, across the different storage layouts OpenCode has used:
//   - storage/session/<projectID>/<id>.json (flat layout)
//   - project/<projectID>/storage/session/<id>.json (nested layout)
func sessionSearchRoots(dataDir string) []string {
	return []string{
		filepath.Join(dataDir, "storage", "session"),
		filepath.Join(dataDir, "project"),
	}
}

// isSessionFileForProject reports whether a session JSON file belongs to
// the given project, either by its location on disk (a path segment
// matching projectID) or by fields inside the file itself
// (projectID/directory/path/cwd).
func isSessionFileForProject(path, projectID, projectPath string) bool {
	for _, part := range strings.Split(filepath.ToSlash(filepath.Dir(path)), "/") {
		if part != "" && part == projectID {
			return true
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	var meta struct {
		ProjectID string `json:"projectID"`
		Directory string `json:"directory"`
		Path      string `json:"path"`
		CWD       string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return false
	}
	if meta.ProjectID != "" && meta.ProjectID == projectID {
		return true
	}
	for _, candidate := range []string{meta.Directory, meta.Path, meta.CWD} {
		if candidate == "" {
			continue
		}
		if candidate == projectPath {
			return true
		}
		if abs, err := filepath.Abs(candidate); err == nil && abs == projectPath {
			return true
		}
	}
	return false
}

// findMessageDir locates the message directory for a session, trying the
// known storage layouts before falling back to a search of the whole data
// directory for a "message/<sessionID>" path. OpenCode has moved the
// message directory around relative to the session directory across
// releases, so this doesn't assume a single fixed relationship.
func findMessageDir(dataDir, sessionID, sessionFilePath string) string {
	candidates := []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
	}

	if sessionFilePath != "" {
		sessionDir := filepath.Dir(sessionFilePath)
		if filepath.Base(sessionDir) == "session" {
			storageDir := filepath.Dir(sessionDir)
			candidates = append(candidates, filepath.Join(storageDir, "message", sessionID))
		}
	}

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}

	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" || !d.IsDir() {
			return nil
		}
		if d.Name() == sessionID && filepath.Base(filepath.Dir(path)) == "message" {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if found != "" {
		return found
	}

	return candidates[0]
}
```
