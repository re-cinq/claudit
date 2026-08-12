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

// recentStorageKeywords identify path segments that indicate a file is part
// of OpenCode's session/message storage, as opposed to unrelated config,
// auth, or cache files that also live under the data directory.
var recentStorageKeywords = []string{"session", "message", "storage", "project"}

// looksLikeStorageFile reports whether path contains at least one segment
// associated with OpenCode's session/message storage.
func looksLikeStorageFile(path string) bool {
	lower := strings.ToLower(path)
	for _, kw := range recentStorageKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// discoverByRecentFile performs a storage-layout-agnostic scan of the
// OpenCode data directory for the most recently modified session/message
// file within RecentSessionTimeout. This is a last-resort fallback used
// when neither the flat-file layout (storage/session/<projectID>/*.json)
// nor the SQLite layout (opencode.db) match what's on disk — e.g. because
// a newer OpenCode release reorganized its storage directories.
func discoverByRecentFile(dataDir, projectPath string) (*agent.SessionInfo, error) {
	if _, err := os.Stat(dataDir); err != nil {
		return nil, nil
	}

	now := time.Now()
	var bestPath string
	var bestModTime time.Time

	_ = filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		if !looksLikeStorageFile(path) {
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

		if bestPath == "" || modTime.After(bestModTime) {
			bestPath = path
			bestModTime = modTime
		}
		return nil
	})

	if bestPath == "" {
		return nil, nil
	}

	dir := filepath.Dir(bestPath)
	sessionID := strings.TrimSuffix(filepath.Base(bestPath), filepath.Ext(bestPath))
	transcriptPath := bestPath

	// If the containing directory holds multiple message-like files, treat
	// the whole directory as the transcript source and use its name as the
	// session ID (mirrors the flat-file message-directory layout).
	if entries, err := os.ReadDir(dir); err == nil {
		jsonCount := 0
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".jsonl") {
				jsonCount++
			}
		}
		if jsonCount > 1 {
			transcriptPath = dir
			sessionID = filepath.Base(dir)
		}
	}

	return &agent.SessionInfo{
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
		StartedAt:      bestModTime.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}, nil
}
