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

// candidateSessionDirs returns, in priority order, directories that may
// contain this project's OpenCode session files. OpenCode's on-disk layout
// has changed across releases: some versions nest session files under a
// per-project directory (storage/session/<projectID>), while others store
// all sessions in one flat directory (storage/session/info or
// storage/session) and record the owning project inside each session file.
// The first entry is always the legacy per-project directory.
func candidateSessionDirs(dataDir, projectID string) []string {
	return []string{
		filepath.Join(dataDir, "storage", "session", projectID),
		filepath.Join(dataDir, "storage", "session", "info"),
		filepath.Join(dataDir, "storage", "session"),
	}
}

// candidateMessageDirs returns, in priority order, directories that may
// contain message files for a session.
func candidateMessageDirs(dataDir, sessionID string) []string {
	return []string{
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
		filepath.Join(dataDir, "storage", "message", sessionID),
	}
}

// resolveMessageDir picks the first candidate message directory that
// actually exists and contains entries, falling back to the legacy path
// (storage/message/<sessionID>) so callers get a stable default even when
// nothing is found on disk yet.
func resolveMessageDir(dataDir, sessionID string) string {
	dirs := candidateMessageDirs(dataDir, sessionID)
	for _, dir := range dirs {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return dir
		}
	}
	return dirs[len(dirs)-1]
}

// sessionIDFromFile extracts the "id" field from a session JSON blob, if
// present.
func sessionIDFromFile(data []byte) string {
	var sess struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &sess) == nil {
		return sess.ID
	}
	return ""
}

// sessionBelongsToProject checks whether a session JSON blob references the
// given project, trying every field name OpenCode has used across releases
// to record project ownership (a project ID hash, or the project's absolute
// directory path).
func sessionBelongsToProject(data []byte, projectID, absProjectPath string) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false
	}

	for _, key := range []string{"projectID", "project_id", "projectId"} {
		if raw, ok := fields[key]; ok {
			var v string
			if json.Unmarshal(raw, &v) == nil && v == projectID {
				return true
			}
		}
	}

	for _, key := range []string{"directory", "worktree", "path", "cwd"} {
		if raw, ok := fields[key]; ok {
			var v string
			if json.Unmarshal(raw, &v) == nil && v == absProjectPath {
				return true
			}
		}
	}

	return false
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
