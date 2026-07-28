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

// sessionDirCandidate is a directory that may contain OpenCode session JSON
// files. When projectScoped is true, the directory path itself already
// narrows results to the current project (e.g. it includes the project ID
// as a path segment), so any JSON file found there can be trusted as
// belonging to this project. When false, the directory may contain sessions
// for multiple projects (e.g. a flat, non-nested layout), so callers must
// inspect file content to confirm the project before accepting a match.
type sessionDirCandidate struct {
	path          string
	projectScoped bool
}

// candidateSessionDirs returns directories that may contain OpenCode session
// JSON files for a given project. OpenCode has changed its on-disk storage
// layout across releases (e.g. "storage/session/<id>" vs a newer
// "storage/session/info/<id>" layout, with or without a top-level
// "project/<id>" nesting), so we probe several known shapes rather than
// assuming a single fixed layout.
func candidateSessionDirs(dataDir, projectID string) []sessionDirCandidate {
	return []sessionDirCandidate{
		{filepath.Join(dataDir, "storage", "session", projectID), true},
		{filepath.Join(dataDir, "storage", "session", "info", projectID), true},
		{filepath.Join(dataDir, "project", projectID, "storage", "session"), true},
		{filepath.Join(dataDir, "project", projectID, "storage", "session", "info"), true},
		// Flat layouts with no per-project directory nesting: sessions for
		// all projects live side by side, keyed by a "projectID"/"directory"
		// field inside each session file instead.
		{filepath.Join(dataDir, "storage", "session", "info"), false},
		{filepath.Join(dataDir, "storage", "session"), false},
	}
}

// candidateMessageDirs returns directories that may contain OpenCode message
// JSON files for a session, mirroring the storage layout variations handled
// by candidateSessionDirs.
func candidateMessageDirs(dataDir, projectID, sessionID string) []string {
	return []string{
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "storage", "session", "message", sessionID),
		filepath.Join(dataDir, "project", projectID, "storage", "message", sessionID),
		filepath.Join(dataDir, "project", projectID, "storage", "session", "message", sessionID),
	}
}

// resolveMessageDir returns the first existing candidate message directory
// for a session, falling back to the legacy default path if none exist yet
// (e.g. the message directory hasn't been created at discovery time).
func resolveMessageDir(dataDir, projectID, sessionID string) string {
	for _, dir := range candidateMessageDirs(dataDir, projectID, sessionID) {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return filepath.Join(dataDir, "storage", "message", sessionID)
}

// sessionMatchesProject reports whether session JSON data belongs to the
// given project, checking field names used across OpenCode versions.
func sessionMatchesProject(data []byte, projectID, projectPath string) bool {
	var probe struct {
		ProjectID string `json:"projectID"`
		Directory string `json:"directory"`
		Path      string `json:"path"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	if probe.ProjectID != "" && probe.ProjectID == projectID {
		return true
	}
	if probe.Directory != "" && probe.Directory == projectPath {
		return true
	}
	if probe.Path != "" && probe.Path == projectPath {
		return true
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
