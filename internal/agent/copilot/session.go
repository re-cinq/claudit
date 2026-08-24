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

// sessionCacheFileName is where the most recently discovered live session is
// snapshotted, keyed by project path.
//
// Copilot CLI removes its ~/.copilot/session-state/<id> directory once a
// non-interactive ("-p") session process exits, so scanning session-state after
// the fact (e.g. from the post-commit "shiftlog store --manual" hook, which runs
// seconds after the agent process has already terminated) can find nothing even
// though the session ran successfully. To survive that cleanup, every time
// scanForRecentSession successfully resolves a live session (which happens while
// Copilot's own postToolUse hook is still firing, i.e. before the process exits)
// we snapshot its transcript into .shiftlog/, so manual-mode discovery still has
// something to attribute the commit to.
const sessionCacheFileName = "copilot-session-cache.json"

// cachedSession is the on-disk shape of the session cache.
type cachedSession struct {
	SessionID      string    `json:"session_id"`
	ProjectPath    string    `json:"project_path"`
	TranscriptData []byte    `json:"transcript_data"`
	CachedAt       time.Time `json:"cached_at"`
}

func sessionCachePath(projectPath string) string {
	return filepath.Join(projectPath, ".shiftlog", sessionCacheFileName)
}

// CacheSession snapshots a discovered session's transcript so it survives
// Copilot CLI removing its session-state directory after the process exits.
// Best-effort: failures are silently ignored so they never disrupt the hook.
func CacheSession(projectPath, sessionID, transcriptPath string) {
	if projectPath == "" || sessionID == "" {
		return
	}

	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		return
	}

	encoded, err := json.Marshal(cachedSession{
		SessionID:      sessionID,
		ProjectPath:    projectPath,
		TranscriptData: data,
		CachedAt:       time.Now(),
	})
	if err != nil {
		return
	}

	path := sessionCachePath(projectPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return
	}
	_ = os.WriteFile(path, encoded, 0600)
}

// ReadCachedSession returns a previously cached session for projectPath, if one
// exists and is still within the recent-session window.
func ReadCachedSession(projectPath string) *agent.SessionInfo {
	data, err := os.ReadFile(sessionCachePath(projectPath))
	if err != nil {
		return nil
	}

	var cached cachedSession
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil
	}

	if !agent.PathsEqual(cached.ProjectPath, projectPath) {
		return nil
	}
	if time.Since(cached.CachedAt) > agent.RecentSessionTimeout {
		return nil
	}

	return &agent.SessionInfo{
		SessionID:      cached.SessionID,
		ProjectPath:    projectPath,
		TranscriptData: cached.TranscriptData,
		StartedAt:      cached.CachedAt.Format(time.RFC3339),
	}
}
```
