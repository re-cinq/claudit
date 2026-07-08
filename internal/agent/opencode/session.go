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

// storageLayout describes one generation of OpenCode's on-disk session
// storage: a directory containing per-session JSON files, plus a way to
// derive the message directory for a session found in that layout.
type storageLayout struct {
	sessionDir    string
	messageDirFor func(sessionID string) string
}

// nestedLayouts returns the per-project ("project/<id>/storage/...") layout
// candidates used by newer OpenCode releases, keyed off of the given
// storage base directory (dataDir/project/<id>/storage).
func nestedLayouts(storageBase string) []storageLayout {
	messageDirFor := func(sessionID string) string {
		return filepath.Join(storageBase, "session", "message", sessionID)
	}
	return []storageLayout{
		// OpenCode versions that key session metadata under an "info" subdir.
		{sessionDir: filepath.Join(storageBase, "session", "info"), messageDirFor: messageDirFor},
		// OpenCode versions that store session JSON directly under "session".
		{sessionDir: filepath.Join(storageBase, "session"), messageDirFor: messageDirFor},
	}
}

// storageLayouts returns all known OpenCode storage layouts to probe for a
// given project: the legacy flat layout (pre-v1.2, "storage/session/<projectID>")
// first since it was the first verified layout, followed by the newer
// per-project nested layouts introduced in later releases. OpenCode's
// on-disk format has changed across releases (see .github/agent-versions.json
// for the pinned last-known-good version), so DiscoverSession must tolerate
// drift instead of assuming a single fixed path.
func storageLayouts(dataDir, projectID string) []storageLayout {
	layouts := []storageLayout{
		{
			sessionDir: filepath.Join(dataDir, "storage", "session", projectID),
			messageDirFor: func(sessionID string) string {
				return filepath.Join(dataDir, "storage", "message", sessionID)
			},
		},
	}
	return append(layouts, nestedLayouts(filepath.Join(dataDir, "project", projectID, "storage"))...)
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
```
