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

// FindRecentSessionID scans OpenCode's session storage tree for the most
// recently modified session belonging to projectPath, within the given
// timeout window.
//
// OpenCode has changed how it partitions session storage by project across
// versions (the project-ID computation and directory nesting are internal
// implementation details), so rather than predicting the exact directory a
// session lives under (as GetSessionDir does), this walks the whole session
// tree and matches each session file's own recorded "directory" field
// against projectPath. This mirrors how the Codex and Claude agents in this
// package discover sessions (by content, not by a guessed path), which
// keeps discovery working even when OpenCode's on-disk layout shifts.
//
// The legacy projectID match (GetSessionDir's assumption) is also accepted
// as a fallback signal for older OpenCode versions that still use it.
func FindRecentSessionID(projectPath string, timeout time.Duration) (sessionID string, modTime time.Time, err error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", time.Time{}, err
	}

	sessionRoot := filepath.Join(dataDir, "storage", "session")
	projectID := GetProjectID(projectPath)
	now := time.Now()

	_ = filepath.Walk(sessionRoot, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}

		fileModTime := info.ModTime()
		if now.Sub(fileModTime) > timeout {
			return nil
		}

		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}

		var s sessionInfo
		if jsonErr := json.Unmarshal(data, &s); jsonErr != nil || s.ID == "" {
			return nil
		}

		matches := (s.Directory != "" && agent.PathsEqual(s.Directory, projectPath)) ||
			(s.ProjectID != "" && s.ProjectID == projectID)
		if !matches {
			return nil
		}

		if sessionID == "" || fileModTime.After(modTime) {
			sessionID = s.ID
			modTime = fileModTime
		}
		return nil
	})

	return sessionID, modTime, nil
}

// FindMessageDir locates the directory containing message files for sessionID.
// The expected layout is dataDir/storage/message/<sessionID>, but newer
// OpenCode versions may nest messages elsewhere under the storage tree (e.g.
// under the session's own directory). If the expected path doesn't exist,
// this falls back to searching storage/ for any directory literally named
// after the session ID.
func FindMessageDir(sessionID string) (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}

	direct := filepath.Join(dataDir, "storage", "message", sessionID)
	if info, statErr := os.Stat(direct); statErr == nil && info.IsDir() {
		return direct, nil
	}

	storageRoot := filepath.Join(dataDir, "storage")
	var found string
	_ = filepath.Walk(storageRoot, func(p string, info os.FileInfo, walkErr error) error {
		if found != "" || walkErr != nil || info == nil || !info.IsDir() {
			return nil
		}
		if info.Name() == sessionID {
			found = p
			return filepath.SkipDir
		}
		return nil
	})

	if found != "" {
		return found, nil
	}
	return direct, nil
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
