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

// sessionCacheFileName is shiftlog's own record of the most recently
// observed OpenCode session for a project, written under the project's
// .shiftlog directory (already gitignored).
const sessionCacheFileName = "opencode-session-cache.json"

// cachedSession is the on-disk representation of a live-discovered session,
// captured by CacheSession while the shiftlog plugin's tool.execute.after
// hook fires (i.e. while the OpenCode CLI process is still running).
// OpenCode's own on-disk session storage location/format has changed across
// releases and is not guaranteed to be discoverable once a non-interactive
// ("opencode run") invocation has exited, so shiftlog keeps its own record
// here (sourced from the stable SDK client API) to support manual
// (post-commit hook) session discovery.
type cachedSession struct {
	SessionID      string    `json:"session_id"`
	TranscriptData []byte    `json:"transcript_data,omitempty"`
	CachedAt       time.Time `json:"cached_at"`
}

// sessionCachePath returns the path to the project's cached-session file.
func sessionCachePath(projectPath string) string {
	return filepath.Join(projectPath, ".shiftlog", sessionCacheFileName)
}

// CacheSession records a live-discovered session for later lookup by
// ReadCachedSession. It is called from ParseHookInput while the OpenCode CLI
// process is still active and the SDK-sourced transcript data is available.
func CacheSession(projectPath, sessionID string, transcriptData []byte) {
	if projectPath == "" || sessionID == "" || len(transcriptData) == 0 {
		return
	}

	cached := cachedSession{
		SessionID:      sessionID,
		TranscriptData: transcriptData,
		CachedAt:       time.Now(),
	}
	out, err := json.Marshal(&cached)
	if err != nil {
		return
	}

	dir := filepath.Join(projectPath, ".shiftlog")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	_ = os.WriteFile(sessionCachePath(projectPath), out, 0600)
}

// ReadCachedSession returns the most recently cached session for a project,
// if one was recorded within the recent-session window.
func ReadCachedSession(projectPath string) *agent.SessionInfo {
	data, err := os.ReadFile(sessionCachePath(projectPath))
	if err != nil {
		return nil
	}

	var cached cachedSession
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil
	}

	if cached.SessionID == "" || len(cached.TranscriptData) == 0 {
		return nil
	}
	if time.Since(cached.CachedAt) > agent.RecentSessionTimeout {
		return nil
	}

	return &agent.SessionInfo{
		SessionID:      cached.SessionID,
		StartedAt:      cached.CachedAt.Format(time.RFC3339),
		ProjectPath:    projectPath,
		TranscriptData: cached.TranscriptData,
	}
}
```
