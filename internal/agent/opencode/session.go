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

// sessionRootCandidates returns, in priority order, directories that OpenCode
// versions have used to store the set of session files belonging to a single
// project. Older releases nested sessions under storage/session/<projectID>;
// newer releases have been observed nesting per-project data under a
// top-level project/<projectID> directory instead.
func sessionRootCandidates(dataDir, projectID string) []string {
	return []string{
		filepath.Join(dataDir, "storage", "session", projectID),
		filepath.Join(dataDir, "project", projectID, "storage", "session"),
	}
}

// findRecentSessionInDirs scans candidate session directories in order and
// returns the most recently modified session ID from the first directory
// that has one updated within RecentSessionTimeout. Each directory may store
// sessions as flat "<id>.json" files or as "<id>/info.json" subdirectories.
func findRecentSessionInDirs(dirs []string) string {
	now := time.Now()

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}

		var bestID string
		var bestMod time.Time

		for _, e := range entries {
			var sessionID string
			var modTime time.Time

			if e.IsDir() {
				info, err := os.Stat(filepath.Join(dir, e.Name(), "info.json"))
				if err != nil {
					continue
				}
				sessionID = e.Name()
				modTime = info.ModTime()
			} else if strings.HasSuffix(e.Name(), ".json") {
				info, err := e.Info()
				if err != nil {
					continue
				}
				sessionID = strings.TrimSuffix(e.Name(), ".json")
				modTime = info.ModTime()
			} else {
				continue
			}

			if now.Sub(modTime) > agent.RecentSessionTimeout {
				continue
			}
			if bestID == "" || modTime.After(bestMod) {
				bestID = sessionID
				bestMod = modTime
			}
		}

		if bestID != "" {
			return bestID
		}
	}

	return ""
}

// findRecentSessionByContent scans a flat, non-partitioned session directory
// (some OpenCode versions store all sessions together instead of nesting
// them under a per-project directory) for the most recently updated session
// whose stored project-identifying field matches this project.
func findRecentSessionByContent(sessionRoot, projectPath, projectID string) string {
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		return ""
	}

	now := time.Now()
	var bestID string
	var bestMod time.Time

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			continue
		}

		data, err := os.ReadFile(filepath.Join(sessionRoot, e.Name()))
		if err != nil || !sessionMatchesProject(data, projectPath, projectID) {
			continue
		}

		sessionID := strings.TrimSuffix(e.Name(), ".json")
		if bestID == "" || modTime.After(bestMod) {
			bestID = sessionID
			bestMod = modTime
		}
	}

	return bestID
}

// sessionMatchesProject reports whether a session info JSON blob belongs to
// the given project, checking whichever project-identifying field the
// running OpenCode version populates on session records.
func sessionMatchesProject(data []byte, projectPath, projectID string) bool {
	var fields struct {
		ProjectID     string `json:"projectID"`
		ProjectIDSnak string `json:"project_id"`
		Directory     string `json:"directory"`
		Worktree      string `json:"worktree"`
		Cwd           string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return false
	}

	if fields.ProjectID == projectID || fields.ProjectIDSnak == projectID {
		return true
	}

	for _, dir := range []string{fields.Directory, fields.Worktree, fields.Cwd} {
		if dir != "" && agent.PathsEqual(dir, projectPath) {
			return true
		}
	}
	return false
}

// messageDirCandidates returns, in priority order, directories that OpenCode
// versions have used to store a session's messages.
func messageDirCandidates(dataDir, projectID, sessionID string) []string {
	return []string{
		filepath.Join(dataDir, "storage", "session", sessionID, "message"),
		filepath.Join(dataDir, "storage", "message", sessionID),
		filepath.Join(dataDir, "project", projectID, "storage", "session", sessionID, "message"),
		filepath.Join(dataDir, "project", projectID, "storage", "message", sessionID),
	}
}

// resolveMessageDir returns the first candidate message directory that
// actually has content. If none do, it falls back to the legacy path so the
// caller gets a clear "failed to read transcript" error instead of silently
// dropping the session.
func resolveMessageDir(dataDir, projectID, sessionID string) string {
	for _, dir := range messageDirCandidates(dataDir, projectID, sessionID) {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return dir
		}
	}
	return filepath.Join(dataDir, "storage", "message", sessionID)
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
