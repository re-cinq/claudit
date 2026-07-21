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

// scannedSessionInfo is the shape used when scanning for session files whose
// exact on-disk location may have changed between OpenCode versions.
type scannedSessionInfo struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectID"`
	Directory string `json:"directory"`
}

// discoverByScanning walks the OpenCode data directory looking for a session
// info file that belongs to this project. Unlike GetSessionDir/GetMessageDir,
// it does not assume an exact directory layout — only that session files are
// JSON with an "id" plus a "projectID" or "directory" field, and that a
// session's messages live under a directory named after its session ID
// somewhere in the tree. This tolerates OpenCode reorganizing its storage
// layout across versions (e.g. adding a "project/<id>/" wrapper directory,
// or nesting "session"/"message" differently).
func discoverByScanning(dataDir, projectPath string) *agent.SessionInfo {
	projectID := GetProjectID(projectPath)
	now := time.Now()

	var bestSessionID string
	var bestModTime time.Time

	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		modTime := info.ModTime()
		if now.Sub(modTime) > agent.RecentSessionTimeout {
			return nil
		}

		// Skip unusually large files — session info files are small.
		if info.Size() > 1<<20 {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var candidate scannedSessionInfo
		if err := json.Unmarshal(data, &candidate); err != nil || candidate.ID == "" {
			return nil
		}

		matches := (candidate.ProjectID != "" && candidate.ProjectID == projectID) ||
			(candidate.Directory != "" && agent.PathsEqual(candidate.Directory, projectPath))
		if !matches {
			return nil
		}

		if bestSessionID == "" || modTime.After(bestModTime) {
			bestSessionID = candidate.ID
			bestModTime = modTime
		}
		return nil
	})

	if bestSessionID == "" {
		return nil
	}

	return &agent.SessionInfo{
		SessionID:      bestSessionID,
		TranscriptPath: findMessageDir(dataDir, bestSessionID),
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}

// findMessageDir looks for a directory named after the session ID anywhere
// under dataDir, which is where OpenCode stores that session's message files
// regardless of how deeply the surrounding storage tree is nested.
func findMessageDir(dataDir, sessionID string) string {
	var found string
	_ = filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if found != "" {
			return fs.SkipAll
		}
		if d.IsDir() && d.Name() == sessionID {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	return found
}
