```go
package copilot

import (
	"crypto/sha256"
	"encoding/hex"
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

// cachedSession is shiftlog's own durable record of a discovered Copilot
// session, keyed by project path. Copilot's on-disk session-state directory
// for a session is not guaranteed to still exist or still be matchable by
// CWD once the CLI process has exited (e.g. after a one-shot `copilot -p`
// run), so shiftlog caches whatever it discovers while the process is still
// live and falls back to this cache for later, out-of-process discovery
// (the `--manual` / post-commit hook flow).
type cachedSession struct {
	SessionID      string    `json:"session_id"`
	TranscriptPath string    `json:"transcript_path"`
	ProjectPath    string    `json:"project_path"`
	CachedAt       time.Time `json:"cached_at"`
}

// cacheFilePath returns the path to shiftlog's cache file for a project.
func cacheFilePath(projectPath string) (string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(projectPath))
	name := hex.EncodeToString(sum[:]) + ".json"
	return filepath.Join(copilotDir, "shiftlog-cache", name), nil
}

// cacheDiscoveredSession records a successful session discovery so it can be
// recovered later even if Copilot's own session-state has since disappeared.
func cacheDiscoveredSession(projectPath, sessionID, transcriptPath string) {
	if sessionID == "" {
		return
	}
	path, err := cacheFilePath(projectPath)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	data, err := json.Marshal(cachedSession{
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
		ProjectPath:    projectPath,
		CachedAt:       time.Now(),
	})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0600)
}

// loadCachedSession returns a previously cached session discovery for a
// project, if one exists and is still within the recent-session window.
func loadCachedSession(projectPath string) *agent.SessionInfo {
	path, err := cacheFilePath(projectPath)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cs cachedSession
	if err := json.Unmarshal(data, &cs); err != nil {
		return nil
	}
	if time.Since(cs.CachedAt) > agent.RecentSessionTimeout {
		return nil
	}
	return &agent.SessionInfo{
		SessionID:      cs.SessionID,
		TranscriptPath: cs.TranscriptPath,
		StartedAt:      cs.CachedAt.Format(time.RFC3339),
		ProjectPath:    projectPath,
	}
}
```
