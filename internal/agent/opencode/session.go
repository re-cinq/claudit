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

// findRecentSession recursively walks dataDir/storage/session looking for the
// most recently modified session within timeout. Newer OpenCode releases have
// repeatedly reshuffled how sessions are laid out on disk (flat "<id>.json"
// files directly under a project folder vs. per-session subdirectories like
// "<id>/info.json"), so rather than hardcoding one exact shape this walks the
// whole tree and infers the session ID from whichever level looks like a
// leaf. When requireMatch is true, only sessions that appear to belong to
// projectID/projectPath (by directory name or by a directory/cwd/worktree/
// projectID field embedded in the JSON) are considered; when false, the
// single most recently modified session anywhere is used as a last resort.
func findRecentSession(dataDir, projectID, projectPath string, timeout time.Duration, requireMatch bool) (sessionID string, modTime time.Time, found bool) {
	root := filepath.Join(dataDir, "storage", "session")
	now := time.Now()

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		modified := info.ModTime()
		if now.Sub(modified) > timeout {
			return nil
		}

		parentDir := filepath.Dir(path)
		parentName := filepath.Base(parentDir)

		var id string
		if parentName == projectID || parentName == "session" {
			// Flat layout: storage/session/<projectID>/<sessionID>.json
			id = strings.TrimSuffix(d.Name(), ".json")
		} else {
			// Nested layout: storage/session/<projectID>/<sessionID>/<file>.json
			id = parentName
		}

		if requireMatch && !sessionBelongsToProject(path, parentName, projectID, projectPath) {
			return nil
		}

		if !found || modified.After(modTime) {
			sessionID, modTime, found = id, modified, true
		}
		return nil
	})

	return sessionID, modTime, found
}

// sessionBelongsToProject checks whether a session file is scoped to the
// given project, either via a path component matching projectID or via a
// directory/cwd/worktree/projectID field embedded in the JSON itself.
func sessionBelongsToProject(path, parentName, projectID, projectPath string) bool {
	if parentName == projectID {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(filepath.Dir(path)), "/") {
		if part == projectID {
			return true
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	var meta struct {
		Directory string `json:"directory"`
		Cwd       string `json:"cwd"`
		Worktree  string `json:"worktree"`
		ProjectID string `json:"projectID"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return false
	}

	for _, v := range []string{meta.Directory, meta.Cwd, meta.Worktree} {
		if v != "" && v == projectPath {
			return true
		}
	}
	return meta.ProjectID != "" && meta.ProjectID == projectID
}

// findMessageDirFallback recursively searches dataDir/storage/message for a
// directory named after sessionID, for layouts that nest messages one level
// deeper (e.g. under a project-scoped subfolder) than the flat
// "storage/message/<sessionID>" path.
func findMessageDirFallback(dataDir, sessionID string) (string, bool) {
	root := filepath.Join(dataDir, "storage", "message")
	var result string

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || result != "" {
			return nil
		}
		if d.IsDir() && d.Name() == sessionID {
			result = path
			return filepath.SkipDir
		}
		return nil
	})

	return result, result != ""
}
