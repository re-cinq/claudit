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

// sessionCacheFileName is the shiftlog-managed cache file, written under the
// repo's .shiftlog directory, that tracks the most recently active OpenCode
// session's transcript.
//
// OpenCode's own on-disk storage (flat JSON files vs. SQLite, directory
// layout, table/column names) is an internal implementation detail that has
// changed across releases and is not a stable integration point. Instead,
// the shiftlog plugin (see plugin.go) proactively caches the transcript via
// OpenCode's SDK client (client.session.messages) on every tool call, while
// the agent process is still running and the client is reachable. Manual /
// post-commit session discovery reads this cache first, and only falls back
// to scanning OpenCode's on-disk storage when no cache is present (e.g. the
// very first run before the plugin has written one).
const sessionCacheFileName = "opencode-session.json"

// sessionCache is the JSON structure written by the shiftlog plugin to
// <repoRoot>/.shiftlog/opencode-session.json.
type sessionCache struct {
	SessionID      string `json:"session_id"`
	TranscriptData string `json:"transcript_data"`
	UpdatedAt      string `json:"updated_at"`
	ProjectDir     string `json:"project_dir"`
}

// readSessionCache reads the shiftlog-managed session cache for a project.
// It returns nil if no cache file exists, it's malformed, or it has gone stale.
func readSessionCache(projectPath string) *sessionCache {
	cachePath := filepath.Join(projectPath, ".shiftlog", sessionCacheFileName)

	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil
	}

	var cache sessionCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil
	}

	if cache.SessionID == "" || cache.TranscriptData == "" {
		return nil
	}

	updatedAt, err := time.Parse(time.RFC3339, cache.UpdatedAt)
	if err != nil {
		// Fall back to the file's own modification time if the timestamp
		// field is missing or malformed.
		info, statErr := os.Stat(cachePath)
		if statErr != nil {
			return nil
		}
		updatedAt = info.ModTime()
	}

	if time.Since(updatedAt) > agent.RecentSessionTimeout {
		return nil
	}

	return &cache
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
