```go
package copilot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/re-cinq/shift-log/internal/agent"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace.yaml.
type sessionMeta struct {
	ID      string `yaml:"id"`
	CWD     string `yaml:"cwd"`
	GitRoot string `yaml:"git_root,omitempty"`
}

// GetCopilotDir returns the path to Copilot's config/data directory.
func GetCopilotDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".copilot"), nil
}

// GetSessionStateDir returns the session state directory.
func GetSessionStateDir() (string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(copilotDir, "session-state"), nil
}

// parseSessionMeta reads a workspace.yaml from a Copilot session directory.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	path := filepath.Join(sessionDir, "workspace.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var meta sessionMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, err
	}

	return &meta, nil
}

// GetTranscriptPath returns the path to the events.jsonl transcript within a session directory.
func GetTranscriptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// WriteSessionFile writes a session directory structure to Copilot's session state directory.
// Creates <sessionDir>/<sessionID>/ with workspace.yaml and events.jsonl.
func WriteSessionFile(sessionID string, data []byte) (string, error) {
	stateDir, err := GetSessionStateDir()
	if err != nil {
		return "", err
	}

	sessionDir := filepath.Join(stateDir, sessionID)
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return "", fmt.Errorf("could not create session directory: %w", err)
	}

	// Write workspace.yaml
	meta := sessionMeta{ID: sessionID}
	yamlData, err := yaml.Marshal(&meta)
	if err != nil {
		return "", fmt.Errorf("could not marshal workspace.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "workspace.yaml"), yamlData, 0600); err != nil {
		return "", err
	}

	// Write events.jsonl
	eventsPath := GetTranscriptPath(sessionDir)
	return eventsPath, os.WriteFile(eventsPath, data, 0600)
}

// sessionCacheFileName is shiftlog's own record of the most recently
// observed Copilot session for a project, written under the project's
// .shiftlog directory (already gitignored).
const sessionCacheFileName = "copilot-session-cache.json"

// cachedSession is the on-disk representation of a live-discovered session,
// captured by CacheSession while the postToolUse hook fires (i.e. while the
// Copilot CLI process is still running). Copilot does not guarantee that its
// own ~/.copilot/session-state directory remains discoverable once a
// non-interactive (`-p`) invocation has exited, so shiftlog keeps its own
// record here to support manual (post-commit hook) session discovery.
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
// ReadCachedSession. It is called from ParseHookInput while the Copilot CLI
// process is still active, so the transcript file is guaranteed to be
// readable at this point even if it disappears once the process exits.
func CacheSession(projectPath, sessionID, transcriptPath string) {
	if projectPath == "" || sessionID == "" || transcriptPath == "" {
		return
	}

	data, err := os.ReadFile(transcriptPath)
	if err != nil || len(data) == 0 {
		return
	}

	cached := cachedSession{
		SessionID:      sessionID,
		TranscriptData: data,
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
